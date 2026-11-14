package store

import (
	"strconv"
	"testing"
	"time"
)

func TestInsertAndRecentUsage(t *testing.T) {
	s := mustOpen(t)
	cost := 0.0021
	rows := []UsageRow{
		{TS: "2026-10-28T08:00:01Z", KeyID: 1, KeyName: "opencode", SourceID: 2, SourceName: "primary",
			ClientFormat: "chat", ModelAlias: "model-a", UpstreamModel: "model-a",
			In: 100, Out: 50, CacheRead: 40, LatencyMS: 1200, TTFTMS: 300, Stream: true, Status: 200,
			Session: "s1"},
		{TS: "2026-10-28T08:00:02.5Z", KeyID: 1, ModelAlias: "model-b", In: 10, Out: 5,
			Cost: &cost, Status: 200},
		{TS: "2026-10-28T08:00:03.123456789Z", SourceID: 3, SourceName: "ant",
			ClientFormat: "anthropic", Status: 408, ErrorKind: "network", Degraded: true},
	}
	for _, u := range rows {
		if err := s.InsertUsage(u); err != nil {
			t.Fatalf("InsertUsage(%+v): %v", u, err)
		}
	}

	got, err := s.RecentUsage(0)
	if err != nil {
		t.Fatalf("RecentUsage(0): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("RecentUsage(0) returned %d rows; want 3", len(got))
	}
	// Newest first even without a limit.
	if got[0].TS != rows[2].TS || got[2].TS != rows[0].TS {
		t.Errorf("order = %q..%q; want newest first", got[0].TS, got[2].TS)
	}
	last := got[0]
	if last.KeyID != 0 || last.KeyName != "" || last.Status != 408 || !last.Degraded ||
		last.ErrorKind != "network" || last.ClientFormat != "anthropic" || last.Cost != nil {
		t.Errorf("third row round-trip mismatch: %+v", last)
	}
	second := got[1]
	if second.Cost == nil || *second.Cost != cost {
		t.Errorf("cost round-trip = %v; want %v", second.Cost, cost)
	}
	if second.CacheRead != 0 || second.Stream {
		t.Errorf("zero-default fields drifted: %+v", second)
	}
	first := got[2]
	if first.In != 100 || first.Out != 50 || first.CacheRead != 40 || !first.Stream ||
		first.LatencyMS != 1200 || first.TTFTMS != 300 || first.Status != 200 || first.Session != "s1" {
		t.Errorf("first row round-trip mismatch: %+v", first)
	}

	top, err := s.RecentUsage(2)
	if err != nil || len(top) != 2 || top[0].TS != rows[2].TS || top[1].TS != rows[1].TS {
		t.Errorf("RecentUsage(2) = %d rows err=%v; want the two newest", len(top), err)
	}
}

func TestKeyWindows(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	type fixture struct {
		ts      time.Time
		in, out int64
	}
	fixtures := []fixture{
		{now, 100, 50},                             // in every window
		{now.Add(-20 * time.Second), 10, 10},       // in 60s window
		{now.Add(-90 * time.Second), 1000, 1000},   // out of 60s window
		{now.Add(-2 * time.Hour), 500, 500},        // in daily window unless it crossed midnight
		{dayStart.Add(-90 * time.Second), 7000, 0}, // yesterday: in no window
	}
	// Spec-level oracle: which rows belong to which window.
	var wantRPM, wantTPM, wantDaily int64
	for _, f := range fixtures {
		if f.ts.After(now.Add(-time.Minute)) {
			wantRPM++
			wantTPM += f.in + f.out
		}
		if !f.ts.Before(dayStart) {
			wantDaily += f.in + f.out
		}
		if err := s.InsertUsage(UsageRow{TS: f.ts.Format(time.RFC3339Nano), KeyID: 7,
			ModelAlias: "model-a", In: f.in, Out: f.out}); err != nil {
			t.Fatalf("InsertUsage: %v", err)
		}
	}
	// Another key's traffic must not leak into key 7's windows.
	if err := s.InsertUsage(UsageRow{TS: now.Format(time.RFC3339Nano), KeyID: 8, In: 999, Out: 999}); err != nil {
		t.Fatalf("InsertUsage: %v", err)
	}
	// A pre-midnight row exercises sub-second RFC3339Nano parsing in
	// datetime(); fixture 5 also covers the yesterday boundary.

	rpm, tpm, daily, err := s.KeyWindows(7)
	if err != nil {
		t.Fatalf("KeyWindows: %v", err)
	}
	if rpm != wantRPM {
		t.Errorf("rpm = %d; want %d", rpm, wantRPM)
	}
	if tpm != wantTPM {
		t.Errorf("tpm = %d; want %d", tpm, wantTPM)
	}
	if daily != wantDaily {
		t.Errorf("dailyTokens = %d; want %d", daily, wantDaily)
	}

	// A key with no traffic gets zeroed windows, not an error.
	rpm, tpm, daily, err = s.KeyWindows(4242)
	if err != nil || rpm != 0 || tpm != 0 || daily != 0 {
		t.Errorf("empty windows = %d/%d/%d err=%v; want zeros", rpm, tpm, daily, err)
	}
}

func TestSummarySince(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	fresh := now.Format(time.RFC3339Nano)

	insert := func(ts, key, model, source string, in, out int64, cost *float64) {
		t.Helper()
		if err := s.InsertUsage(UsageRow{TS: ts, KeyName: key, ModelAlias: model, SourceName: source,
			In: in, Out: out, Cost: cost}); err != nil {
			t.Fatalf("InsertUsage: %v", err)
		}
	}
	c1, c2 := 0.10, 0.20
	insert(old, "opencode", "model-a", "primary", 1, 1, nil) // before the window
	insert(fresh, "opencode", "model-a", "primary", 100, 50, &c1)
	insert(fresh, "opencode", "model-b", "primary", 10, 5, nil)
	insert(fresh, "cline", "model-a", "ant", 7, 3, &c2)

	since := now.Add(-time.Hour).Format(time.RFC3339)
	byKey, byModel, bySource, byAid, err := s.SummarySince(since)
	if err != nil {
		t.Fatalf("SummarySince: %v", err)
	}

	if len(byKey) != 2 {
		t.Fatalf("byKey = %+v; want 2 groups", byKey)
	}
	opencode := byKey[0] // most requests first
	if opencode.Label != "opencode" || opencode.Requests != 2 ||
		opencode.InTokens != 110 || opencode.OutTokens != 55 ||
		opencode.CostEst == nil || *opencode.CostEst != c1 {
		t.Errorf("byKey[opencode] = %+v; want 2 req, 110/55 tokens, cost %.2f", opencode, c1)
	}
	cline := byKey[1]
	if cline.Label != "cline" || cline.Requests != 1 || cline.CostEst == nil || *cline.CostEst != c2 {
		t.Errorf("byKey[cline] = %+v", cline)
	}

	if len(byModel) != 2 || byModel[0].Label != "model-a" || byModel[0].Requests != 2 ||
		byModel[1].Label != "model-b" || byModel[1].Requests != 1 {
		t.Errorf("byModel = %+v; want model-a(2) then model-b(1)", byModel)
	}
	if byModel[1].CostEst != nil {
		t.Errorf("byModel[model-b].CostEst = %v; want nil (no priced rows in group)", *byModel[1].CostEst)
	}
	if len(bySource) != 2 || bySource[0].Label != "primary" || bySource[0].Requests != 2 ||
		bySource[0].InTokens != 110 || bySource[0].OutTokens != 55 ||
		bySource[1].Label != "ant" || bySource[1].Requests != 1 {
		t.Errorf("bySource = %+v; want primary(2) then ant(1)", bySource)
	}
	// All rows have empty AID: one group under "".
	if len(byAid) != 1 || byAid[0].Requests != 3 {
		t.Errorf("byAid = %+v; want one empty-label group of 3", byAid)
	}

	// A window that excludes everything yields empty aggregates, and a
	// group with only unpriced rows has a nil CostEst.
	future := now.Add(time.Hour).Format(time.RFC3339)
	byKey, byModel, bySource, byAid, err = s.SummarySince(future)
	if err != nil {
		t.Fatalf("SummarySince(future): %v", err)
	}
	if len(byKey) != 0 || len(byModel) != 0 || len(bySource) != 0 || len(byAid) != 0 {
		t.Errorf("future window = %+v/%+v/%+v/%+v; want all empty", byKey, byModel, bySource, byAid)
	}
}

func TestHistogramAndStats(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()
	insert := func(offset time.Duration, key, model string, in, out, latency int64, status int) {
		t.Helper()
		ts := now.Add(offset).Format(time.RFC3339Nano)
		if err := s.InsertUsage(UsageRow{TS: ts, KeyName: key, ModelAlias: model,
			In: in, Out: out, LatencyMS: latency, Status: status}); err != nil {
			t.Fatalf("InsertUsage: %v", err)
		}
	}
	insert(-30*time.Minute, "opencode", "model-a", 100, 50, 800, 200)
	insert(-30*time.Minute, "opencode", "model-a", 20, 10, 1000, 200)
	insert(-3*time.Hour, "dsh", "model-a", 5, 5, 500, 429)

	since := now.Add(-24 * time.Hour).Format(time.RFC3339)

	hh, err := s.Histogram(since, "hour", "")
	if err != nil {
		t.Fatalf("Histogram hour totals: %v", err)
	}
	if len(hh) != 2 {
		t.Fatalf("hour buckets = %+v; want 2", hh)
	}
	if hh[0].Requests+hh[1].Requests != 3 || hh[0].In+hh[1].In != 125 {
		t.Fatalf("hour totals wrong: %+v", hh)
	}

	hm, err := s.Histogram(since, "day", "model_alias")
	if err != nil {
		t.Fatalf("Histogram day by model: %v", err)
	}
	if len(hm) != 1 || hm[0].Group != "model-a" || hm[0].Requests != 3 {
		t.Fatalf("day-by-model = %+v", hm)
	}

	hk, err := s.Histogram(since, "hour", "key_name")
	if err != nil {
		t.Fatalf("Histogram hour by key: %v", err)
	}
	groups := map[string]int64{}
	for _, b := range hk {
		groups[b.Group] += b.Requests
	}
	if groups["opencode"] != 2 || groups["dsh"] != 1 {
		t.Fatalf("by-key buckets = %+v", hk)
	}
	if _, err := s.Histogram(since, "week", ""); err == nil {
		t.Error("week bucket accepted")
	}
	if _, err := s.Histogram(since, "hour", "evil; DROP TABLE usage"); err == nil {
		t.Error("evil dimension accepted")
	}

	st, err := s.Stats(since)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Requests != 3 || st.In != 125 || st.Out != 65 {
		t.Fatalf("stats totals = %+v", st)
	}
	// nearest-rank p50 of {500,800,1000} = 800, p95 = 1000
	if st.LatencyP50MS != 800 || st.LatencyP95MS != 1000 {
		t.Fatalf("latency p50/p95 = %d/%d; want 800/1000", st.LatencyP50MS, st.LatencyP95MS)
	}
	if got := st.ErrorRate; got < 0.33 || got > 0.34 {
		t.Fatalf("error rate = %v; want ~1/3", got)
	}
	if st.ActiveIds != 2 {
		t.Fatalf("active ids = %d; want 2", st.ActiveIds)
	}
}

func TestUsagePageFilterPaging(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()
	base := now.Add(-10 * time.Minute)

	// 10 行：opencode×4(model-a)、cline×3(model-b)、agentfs×3(aid 计费行)，
	// 其中 model-b 的第 2 行是错误行（429 + error_kind）。
	ins := func(i int, key, model, source, aid string, status int, ekind string) {
		t.Helper()
		ts := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		u := UsageRow{TS: ts, KeyName: key, ModelAlias: model, SourceName: source,
			AID: aid, In: 100, Out: 50, Status: status, ErrorKind: ekind,
			ReqID: "req-" + strconv.Itoa(i)}
		if err := s.InsertUsage(u); err != nil {
			t.Fatalf("InsertUsage: %v", err)
		}
	}
	for i := 0; i < 4; i++ {
		ins(i, "opencode", "model-a", "primary", "", 200, "")
	}
	ins(4, "cline", "model-b", "primary", "", 429, "rate_limited")
	ins(5, "cline", "model-b", "primary", "", 200, "")
	ins(6, "cline", "model-b", "primary", "", 200, "")
	for i := 7; i < 10; i++ {
		ins(i, "", "model-a", "agentfs", "aid-7", 200, "")
	}
	since := base.Add(-time.Minute).Format(time.RFC3339)
	noErr := false
	yesErr := true

	// 全量：10 行、按 id DESC（新→旧）
	rows, total, err := s.UsagePage(UsageFilter{Since: since}, 0, 50)
	if err != nil {
		t.Fatalf("UsagePage: %v", err)
	}
	if total != 10 || len(rows) != 10 {
		t.Fatalf("all: total=%d rows=%d, want 10/10", total, len(rows))
	}
	if rows[0].ReqID != "req-9" {
		t.Fatalf("newest first broken: %s", rows[0].ReqID)
	}

	// ident 精确过滤
	_, total, err = s.UsagePage(UsageFilter{Since: since, Ident: "opencode"}, 0, 50)
	if err != nil || total != 4 {
		t.Fatalf("ident filter: total=%d err=%v, want 4", total, err)
	}
	// 错误行（status>=400 或 error_kind 非空）
	_, total, _ = s.UsagePage(UsageFilter{Since: since, HasError: &yesErr}, 0, 50)
	if total != 1 {
		t.Fatalf("error filter total=%d, want 1", total)
	}
	_, total, _ = s.UsagePage(UsageFilter{Since: since, HasError: &noErr}, 0, 50)
	if total != 9 {
		t.Fatalf("ok filter total=%d, want 9", total)
	}
	// 跨列搜索：req id / error kind
	_, total, _ = s.UsagePage(UsageFilter{Since: since, Search: "rate_limited"}, 0, 50)
	if total != 1 {
		t.Fatalf("search error_kind total=%d, want 1", total)
	}
	_, total, _ = s.UsagePage(UsageFilter{Since: since, Search: "req-8"}, 0, 50)
	if total != 1 {
		t.Fatalf("search req id total=%d, want 1", total)
	}
	// 分页：page size 4 → 第 2 页 4 行，第 3 页 2 行
	rows, total, _ = s.UsagePage(UsageFilter{Since: since}, 4, 4)
	if total != 10 || len(rows) != 4 || rows[0].ReqID != "req-5" {
		t.Fatalf("page2: total=%d rows=%d first=%s", total, len(rows), rows[0].ReqID)
	}
	rows, _, _ = s.UsagePage(UsageFilter{Since: since}, 8, 4)
	if len(rows) != 2 {
		t.Fatalf("page3 rows=%d, want 2", len(rows))
	}
	// 窗口外不计：since 取 base+5s → 5 行
	_, total, _ = s.UsagePage(UsageFilter{
		Since: base.Add(5 * time.Second).Format(time.RFC3339)}, 0, 50)
	if total != 5 {
		t.Fatalf("window total=%d, want 5", total)
	}
}
