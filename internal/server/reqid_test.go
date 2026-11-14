package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"kleinia.io/llmgw/internal/ir"
	"kleinia.io/llmgw/internal/router"
	"kleinia.io/llmgw/internal/store"
)

// Req-id header tests. The contract under test:
// x-kln-req-id rides on every response that produced at least one usage
// row — success, terminal error and exhausted retries, stream and
// non-stream — and is absent on responses that produced none (parse /
// auth / ACL / quota / routing rejections), because agenttrace counts
// "req_id visible but no matching usage row" as a bypass call.

// ---- fake codecs ---------------------------------------------------------

type fakeEnc struct{ w http.ResponseWriter }

func (e fakeEnc) Emit(ev ir.StreamEvent) error {
	fmt.Fprintf(e.w, "data: {\"kind\":%q}\n\n", ev.Kind)
	return nil
}

func (e fakeEnc) Close(ir.Finish) error {
	io.WriteString(e.w, "data: [DONE]\n\n")
	return nil
}

type fakeParser struct{}

func (fakeParser) Line(line string) ([]ir.StreamEvent, error) {
	if strings.Contains(line, "hi") {
		return []ir.StreamEvent{{Kind: ir.EvTextDelta, Text: "hi"}}, nil
	}
	return nil, nil
}

func fakeClientCodec() ClientCodec {
	return ClientCodec{
		Parse: func(body []byte) (ir.Request, error) {
			var r struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			if err := json.Unmarshal(body, &r); err != nil || r.Model == "" {
				return ir.Request{}, fmt.Errorf("model required")
			}
			return ir.Request{Model: r.Model, Stream: r.Stream}, nil
		},
		WriteResp: func(w http.ResponseWriter, res ir.Response) error {
			writeJSON(w, http.StatusOK, map[string]string{"model": res.Model})
			return nil
		},
		NewEnc: func(w http.ResponseWriter) StreamEncoder { return fakeEnc{w} },
		WriteErr: func(w http.ResponseWriter, status int, kind, msg string) error {
			writeJSON(w, status, map[string]string{"error": kind, "message": msg})
			return nil
		},
	}
}

// newReqIDServer wires a Server with the fake chat codec and one stub
// upstream source; upstream is the canned upstream HTTP handler.
func newReqIDServer(t *testing.T, upstream http.HandlerFunc, parse func([]byte) (ir.Response, error)) (*httptest.Server, *store.Store, string) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	if _, err := st.AddSource(store.Source{Name: "stub", Type: "stubup", BaseURL: up.URL, APIKey: "k", Enabled: true, Priority: 1},
		[]store.ModelMapping{{Alias: "m1", UpstreamModel: "m1-up"}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.AddKey("reqid-test", "*", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{}, st, router.New(nil), LoadPrices(""))
	// Handler() registers all three client formats; the fake codec covers
	// each so handleLLM's codec lookup cannot panic.
	clients := map[string]ClientCodec{}
	for _, f := range []string{"chat", "responses", "anthropic"} {
		clients[f] = fakeClientCodec()
	}
	srv.SetCodecs(clients, map[string]UpstreamCodec{
		"stubup": {
			Build: func(ctx context.Context, baseURL, apiKey, model string, req ir.Request, stream bool) (*http.Request, error) {
				return http.NewRequestWithContext(ctx, "POST", baseURL, strings.NewReader("{}"))
			},
			ParseBody: parse,
			NewParser: func() StreamParser { return fakeParser{} },
		},
	})
	srv.RefreshSources()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st, key.Key
}

var reqIDRe = regexp.MustCompile(`^req_[0-9a-f]{16}$`)

func gwPost(t *testing.T, url, body, auth string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func lastRows(t *testing.T, st *store.Store, n int) []store.UsageRow {
	t.Helper()
	rows, err := st.RecentUsage(n)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestReqIDHeaderNonStreamSuccess(t *testing.T) {
	okParse := func([]byte) (ir.Response, error) {
		return ir.Response{Model: "m1-up", Content: []ir.Part{{Type: ir.PartText, Text: "ok"}},
			FinishReason: ir.FinishStop, Usage: ir.Usage{InputTokens: 5, OutputTokens: 3}}, nil
	}
	ts, st, key := newReqIDServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("{}"))
	}, okParse)

	resp := gwPost(t, ts.URL+"/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"x"}]}`, key)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	hdr := resp.Header.Get(ReqIDHeader)
	if !reqIDRe.MatchString(hdr) {
		t.Fatalf("header %q does not match req_<16hex>", hdr)
	}
	rows := lastRows(t, st, 1)
	if len(rows) != 1 || rows[0].ReqID != hdr {
		t.Fatalf("usage row req_id %v does not match header %q", rows, hdr)
	}
	if rows[0].Status != 200 || rows[0].Stream {
		t.Fatalf("unexpected row %+v", rows[0])
	}
}

func TestReqIDHeaderStreamSuccess(t *testing.T) {
	okParse := func([]byte) (ir.Response, error) {
		return ir.Response{}, nil
	}
	ts, st, key := newReqIDServer(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "data: hi\n\n")
	}, okParse)

	resp := gwPost(t, ts.URL+"/v1/chat/completions", `{"model":"m1","stream":true,"messages":[{"role":"user","content":"x"}]}`, key)
	// Headers must carry the id before any body byte is consumed.
	hdr := resp.Header.Get(ReqIDHeader)
	if !reqIDRe.MatchString(hdr) {
		t.Fatalf("stream response header %q does not match req_<16hex>", hdr)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("stream body incomplete: %q", body)
	}
	rows := lastRows(t, st, 1)
	if len(rows) != 1 || rows[0].ReqID != hdr || !rows[0].Stream {
		t.Fatalf("stream usage row %+v does not match header %q", rows, hdr)
	}
}

func TestReqIDHeaderTerminalUpstreamError(t *testing.T) {
	errParse := func([]byte) (ir.Response, error) {
		return ir.Response{}, &ir.UpstreamError{Kind: ir.ErrUpstream, Message: "bad input"}
	}
	ts, st, key := newReqIDServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}, errParse)

	resp := gwPost(t, ts.URL+"/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"x"}]}`, key)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d (want 400)", resp.StatusCode)
	}
	hdr := resp.Header.Get(ReqIDHeader)
	if !reqIDRe.MatchString(hdr) {
		t.Fatalf("terminal-error response missing valid header: %q", hdr)
	}
	rows := lastRows(t, st, 1)
	if len(rows) != 1 || rows[0].ReqID != hdr || rows[0].Status != 400 || rows[0].ErrorKind != ir.ErrUpstream {
		t.Fatalf("error usage row %+v does not match header %q", rows, hdr)
	}
}

func TestReqIDHeaderRetryExhausted(t *testing.T) {
	// Two sources, both failing retryably: every attempt writes a usage row
	// sharing one req id, and the final "all candidates failed" response
	// still carries it.
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(up.Close)
	for i, prio := range []int{2, 1} {
		if _, err := st.AddSource(store.Source{Name: fmt.Sprintf("stub%d", i), Type: "stubup",
			BaseURL: up.URL, APIKey: "k", Enabled: true, Priority: prio},
			[]store.ModelMapping{{Alias: "m1", UpstreamModel: "m1-up"}}); err != nil {
			t.Fatal(err)
		}
	}
	krow, err := st.AddKey("reqid-retry", "*", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := krow.Key
	srv := New(Config{}, st, router.New(nil), LoadPrices(""))
	clients := map[string]ClientCodec{}
	for _, f := range []string{"chat", "responses", "anthropic"} {
		clients[f] = fakeClientCodec()
	}
	srv.SetCodecs(clients, map[string]UpstreamCodec{
		"stubup": {
			Build: func(ctx context.Context, baseURL, apiKey, model string, req ir.Request, stream bool) (*http.Request, error) {
				return http.NewRequestWithContext(ctx, "POST", baseURL, strings.NewReader("{}"))
			},
			ParseBody: func([]byte) (ir.Response, error) {
				return ir.Response{}, &ir.UpstreamError{Kind: ir.ErrNetwork, Message: "boom"}
			},
			NewParser: func() StreamParser { return fakeParser{} },
		},
	})
	srv.RefreshSources()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := gwPost(t, ts.URL+"/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"x"}]}`, key)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d (want 502)", resp.StatusCode)
	}
	hdr := resp.Header.Get(ReqIDHeader)
	if !reqIDRe.MatchString(hdr) {
		t.Fatalf("exhausted-retry response missing valid header: %q", hdr)
	}
	var mine []store.UsageRow
	for _, r := range lastRows(t, st, 10) {
		if r.ReqID == hdr {
			mine = append(mine, r)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("want 2 usage rows (one per attempt) with req id %s, got %d", hdr, len(mine))
	}
}

func TestReqIDHeaderAbsentWithoutUsageRow(t *testing.T) {
	okParse := func([]byte) (ir.Response, error) { return ir.Response{}, nil }
	ts, st, key := newReqIDServer(t, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("{}")) }, okParse)

	cases := []struct {
		name string
		body string
		auth string
		want int
	}{
		{"unknown model", `{"model":"nope","messages":[]}`, key, 404},
		{"missing auth", `{"model":"m1","messages":[]}`, "", 401},
		{"bad key", `{"model":"m1","messages":[]}`, "sk-kln-bogus", 401},
		{"unparseable body", `{"model":`, key, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(lastRows(t, st, 100))
			resp := gwPost(t, ts.URL+"/v1/chat/completions", tc.body, tc.auth)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d (want %d)", resp.StatusCode, tc.want)
			}
			if h := resp.Header.Get(ReqIDHeader); h != "" {
				t.Fatalf("rejection carried %s: %q — ids without a usage row read as bypass calls", ReqIDHeader, h)
			}
			if after := len(lastRows(t, st, 100)); after != before {
				t.Fatalf("usage rows written for a rejected request: %d -> %d", before, after)
			}
		})
	}
}

func TestNewReqIDFormatAndWriteReqID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newReqID()
		if !reqIDRe.MatchString(id) {
			t.Fatalf("id %q does not match req_<16hex>", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	rec := httptest.NewRecorder()
	writeReqID(rec, "")
	if got := rec.Header().Get(ReqIDHeader); got != "" {
		t.Fatalf("empty id must not write the header, got %q", got)
	}
	writeReqID(rec, "req_deadbeefdeadbeef")
	if got := rec.Header().Get(ReqIDHeader); got != "req_deadbeefdeadbeef" {
		t.Fatalf("header not written: %q", got)
	}
}
