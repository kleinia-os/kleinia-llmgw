package server

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"kleinia.io/llmgw/internal/ir"
)

// ModelPrice is one alias's price table: USD per million tokens, one
// optional component each. A nil component is "not enabled" — models
// without e.g. cache writes simply leave that field unset.
type ModelPrice struct {
	InputPerMTok      *float64 `json:"input_per_mtok,omitempty"`
	OutputPerMTok     *float64 `json:"output_per_mtok,omitempty"`
	CacheReadPerMTok  *float64 `json:"cache_read_per_mtok,omitempty"`
	CacheWritePerMTok *float64 `json:"cache_write_per_mtok,omitempty"`
}

// PricedModel is one catalog entry of List().
type PricedModel struct {
	Alias string `json:"alias"`
	ModelPrice
}

// Pricer maps model aliases to USD pricing (per million tokens) for cost
// estimation. Unknown models yield nil cost — we never guess a price.
//
// Billing base (canonical IR semantics): CacheReadTokens is a carve-out
// of InputTokens; CacheWriteTokens is an additive dimension:
//
//	input billed   = (InputTokens − CacheReadTokens) × input  [when input enabled]
//	cache read     = CacheReadTokens × cache_read             [when enabled;
//	                 otherwise the tokens stay inside the input term above]
//	output         = OutputTokens × output                    [when enabled]
//	cache write    = CacheWriteTokens × cache_write           [when enabled]
//
// Mutations (Set/Remove) apply immediately to the running gateway and are
// persisted back to the prices.json path given at load time (atomic
// tmp+rename); a Pricer without a path is in-memory only.
type Pricer struct {
	path string
	mu   sync.RWMutex
	m    map[string]ModelPrice
}

// LoadPrices reads prices.json; a missing file yields an empty (all-unknown)
// pricer, which is a valid state.
func LoadPrices(path string) *Pricer {
	p := &Pricer{path: path, m: map[string]ModelPrice{}}
	if path == "" {
		return p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	var file struct {
		Models map[string]ModelPrice `json:"models"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return p
	}
	for k, v := range file.Models {
		p.m[k] = v
	}
	return p
}

func perMTok(tokens int64, price float64) float64 {
	return float64(tokens) * price / 1e6
}

// Cost estimates the USD cost of a call; nil when pricing is unknown.
func (p *Pricer) Cost(alias string, u ir.Usage) *float64 {
	p.mu.RLock()
	mp, ok := p.m[alias]
	p.mu.RUnlock()
	if !ok {
		return nil
	}
	// 前提：启用缓存读价时才从输入里切出；否则缓存读按输入价计。
	billedInput := u.InputTokens
	if mp.CacheReadPerMTok != nil {
		billedInput -= u.CacheReadTokens
	}
	c := 0.0
	if mp.InputPerMTok != nil {
		c += perMTok(billedInput, *mp.InputPerMTok)
	}
	if mp.CacheReadPerMTok != nil {
		c += perMTok(u.CacheReadTokens, *mp.CacheReadPerMTok)
	}
	if mp.OutputPerMTok != nil {
		c += perMTok(u.OutputTokens, *mp.OutputPerMTok)
	}
	if mp.CacheWritePerMTok != nil {
		c += perMTok(u.CacheWriteTokens, *mp.CacheWritePerMTok)
	}
	return &c
}

// List returns all priced aliases sorted by name.
func (p *Pricer) List() []PricedModel {
	p.mu.RLock()
	out := make([]PricedModel, 0, len(p.m))
	for alias, mp := range p.m {
		out = append(out, PricedModel{Alias: alias, ModelPrice: mp})
	}
	p.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

func nonNeg(v float64) bool { return v >= 0 }

// Set prices an alias (USD per million tokens; 0 is a valid free price).
// At least one component must be enabled; use Remove to unprice.
// The update is live immediately and persisted when a path is configured.
func (p *Pricer) Set(alias string, mp ModelPrice) error {
	if alias == "" {
		return fmt.Errorf("prices: empty alias")
	}
	for name, v := range map[string]*float64{
		"input": mp.InputPerMTok, "output": mp.OutputPerMTok,
		"cache_read": mp.CacheReadPerMTok, "cache_write": mp.CacheWritePerMTok,
	} {
		if v != nil && !nonNeg(*v) {
			return fmt.Errorf("prices: negative %s value for %q", name, alias)
		}
	}
	if mp.InputPerMTok == nil && mp.OutputPerMTok == nil &&
		mp.CacheReadPerMTok == nil && mp.CacheWritePerMTok == nil {
		return fmt.Errorf("prices: %q has no enabled component", alias)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[alias] = mp
	return p.saveLocked()
}

// Remove clears an alias's pricing (cost goes back to unknown).
func (p *Pricer) Remove(alias string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, alias)
	return p.saveLocked()
}

// saveLocked writes the table atomically (caller holds mu).
func (p *Pricer) saveLocked() error {
	if p.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(struct {
		Models map[string]ModelPrice `json:"models"`
	}{p.m}, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}
