package openairesponses

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

func keySet(m map[string]any) map[string]bool {
	s := make(map[string]bool, len(m))
	for k := range m {
		s[k] = true
	}
	return s
}

func sameKeys(t *testing.T, what string, got, want map[string]any) {
	t.Helper()
	g, w := keySet(got), keySet(want)
	for k := range g {
		if !w[k] {
			t.Errorf("%s: unexpected key %q (golden keys: %v)", what, k, w)
		}
	}
	for k := range w {
		if !g[k] {
			t.Errorf("%s: missing key %q", what, k)
		}
	}
}

// goldenOutputItem loads a golden response file and returns the first output
// item of the given type, for structural (key-set) comparison.
func goldenOutputItem(t *testing.T, file, itemType string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	for _, it := range g.Output {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(it, &probe); err != nil {
			t.Fatal(err)
		}
		if probe.Type == itemType {
			var m map[string]any
			if err := json.Unmarshal(it, &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("%s: no output item of type %s", file, itemType)
	return nil
}

func writeResponse(t *testing.T, res ir.Response) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := WriteClientResponse(rec, res); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	return rec, env
}

func TestWriteClientResponsePlain(t *testing.T) {
	_, env := writeResponse(t, ir.Response{
		Model: "test-model",
		Content: []ir.Part{
			{Type: ir.PartThinking, Text: "推理原文"},
			{Type: ir.PartText, Text: "最终回答"},
		},
		FinishReason: ir.FinishStop,
		Usage:        ir.Usage{InputTokens: 34, OutputTokens: 114},
	})

	if id, _ := env["id"].(string); !strings.HasPrefix(id, "resp_") {
		t.Fatalf("id = %v", env["id"])
	}
	if env["object"] != "response" || env["status"] != "completed" || env["model"] != "test-model" {
		t.Fatalf("envelope = %v", env)
	}
	if env["error"] != nil || env["incomplete_details"] != nil {
		t.Fatalf("error/incomplete_details should be null: %v / %v", env["error"], env["incomplete_details"])
	}
	if env["usage"] == nil {
		t.Fatal("usage missing")
	}

	out := env["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output len = %d", len(out))
	}
	rs := out[0].(map[string]any)
	msg := out[1].(map[string]any)

	// Structure must match golden resp_plain.json exactly (key sets).
	sameKeys(t, "reasoning item", rs, goldenOutputItem(t, "resp_plain.json", "reasoning"))
	sameKeys(t, "message item", msg, goldenOutputItem(t, "resp_plain.json", "message"))
	if rs["type"] != "reasoning" || !strings.HasPrefix(rs["id"].(string), "rs_") {
		t.Fatalf("reasoning item = %v", rs)
	}
	sum := rs["summary"].([]any)[0].(map[string]any)
	sameKeys(t, "summary part", sum, goldenOutputItem(t, "resp_plain.json", "reasoning")["summary"].([]any)[0].(map[string]any))
	if sum["type"] != "summary_text" || sum["text"] != "推理原文" {
		t.Fatalf("summary = %v", sum)
	}

	if msg["role"] != "assistant" || msg["status"] != "completed" {
		t.Fatalf("message item = %v", msg)
	}
	cp := msg["content"].([]any)[0].(map[string]any)
	sameKeys(t, "content part", cp, goldenOutputItem(t, "resp_plain.json", "message")["content"].([]any)[0].(map[string]any))
	if cp["type"] != "output_text" || cp["text"] != "最终回答" {
		t.Fatalf("content part = %v", cp)
	}
	if len(cp["annotations"].([]any)) != 0 || len(cp["logprobs"].([]any)) != 0 {
		t.Fatalf("annotations/logprobs should be empty arrays: %v", cp)
	}

	u := env["usage"].(map[string]any)
	if u["input_tokens"] != float64(34) || u["output_tokens"] != float64(114) || u["total_tokens"] != float64(148) {
		t.Fatalf("usage = %v", u)
	}
}

func TestWriteClientResponseToolCallBeforeMessage(t *testing.T) {
	// IR order text-first; wire order must be reasoning, function_calls,
	// then the message (Codex depends on tools preceding the message).
	_, env := writeResponse(t, ir.Response{
		Model: "test-model",
		Content: []ir.Part{
			{Type: ir.PartText, Text: "看天气"},
			{Type: ir.PartToolUse, ToolCallID: "call_00_f6cjso6e2ck984bd46iq7nlr",
				ToolName: "get_weather", ToolArgsJSON: `{"city": "北京"}`},
		},
		FinishReason: ir.FinishToolCalls,
		Usage:        ir.Usage{InputTokens: 302, OutputTokens: 47, CacheReadTokens: 256},
	})
	out := env["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output len = %d", len(out))
	}
	if out[0].(map[string]any)["type"] != "function_call" || out[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output order = %v", out)
	}
	fc := out[0].(map[string]any)
	sameKeys(t, "function_call item", fc, goldenOutputItem(t, "resp_tools.json", "function_call"))
	if fc["call_id"] != "call_00_f6cjso6e2ck984bd46iq7nlr" || fc["name"] != "get_weather" ||
		fc["arguments"] != `{"city": "北京"}` || fc["status"] != "completed" {
		t.Fatalf("function_call item = %v", fc)
	}
	if !strings.HasPrefix(fc["id"].(string), "fc_") {
		t.Fatalf("function_call id = %v", fc["id"])
	}
	if env["status"] != "completed" {
		t.Fatalf("tool_calls finish must map to completed, got %v", env["status"])
	}
}

func TestWriteClientResponseIncomplete(t *testing.T) {
	_, env := writeResponse(t, ir.Response{
		Model:        "m",
		Content:      []ir.Part{{Type: ir.PartText, Text: "截断"}},
		FinishReason: ir.FinishLength,
	})
	if env["status"] != "incomplete" {
		t.Fatalf("status = %v", env["status"])
	}
	det := env["incomplete_details"].(map[string]any)
	if det["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %v", det)
	}

	_, env = writeResponse(t, ir.Response{
		Model: "m", FinishReason: ir.FinishContentFilter,
	})
	if env["status"] != "incomplete" || env["incomplete_details"].(map[string]any)["reason"] != "content_filter" {
		t.Fatalf("content_filter mapping: %v", env["incomplete_details"])
	}
}

func TestWriteClientError(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := WriteClientError(rec, http.StatusTooManyRequests, "rate_limited", "slow down"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   any    `json:"param"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message != "slow down" || body.Error.Type != "rate_limited" {
		t.Fatalf("error envelope = %+v", body.Error)
	}
	if body.Error.Param != nil || body.Error.Code != nil {
		t.Fatalf("param/code should be null: %+v", body.Error)
	}
}
