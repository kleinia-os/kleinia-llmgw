// Package router selects the upstream source that serves a model alias.
//
// Selection layers four mechanisms, applied in order:
//
//  1. Session stickiness: a session whose last success was on a source keeps
//     that source (1h TTL, refreshed on every hit) while it stays a healthy
//     candidate for the alias, so multi-turn conversations keep hitting a
//     warm prompt cache.
//  2. Priority tiers: candidates are grouped by priority, highest first.
//     A tier whose members are all in cooldown is skipped and the next tier
//     serves, so a cooling or disabled high-priority source fails over
//     instead of blocking.
//  3. Weighted random within the chosen tier: effective weight is
//     Weight+10 (new-api semantics), so weight 0 still draws ~1/3 of a
//     weight-20 source's traffic rather than none.
//  4. Last resort: if every candidate is cooling down, the source that has
//     been cooling longest (i.e. cools out soonest) is returned anyway —
//     degraded service beats a hard failure.
//
// A source enters a 60s cooldown after 3 consecutive failures and recovers
// automatically when the cooldown expires (the failure streak resets with
// it, so re-entry needs 3 fresh failures).
package router

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// Tunables. The time-based ones are fields on Router so tests can shrink
// them; these are the production defaults.
const (
	stickyTTL     = time.Hour
	stickyCap     = 4096
	failThreshold = 3
	cooldown      = 60 * time.Second
	baseWeight    = 10
)

// ErrUnknownModel is returned (wrapped, with the alias) when no enabled
// source serves the requested alias.
var ErrUnknownModel = errors.New("router: no source serves model alias")

// Source is the router's view of one upstream: the stored source record
// plus its resolved alias→upstream model mappings.
type Source struct {
	ID       int64
	Name     string
	Type     string
	BaseURL  string
	APIKey   string
	Priority int
	Weight   int
	Enabled  bool
	Models   map[string]string // alias -> upstream model
}

// HealthInfo reports one source's failure and cooldown state. A zero
// CoolingUntil means healthy.
type HealthInfo struct {
	ID               int64
	Name             string
	CoolingUntil     time.Time
	ConsecutiveFails int
}

type healthState struct {
	consecutiveFails int
	coolingUntil     time.Time // zero = healthy
}

type stickyEntry struct {
	sourceID int64
	expires  time.Time
}

// Router is a concurrency-safe source picker. Update swaps the source view
// atomically; health and stickiness state survive the swap.
type Router struct {
	mu        sync.Mutex
	rnd       *rand.Rand
	sources   []Source
	health    map[int64]*healthState
	sticky    map[string]stickyEntry
	stickyTTL time.Duration
	cooldown  time.Duration
}

// New returns a Router over sources, seeded from the wall clock.
func New(sources []Source) *Router {
	return NewWithRand(rand.NewSource(time.Now().UnixNano()), sources)
}

// NewWithRand returns a Router that draws weighted picks from src, so tests
// get deterministic selections under a fixed seed (math/rand's rand.Source
// is the standard injection point for this).
func NewWithRand(src rand.Source, sources []Source) *Router {
	return &Router{
		rnd:       rand.New(src),
		sources:   cloneSources(sources),
		health:    make(map[int64]*healthState),
		sticky:    make(map[string]stickyEntry),
		stickyTTL: stickyTTL,
		cooldown:  cooldown,
	}
}

// Update atomically replaces the source view. Health and stickiness state
// are kept: a source that reappears with the same ID carries its history.
func (r *Router) Update(sources []Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources = cloneSources(sources)
}

// Sources returns a deep copy snapshot of the current view (including
// Models), safe for callers to mutate.
func (r *Router) Sources() []Source {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneSources(r.sources)
}

// Pick selects a source for alias (see the package comment for semantics).
// The returned Source shares its Models map with the router's view; callers
// must treat it as read-only.
func (r *Router) Pick(alias, session string) (*Source, error) {
	return r.PickExcluding(alias, session, nil)
}

// PickExcluding is Pick with retries in mind: candidates whose ID is true
// in exclude are skipped, so the server can walk down the pool after a
// failed attempt. Excluding every candidate yields the same
// ErrUnknownModel sentinel as having none.
func (r *Router) PickExcluding(alias, session string, exclude map[int64]bool) (*Source, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()

	var cands []Source
	for _, s := range r.sources {
		if !s.Enabled || exclude[s.ID] {
			continue
		}
		if _, ok := s.Models[alias]; !ok {
			continue
		}
		r.refreshHealth(s.ID, now)
		cands = append(cands, s)
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("%w %q", ErrUnknownModel, alias)
	}

	// 1) Sticky session: honored only while the pinned source is still a
	// candidate for this alias and healthy.
	if session != "" {
		if e, ok := r.sticky[session]; ok {
			switch {
			case now.After(e.expires):
				delete(r.sticky, session)
			default:
				if src := findSource(cands, e.sourceID); src != nil && !r.isCooling(src.ID, now) {
					e.expires = now.Add(r.stickyTTL) // refresh on hit
					r.sticky[session] = e
					cp := *src
					return &cp, nil
				}
			}
		}
	}

	// 2) Priority tiers, highest first; fully-cooling tiers are skipped.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Priority > cands[j].Priority })
	for i := 0; i < len(cands); {
		j := i + 1
		for j < len(cands) && cands[j].Priority == cands[i].Priority {
			j++
		}
		var healthy []Source
		for _, s := range cands[i:j] {
			if !r.isCooling(s.ID, now) {
				healthy = append(healthy, s)
			}
		}
		if len(healthy) > 0 {
			// 3) Weighted random within the tier.
			pick := r.weightedPick(healthy)
			return &pick, nil
		}
		i = j
	}

	// 4) Everything is cooling: keep serving from the source that cools out
	// soonest (has been cooling the longest).
	best := cands[0]
	for _, s := range cands[1:] {
		bh, sh := r.health[best.ID], r.health[s.ID]
		if sh != nil && (bh == nil || sh.coolingUntil.Before(bh.coolingUntil)) {
			best = s
		}
	}
	return &best, nil
}

// ReportSuccess clears the source's failure streak and, when session is
// non-empty, pins that session to the source for another TTL.
func (r *Router) ReportSuccess(sourceID int64, session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.refreshHealth(sourceID, now)
	if h := r.health[sourceID]; h != nil {
		h.consecutiveFails = 0
	}
	if session == "" {
		return
	}
	if len(r.sticky) >= stickyCap {
		// Drop expired entries first; only a still-full table is cleared.
		for k, e := range r.sticky {
			if now.After(e.expires) {
				delete(r.sticky, k)
			}
		}
		if len(r.sticky) >= stickyCap {
			r.sticky = make(map[string]stickyEntry)
		}
	}
	r.sticky[session] = stickyEntry{sourceID: sourceID, expires: now.Add(r.stickyTTL)}
}

// ReportFailure records one failure. failThreshold consecutive failures put
// the source into a cooldown that expires on its own.
func (r *Router) ReportFailure(sourceID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.refreshHealth(sourceID, now)
	h := r.health[sourceID]
	if h == nil {
		h = &healthState{}
		r.health[sourceID] = h
	}
	h.consecutiveFails++
	if h.consecutiveFails >= failThreshold {
		h.coolingUntil = now.Add(r.cooldown)
	}
}

// Health reports failure/cooldown state for every source in the current
// view, in view order.
func (r *Router) Health() []HealthInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	out := make([]HealthInfo, 0, len(r.sources))
	for _, s := range r.sources {
		r.refreshHealth(s.ID, now)
		info := HealthInfo{ID: s.ID, Name: s.Name}
		if h := r.health[s.ID]; h != nil {
			info.CoolingUntil = h.coolingUntil
			info.ConsecutiveFails = h.consecutiveFails
		}
		out = append(out, info)
	}
	return out
}

// weightedPick draws one source with probability eff/Σeff where
// eff = Weight+baseWeight (clamped at 0). A tier with total eff 0 falls
// back to uniform choice.
func (r *Router) weightedPick(tier []Source) Source {
	eff := make([]int, len(tier))
	total := 0
	for i, s := range tier {
		e := s.Weight + baseWeight
		if e < 0 {
			e = 0
		}
		eff[i] = e
		total += e
	}
	if total == 0 {
		return tier[r.rnd.Int63n(int64(len(tier)))]
	}
	draw := r.rnd.Int63n(int64(total))
	acc := 0
	for i, e := range eff {
		acc += e
		if draw < int64(acc) {
			return tier[i]
		}
	}
	return tier[len(tier)-1] // unreachable: draw < total by construction
}

// refreshHealth clears an expired cooldown together with its failure streak,
// so a recovered source needs failThreshold fresh failures to cool again.
func (r *Router) refreshHealth(id int64, now time.Time) {
	h := r.health[id]
	if h == nil || h.coolingUntil.IsZero() {
		return
	}
	if !now.Before(h.coolingUntil) {
		h.coolingUntil = time.Time{}
		h.consecutiveFails = 0
	}
}

func (r *Router) isCooling(id int64, now time.Time) bool {
	h := r.health[id]
	return h != nil && !h.coolingUntil.IsZero() && now.Before(h.coolingUntil)
}

func findSource(cands []Source, id int64) *Source {
	for i := range cands {
		if cands[i].ID == id {
			return &cands[i]
		}
	}
	return nil
}

// cloneSources deep-copies the slice and every Models map so router state
// is isolated from caller mutations.
func cloneSources(in []Source) []Source {
	out := make([]Source, len(in))
	for i, s := range in {
		m := make(map[string]string, len(s.Models))
		for k, v := range s.Models {
			m[k] = v
		}
		s.Models = m
		out[i] = s
	}
	return out
}
