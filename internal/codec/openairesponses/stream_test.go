package openairesponses

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

// parseSSE splits an SSE body into event names and decoded data payloads.
func parseSSE(t *testing.T, body string) ([]string, []map[string]any) {
	t.Helper()
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return nil, nil
	}
	blocks := strings.Split(body, "\n\n")
	names := make([]string, 0, len(blocks))
	datas := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		lines := strings.Split(b, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("malformed SSE block %q", b)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(lines[1][len("data: "):]), &m); err != nil {
			t.Fatalf("bad data JSON in %q: %v", b, err)
		}
		names = append(names, lines[0][len("event: "):])
		datas = append(datas, m)
	}
	return names, datas
}

func eventCounts(names []string) map[string]int {
	c := map[string]int{}
	for _, n := range names {
		c[n]++
	}
	return c
}

// runStream feeds events to a fresh encoder and applies the invariants that
// hold for every Responses stream: sequence_number contiguous from 0, and
// every output_item.added paired with an output_item.done for the same
// output_index and item id.
func runStream(t *testing.T, evs []ir.StreamEvent, finish ir.Finish) (*httptest.ResponseRecorder, []string, []map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	for _, ev := range evs {
		if err := enc.Emit(ev); err != nil {
			t.Fatalf("emit %s: %v", ev.Kind, err)
		}
	}
	if err := enc.Close(finish); err != nil {
		t.Fatalf("close: %v", err)
	}
	names, datas := parseSSE(t, rec.Body.String())
	for i, d := range datas {
		if got := int(d["sequence_number"].(float64)); got != i {
			t.Fatalf("event %d (%s): sequence_number = %d, want %d", i, names[i], got, i)
		}
	}
	added := map[float64]map[string]any{}
	done := map[float64]map[string]any{}
	for i, name := range names {
		switch name {
		case "response.output_item.added":
			added[datas[i]["output_index"].(float64)] = datas[i]
		case "response.output_item.done":
			done[datas[i]["output_index"].(float64)] = datas[i]
		}
	}
	if len(added) != len(done) {
		t.Fatalf("added/done mismatch: %d added vs %d done", len(added), len(done))
	}
	for idx, a := range added {
		d, ok := done[idx]
		if !ok {
			t.Fatalf("output_index %v added but never done", idx)
		}
		if a["item"].(map[string]any)["id"] != d["item"].(map[string]any)["id"] {
			t.Fatalf("output_index %v: added item id %v != done item id %v",
				idx, a["item"].(map[string]any)["id"], d["item"].(map[string]any)["id"])
		}
	}
	return rec, names, datas
}

func lastData(t *testing.T, names []string, datas []map[string]any, event string) map[string]any {
	t.Helper()
	for i := len(names) - 1; i >= 0; i-- {
		if names[i] == event {
			return datas[i]
		}
	}
	t.Fatalf("event %s not found", event)
	return nil
}

func TestStreamLazyOpenAndHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	NewClientStreamEncoder(rec)
	if rec.Body.Len() != 0 {
		t.Fatalf("lazy open wrote %d bytes before first Emit", rec.Body.Len())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("cache-control = %q", cc)
	}
	if ab := rec.Header().Get("X-Accel-Buffering"); ab != "no" {
		t.Fatalf("x-accel-buffering = %q", ab)
	}
}

func TestStreamCloseWithoutEvents(t *testing.T) {
	_, names, datas := runStream(t, nil, ir.FinishStop)
	want := []string{"response.created", "response.in_progress", "response.completed"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v", names)
	}
	resp := lastData(t, names, datas, "response.completed")["response"].(map[string]any)
	if resp["status"] != "completed" || len(resp["output"].([]any)) != 0 {
		t.Fatalf("completed response = %v", resp)
	}
}

func TestStreamTextOnly(t *testing.T) {
	rec, names, datas := runStream(t, []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "test-model"},
		{Kind: ir.EvTextDelta, Text: "你"},
		{Kind: ir.EvTextDelta, Text: "好"},
		{Kind: ir.EvUsage, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2}},
	}, ir.FinishStop)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v", names)
	}

	created := datas[0]["response"].(map[string]any)
	if created["status"] != "in_progress" || created["usage"] != nil || created["model"] != "test-model" {
		t.Fatalf("created response = %v", created)
	}
	added := datas[2]["item"].(map[string]any)
	if added["type"] != "message" || added["status"] != "in_progress" {
		t.Fatalf("added message item = %v", added)
	}
	if datas[3]["part"].(map[string]any)["type"] != "output_text" {
		t.Fatalf("content_part.added = %v", datas[3])
	}
	if datas[4]["delta"] != "你" || datas[5]["delta"] != "好" {
		t.Fatalf("deltas = %v %v", datas[4], datas[5])
	}
	doneText := lastData(t, names, datas, "response.output_text.done")
	if doneText["text"] != "你好" {
		t.Fatalf("output_text.done = %v", doneText)
	}
	doneItem := lastData(t, names, datas, "response.output_item.done")["item"].(map[string]any)
	if doneItem["status"] != "completed" ||
		doneItem["content"].([]any)[0].(map[string]any)["text"] != "你好" {
		t.Fatalf("done message item = %v", doneItem)
	}
	resp := lastData(t, names, datas, "response.completed")["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("completed status = %v", resp["status"])
	}
	if len(resp["output"].([]any)) != 1 {
		t.Fatalf("completed output = %v", resp["output"])
	}
	u := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(10) || u["output_tokens"] != float64(2) || u["total_tokens"] != float64(12) {
		t.Fatalf("completed usage = %v", u)
	}
	if !strings.HasSuffix(rec.Body.String(), "\n\n") {
		t.Fatal("SSE body must end with a blank line")
	}
}

// Thinking must be fully closed (text.done + part.done + output_item.done)
// before the message item opens — the canonical order the sampled upstream
// violates (testdata resp_stream.txt interleaves them).
func TestStreamThinkingThenText(t *testing.T) {
	_, names, datas := runStream(t, []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "m"},
		{Kind: ir.EvThinkingDelta, Text: "想一"},
		{Kind: ir.EvThinkingDelta, Text: "想"},
		{Kind: ir.EvTextDelta, Text: "答"},
	}, ir.FinishStop)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v", names)
	}
	if got := lastData(t, names, datas, "response.reasoning_summary_text.done")["text"]; got != "想一想" {
		t.Fatalf("reasoning done text = %v", got)
	}
	resp := lastData(t, names, datas, "response.completed")["response"].(map[string]any)
	out := resp["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["type"] != "reasoning" ||
		out[1].(map[string]any)["type"] != "message" {
		t.Fatalf("completed output = %v", out)
	}
	sum := out[0].(map[string]any)["summary"].([]any)[0].(map[string]any)
	if sum["text"] != "想一想" {
		t.Fatalf("completed reasoning summary = %v", sum)
	}
}

func TestStreamToolCall(t *testing.T) {
	_, names, datas := runStream(t, []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "m"},
		{Kind: ir.EvThinkingDelta, Text: "查天气"},
		{Kind: ir.EvToolStart, ToolID: "call_00_f6cjso6e2ck984bd46iq7nlr", ToolName: "get_weather"},
		{Kind: ir.EvToolDelta, ToolID: "call_00_f6cjso6e2ck984bd46iq7nlr", Text: `{"ci`},
		{Kind: ir.EvToolDelta, ToolID: "call_00_f6cjso6e2ck984bd46iq7nlr", Text: `ty": "北京"}`},
		{Kind: ir.EvToolEnd, ToolID: "call_00_f6cjso6e2ck984bd46iq7nlr"},
		{Kind: ir.EvUsage, Usage: ir.Usage{InputTokens: 302, OutputTokens: 47}},
	}, ir.FinishToolCalls)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v", names)
	}
	fcAdded := datas[8]["item"].(map[string]any)
	if fcAdded["type"] != "function_call" || fcAdded["call_id"] != "call_00_f6cjso6e2ck984bd46iq7nlr" ||
		fcAdded["name"] != "get_weather" || fcAdded["arguments"] != "" || fcAdded["status"] != "in_progress" {
		t.Fatalf("function_call added item = %v", fcAdded)
	}
	argsDone := lastData(t, names, datas, "response.function_call_arguments.done")
	if argsDone["arguments"] != `{"city": "北京"}` {
		t.Fatalf("arguments done = %v", argsDone)
	}
	fcDone := lastData(t, names, datas, "response.output_item.done")["item"].(map[string]any)
	if fcDone["arguments"] != `{"city": "北京"}` || fcDone["status"] != "completed" ||
		fcDone["call_id"] != "call_00_f6cjso6e2ck984bd46iq7nlr" {
		t.Fatalf("function_call done item = %v", fcDone)
	}
	resp := lastData(t, names, datas, "response.completed")["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("tool_calls finish must yield completed, got %v", resp["status"])
	}
	out := resp["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["type"] != "reasoning" ||
		out[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("completed output = %v", out)
	}
}

// Parallel tool calls: both items open at once, argument fragments routed by
// call id, each closed by its own EvToolEnd.
func TestStreamParallelToolCalls(t *testing.T) {
	_, names, datas := runStream(t, []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "m"},
		{Kind: ir.EvToolStart, ToolID: "call_a", ToolName: "fa"},
		{Kind: ir.EvToolStart, ToolID: "call_b", ToolName: "fb"},
		{Kind: ir.EvToolDelta, ToolID: "call_a", Text: `{"x":`},
		{Kind: ir.EvToolDelta, ToolID: "call_b", Text: `{"y":`},
		{Kind: ir.EvToolDelta, ToolID: "call_a", Text: `1}`},
		{Kind: ir.EvToolEnd, ToolID: "call_a"},
		{Kind: ir.EvToolEnd, ToolID: "call_b"},
	}, ir.FinishStop)
	if got := eventCounts(names)["response.output_item.added"]; got != 2 {
		t.Fatalf("added count = %d", got)
	}
	resp := lastData(t, names, datas, "response.completed")["response"].(map[string]any)
	out := resp["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("completed output = %v", out)
	}
	a, b := out[0].(map[string]any), out[1].(map[string]any)
	if a["call_id"] != "call_a" || a["arguments"] != `{"x":1}` || a["name"] != "fa" {
		t.Fatalf("tool a = %v", a)
	}
	if b["call_id"] != "call_b" || b["arguments"] != `{"y":` {
		t.Fatalf("tool b = %v (b never got a closing fragment)", b)
	}
}

// Text before a tool call: the message item must be closed before the
// function_call item opens.
func TestStreamToolAfterText(t *testing.T) {
	_, names, _ := runStream(t, []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "m"},
		{Kind: ir.EvTextDelta, Text: "先说一句"},
		{Kind: ir.EvToolStart, ToolID: "call_a", ToolName: "fa"},
		{Kind: ir.EvToolDelta, ToolID: "call_a", Text: "1"},
		{Kind: ir.EvToolEnd, ToolID: "call_a"},
	}, ir.FinishStop)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v", names)
	}
}

func TestStreamLengthIncomplete(t *testing.T) {
	_, names, datas := runStream(t, []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "m"},
		{Kind: ir.EvTextDelta, Text: "截"},
		{Kind: ir.EvFinish, Finish: ir.FinishLength},
		{Kind: ir.EvUsage, Usage: ir.Usage{InputTokens: 5, OutputTokens: 1}},
	}, ir.FinishStop) // EvFinish (wire truth) must win over Close's fallback arg
	if names[len(names)-1] != "response.incomplete" {
		t.Fatalf("names = %v", names)
	}
	resp := lastData(t, names, datas, "response.incomplete")["response"].(map[string]any)
	if resp["status"] != "incomplete" {
		t.Fatalf("status = %v", resp["status"])
	}
	if resp["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %v", resp["incomplete_details"])
	}
	if resp["usage"].(map[string]any)["total_tokens"] != float64(6) {
		t.Fatalf("usage = %v", resp["usage"])
	}
}

// Tool-delta routing fallbacks: an empty ToolID binds to the single open
// tool call; fragments for an unknown/finished call are dropped, not fatal.
func TestStreamToolDeltaFallbacks(t *testing.T) {
	_, names, datas := runStream(t, []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "m"},
		{Kind: ir.EvToolStart, ToolID: "call_a", ToolName: "fa"},
		{Kind: ir.EvToolDelta, Text: `{"a":`}, // empty id → sole open tool
		{Kind: ir.EvToolDelta, ToolID: "zzz", Text: "junk"},
		{Kind: ir.EvToolEnd, ToolID: "call_a"},
		{Kind: ir.EvToolDelta, ToolID: "call_a", Text: "late"}, // already closed
	}, ir.FinishStop)
	if got := eventCounts(names)["response.function_call_arguments.delta"]; got != 1 {
		t.Fatalf("args delta count = %d (orphan fragments must be dropped)", got)
	}
	resp := lastData(t, names, datas, "response.completed")["response"].(map[string]any)
	fc := resp["output"].([]any)[0].(map[string]any)
	if fc["arguments"] != `{"a":` {
		t.Fatalf("arguments = %v", fc["arguments"])
	}
}

func TestStreamErrorMidStream(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	enc.Emit(ir.StreamEvent{Kind: ir.EvStart, Model: "m"})
	enc.Emit(ir.StreamEvent{Kind: ir.EvTextDelta, Text: "部分"})
	if err := enc.Emit(ir.StreamEvent{Kind: ir.EvError, ErrCode: "upstream_error", ErrMsg: "boom"}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(ir.FinishStop); err != nil {
		t.Fatal(err)
	}
	names, datas := parseSSE(t, rec.Body.String())
	if names[len(names)-1] != "response.failed" {
		t.Fatalf("names = %v", names)
	}
	if eventCounts(names)["response.completed"] != 0 {
		t.Fatal("no response.completed after failure")
	}
	resp := datas[len(datas)-1]["response"].(map[string]any)
	if resp["status"] != "failed" {
		t.Fatalf("status = %v", resp["status"])
	}
	e := resp["error"].(map[string]any)
	if e["code"] != "upstream_error" || e["message"] != "boom" {
		t.Fatalf("error = %v", e)
	}
	if len(resp["output"].([]any)) != 1 {
		t.Fatalf("failed response should carry partial output: %v", resp["output"])
	}
}

func TestStreamErrorFirstEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	if err := enc.Emit(ir.StreamEvent{Kind: ir.EvError, ErrCode: "invalid_key", ErrMsg: "bad key"}); err != nil {
		t.Fatal(err)
	}
	enc.Close(ir.FinishStop)
	names, _ := parseSSE(t, rec.Body.String())
	want := []string{"response.created", "response.in_progress", "response.failed"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v", names)
	}
}

// TestStreamGoldenReplay replays the exact delta fragments captured in
// testdata/resp_stream.txt (46 events from the sampled upstream) and checks the
// encoder reproduces the same event census in canonical order: same per-event
// counts, reasoning closed before the message opens, identical done texts,
// and identical full text in response.completed.
func TestStreamGoldenReplay(t *testing.T) {
	raw, err := os.ReadFile("testdata/resp_stream.txt")
	if err != nil {
		t.Fatal(err)
	}
	gNames, gDatas := parseSSE(t, string(raw))

	// Golden sanity: the upstream numbers events contiguously from 0.
	for i, d := range gDatas {
		if got := int(d["sequence_number"].(float64)); got != i {
			t.Fatalf("golden event %d: sequence_number = %d", i, got)
		}
	}

	var thinkDeltas, textDeltas []string
	for i, n := range gNames {
		switch n {
		case "response.reasoning_summary_text.delta":
			thinkDeltas = append(thinkDeltas, gDatas[i]["delta"].(string))
		case "response.output_text.delta":
			textDeltas = append(textDeltas, gDatas[i]["delta"].(string))
		}
	}
	if len(thinkDeltas) == 0 || len(textDeltas) == 0 {
		t.Fatal("golden stream has no deltas to replay")
	}

	evs := []ir.StreamEvent{{Kind: ir.EvStart, Model: "test-model"}}
	for _, d := range thinkDeltas {
		evs = append(evs, ir.StreamEvent{Kind: ir.EvThinkingDelta, Text: d})
	}
	for _, d := range textDeltas {
		evs = append(evs, ir.StreamEvent{Kind: ir.EvTextDelta, Text: d})
	}

	_, names, datas := runStream(t, evs, ir.FinishStop)

	// Same census as the golden stream.
	if !reflect.DeepEqual(eventCounts(names), eventCounts(gNames)) {
		t.Fatalf("event census mismatch:\n got %v\nwant %v", eventCounts(names), eventCounts(gNames))
	}

	// Canonical order: reasoning fully closed before the message item opens.
	goldenThinkDone := lastData(t, gNames, gDatas, "response.reasoning_summary_part.done")
	msgAdded := -1
	for i, n := range names {
		if n == "response.output_item.added" &&
			datas[i]["item"].(map[string]any)["type"] == "message" {
			msgAdded = i
			break
		}
	}
	if msgAdded < 0 {
		t.Fatal("no message item added")
	}
	closedBeforeMessage := false
	for _, n := range names[:msgAdded] {
		if n == "response.reasoning_summary_part.done" {
			closedBeforeMessage = true
			break
		}
	}
	if !closedBeforeMessage {
		t.Fatal("reasoning part.done must precede the message output_item.added")
	}

	// Done texts must equal the golden's, and equal the joined deltas.
	wantThink := goldenThinkDone["part"].(map[string]any)["text"].(string)
	if got := lastData(t, names, datas, "response.reasoning_summary_text.done")["text"]; got != wantThink {
		t.Fatalf("reasoning done text mismatch:\n got %q\nwant %q", got, wantThink)
	}
	wantText := lastData(t, gNames, gDatas, "response.output_text.done")["text"].(string)
	if got := lastData(t, names, datas, "response.output_text.done")["text"]; got != wantText {
		t.Fatalf("output_text done mismatch:\n got %q\nwant %q", got, wantText)
	}

	// response.completed carries the full rebuilt output.
	resp := lastData(t, names, datas, "response.completed")["response"].(map[string]any)
	if resp["model"] != "test-model" {
		t.Fatalf("model = %v", resp["model"])
	}
	out := resp["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("completed output = %v", out)
	}
	if got := out[0].(map[string]any)["summary"].([]any)[0].(map[string]any)["text"]; got != wantThink {
		t.Fatalf("completed reasoning = %q", got)
	}
	if got := out[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; got != wantText {
		t.Fatalf("completed message = %q", got)
	}
}
