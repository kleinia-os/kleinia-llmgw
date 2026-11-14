package router

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// mksrc builds a router source serving the aliases a1, a2, ...
func mksrc(id int64, name string, priority, weight int, aliases ...string) Source {
	m := make(map[string]string, len(aliases))
	for _, a := range aliases {
		m[a] = name + "/" + a
	}
	return Source{ID: id, Name: name, Type: "openai", BaseURL: "https://" + name + "/v1",
		Priority: priority, Weight: weight, Enabled: true, Models: m}
}

func TestPickUnknownModel(t *testing.T) {
	r := New([]Source{mksrc(1, "a", 0, 0, "model-a")})
	_, err := r.Pick("no-model", "")
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v; want ErrUnknownModel", err)
	}
	if got := err.Error(); !strings.Contains(got, "no-model") {
		t.Errorf("error %q does not mention the alias", got)
	}
	// An enabled-but-disqualified pool is equally empty.
	r2 := New([]Source{mksrc(1, "a", 0, 0, "model-a")})
	r2.Update([]Source{}) // all sources gone
	if _, err := r2.Pick("model-a", ""); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("err after Update = %v; want ErrUnknownModel", err)
	}
}

func TestWeightedDistribution(t *testing.T) {
	// Same tier, weights 20/10/0 => effective 30/20/10 => 3:2:1.
	r := NewWithRand(rand.NewSource(1), []Source{
		mksrc(1, "heavy", 0, 20, "model-a"),
		mksrc(2, "mid", 0, 10, "model-a"),
		mksrc(3, "light", 0, 0, "model-a"),
	})
	const n = 3000
	counts := map[int64]int{}
	for i := 0; i < n; i++ {
		src, err := r.Pick("model-a", "")
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		counts[src.ID]++
	}
	if counts[1]+counts[2]+counts[3] != n {
		t.Fatalf("counts sum = %d; want %d", counts[1]+counts[2]+counts[3], n)
	}
	for id, want := range map[int64]float64{1: 0.5, 2: 1.0 / 3.0, 3: 1.0 / 6.0} {
		got := float64(counts[id]) / n
		if diff := got - want; diff < -0.05 || diff > 0.05 {
			t.Errorf("source %d share = %.3f; want %.3f±0.05 (counts %+v)", id, got, want, counts)
		}
	}
}

func TestPriorityTiers(t *testing.T) {
	hi, lo := mksrc(1, "hi", 10, 0, "model-a"), mksrc(2, "lo", 0, 100, "model-a")
	r := NewWithRand(rand.NewSource(2), []Source{hi, lo})
	for i := 0; i < 50; i++ {
		src, err := r.Pick("model-a", "")
		if err != nil || src.ID != 1 {
			t.Fatalf("pick %d = %+v err=%v; want source 1 (higher tier)", i, src, err)
		}
	}

	// High tier disabled via Update -> low tier serves.
	hi.Enabled = false
	r.Update([]Source{hi, lo})
	for i := 0; i < 20; i++ {
		src, err := r.Pick("model-a", "")
		if err != nil || src.ID != 2 {
			t.Fatalf("pick after disable = %+v err=%v; want source 2", src, err)
		}
	}
}

func TestCooldownFallsToLowTierAndRecovers(t *testing.T) {
	hi, lo := mksrc(1, "hi", 10, 0, "model-a"), mksrc(2, "lo", 0, 0, "model-a")
	r := NewWithRand(rand.NewSource(3), []Source{hi, lo})

	// Two failures: streak recorded, still serving.
	r.ReportFailure(1)
	r.ReportFailure(1)
	h := healthOf(t, r, 1)
	if h.ConsecutiveFails != 2 || !h.CoolingUntil.IsZero() {
		t.Fatalf("after 2 fails = %+v; want streak 2, no cooldown", h)
	}

	// Arm a short cooldown so expiry is testable without sleeping 60s.
	r.mu.Lock()
	r.cooldown = 200 * time.Millisecond
	r.mu.Unlock()
	r.ReportFailure(1) // third: cooldown
	h = healthOf(t, r, 1)
	if h.CoolingUntil.IsZero() || time.Until(h.CoolingUntil) <= 0 {
		t.Fatalf("after 3 fails = %+v; want active cooldown", h)
	}
	for i := 0; i < 20; i++ {
		src, err := r.Pick("model-a", "")
		if err != nil || src.ID != 2 {
			t.Fatalf("pick during cooldown = %+v err=%v; want source 2", src, err)
		}
	}

	// ReportSuccess on the low tier does not disturb the high tier's state;
	// the cooldown lapsing does (streak resets with it).
	time.Sleep(300 * time.Millisecond)
	h = healthOf(t, r, 1)
	if !h.CoolingUntil.IsZero() || h.ConsecutiveFails != 0 {
		t.Fatalf("after expiry = %+v; want cooldown cleared and streak reset", h)
	}
	src, err := r.Pick("model-a", "")
	if err != nil || src.ID != 1 {
		t.Fatalf("pick after recovery = %+v err=%v; want source 1 back on top", src, err)
	}
}

func TestReportSuccessResetsStreak(t *testing.T) {
	r := NewWithRand(rand.NewSource(4), []Source{mksrc(1, "a", 0, 0, "model-a")})
	r.ReportFailure(1)
	r.ReportFailure(1)
	r.ReportSuccess(1, "")
	if h := healthOf(t, r, 1); h.ConsecutiveFails != 0 {
		t.Fatalf("after success = %+v; want streak 0", h)
	}
}

func TestAllCoolingUsesLongestCooling(t *testing.T) {
	hi, lo := mksrc(1, "hi", 10, 0, "model-a"), mksrc(2, "lo", 0, 0, "model-a")
	r := NewWithRand(rand.NewSource(5), []Source{hi, lo})
	for i := 0; i < 3; i++ {
		r.ReportFailure(1)
	}
	time.Sleep(10 * time.Millisecond) // hi cools out strictly before lo
	for i := 0; i < 3; i++ {
		r.ReportFailure(2)
	}
	for i := 0; i < 20; i++ {
		src, err := r.Pick("model-a", "")
		if err != nil || src.ID != 1 {
			t.Fatalf("all-cooling pick = %+v err=%v; want source 1 (cooling longest)", src, err)
		}
	}
}

func TestStickySession(t *testing.T) {
	r := NewWithRand(rand.NewSource(6), []Source{
		mksrc(1, "a", 0, 0, "model-a"),
		mksrc(2, "b", 0, 0, "model-a"),
	})
	first, err := r.Pick("model-a", "sess-1")
	if err != nil {
		t.Fatalf("first Pick: %v", err)
	}
	r.ReportSuccess(first.ID, "sess-1")
	for i := 0; i < 20; i++ {
		src, err := r.Pick("model-a", "sess-1")
		if err != nil || src.ID != first.ID {
			t.Fatalf("sticky pick %d = %+v err=%v; want source %d", i, src, err, first.ID)
		}
	}
	// Another session is not pinned to the same source by that entry (it
	// just gets ordinary weighted picks).
	if _, err := r.Pick("model-a", "sess-2"); err != nil {
		t.Fatalf("Pick other session: %v", err)
	}
}

func TestStickyIgnoredWhenCandidateGoneOrCooling(t *testing.T) {
	a, b := mksrc(1, "a", 10, 0, "model-a"), mksrc(2, "b", 0, 0, "model-a")
	r := NewWithRand(rand.NewSource(7), []Source{a, b})

	// Pin to the low-priority source; stickiness must beat priority.
	r.ReportSuccess(2, "s")
	if src, _ := r.Pick("model-a", "s"); src.ID != 2 {
		t.Fatalf("sticky pick = %d; want 2", src.ID)
	}

	// Pinned source cooling -> fall back to the healthy tier.
	r.ReportFailure(2)
	r.ReportFailure(2)
	r.ReportFailure(2)
	if src, _ := r.Pick("model-a", "s"); src.ID != 1 {
		t.Fatalf("sticky-but-cooling pick = %d; want 1", src.ID)
	}

	// Pinned source removed from the view -> fall back as well.
	r.Update([]Source{a})
	if src, _ := r.Pick("model-a", "s"); src.ID != 1 {
		t.Fatalf("sticky-candidate-gone pick = %d; want 1", src.ID)
	}
}

func TestStickyTTLExpiry(t *testing.T) {
	a, b := mksrc(1, "a", 10, 0, "model-a"), mksrc(2, "b", 0, 0, "model-a")
	r := NewWithRand(rand.NewSource(8), []Source{a, b})
	r.mu.Lock()
	r.stickyTTL = 20 * time.Millisecond
	r.mu.Unlock()
	r.ReportSuccess(2, "s") // pin to the low-priority source
	if src, _ := r.Pick("model-a", "s"); src.ID != 2 {
		t.Fatalf("pick before TTL expiry = %d; want sticky 2", src.ID)
	}
	time.Sleep(50 * time.Millisecond)
	if src, _ := r.Pick("model-a", "s"); src.ID != 1 {
		t.Fatalf("pick after TTL expiry = %d; want priority pick 1", src.ID)
	}
}

func TestPickExcluding(t *testing.T) {
	r := NewWithRand(rand.NewSource(9), []Source{
		mksrc(1, "hi", 10, 0, "model-a"),
		mksrc(2, "lo", 0, 0, "model-a"),
	})
	src, err := r.PickExcluding("model-a", "", map[int64]bool{1: true})
	if err != nil || src.ID != 2 {
		t.Fatalf("PickExcluding(hi) = %+v err=%v; want source 2", src, err)
	}
	if src, err := r.PickExcluding("model-a", "", map[int64]bool{999: true}); err != nil || src.ID != 1 {
		t.Fatalf("PickExcluding(unknown) = %+v err=%v; want source 1 unaffected", src, err)
	}
	if _, err := r.PickExcluding("model-a", "", map[int64]bool{1: true, 2: true}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("PickExcluding(all) err = %v; want ErrUnknownModel", err)
	}
	// Pick itself delegates with no exclusions.
	if src, err := r.Pick("model-a", ""); err != nil || src.ID != 1 {
		t.Fatalf("Pick = %+v err=%v; want source 1", src, err)
	}
}

func TestSourcesSnapshot(t *testing.T) {
	in := []Source{mksrc(1, "a", 5, 3, "model-a", "model-b")}
	r := NewWithRand(rand.NewSource(10), in)

	got := r.Sources()
	if len(got) != 1 || got[0].Models["model-a"] != "a/model-a" || got[0].Priority != 5 {
		t.Fatalf("Sources() = %+v", got)
	}
	// Snapshot must be deep: mutating it cannot corrupt the router.
	got[0].Models["model-a"] = "TAMPERED"
	got[0].Priority = 99
	if again := r.Sources(); again[0].Models["model-a"] != "a/model-a" || again[0].Priority != 5 {
		t.Fatalf("router view mutated through snapshot: %+v", again[0])
	}
	// Update is reflected; health/sticky survive (checked elsewhere too).
	r.Update([]Source{mksrc(1, "a", 6, 3, "model-a")})
	if v := r.Sources(); len(v) != 1 || v[0].Priority != 6 || len(v[0].Models) != 1 {
		t.Fatalf("after Update Sources() = %+v", v)
	}
}

func TestUpdateKeepsHealthAndSticky(t *testing.T) {
	a, b := mksrc(1, "a", 10, 0, "model-a"), mksrc(2, "b", 0, 0, "model-a")
	r := NewWithRand(rand.NewSource(11), []Source{a, b})
	r.ReportSuccess(1, "s")
	r.ReportFailure(2)
	r.ReportFailure(2)
	r.ReportFailure(2)
	r.Update([]Source{a, b}) // same view, fresh copies

	if h := healthOf(t, r, 2); h.CoolingUntil.IsZero() {
		t.Fatal("cooldown lost across Update")
	}
	if src, err := r.Pick("model-a", "s"); err != nil || src.ID != 1 {
		t.Fatalf("sticky lost across Update: %+v err=%v", src, err)
	}
}

func healthOf(t *testing.T, r *Router, id int64) HealthInfo {
	t.Helper()
	for _, h := range r.Health() {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("source %d missing from Health()", id)
	return HealthInfo{}
}
