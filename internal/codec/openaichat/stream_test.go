package openaichat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

// feedStream feeds a golden SSE file line by line into a fresh parser.
func feedStream(t *testing.T, name string) []ir.StreamEvent {
	t.Helper()
	raw := string(load(t, name))
	p := NewUpstreamStreamParser()
	var evs []ir.StreamEvent
	for _, line := range strings.Split(raw, "\n") {
		got, err := p.Line(line)
		if err != nil {
			t.Fatalf("%s: Line(%q) error: %v", name, line, err)
		}
		evs = append(evs, got...)
	}
	return evs
}

func countKind(evs []ir.StreamEvent, k ir.StreamKind) int {
	n := 0
	for _, e := range evs {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func concatKind(evs []ir.StreamEvent, k ir.StreamKind) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Kind == k {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

// ---- upstream stream parser vs golden chat_stream.txt ----

func TestUpstreamStreamParserChatStream(t *testing.T) {
	evs := feedStream(t, "chat_stream.txt")

	if len(evs) == 0 || evs[0].Kind != ir.EvStart || evs[0].Model != "test-model" {
		t.Fatalf("first event should be EvStart with model, got %+v", evs[0])
	}
	if got, want := countKind(evs, ir.EvStart), 1; got != want {
		t.Errorf("EvStart count = %d, want %d", got, want)
	}
	// the upstream sends content:"" and reasoning_content:"" in every chunk; the
	// empty halves must be dropped, only real deltas survive.
	wantThink := "我们需要回答中文，一句话介绍自己。作为AI助手。简洁。需要符合“一句话”。可以：我是由TestBot公司开发的AI助手，乐于为你提供信息与帮助。注意不要 reveal? 可以。用一句话。最终只给一句话。"
	if got := concatKind(evs, ir.EvThinkingDelta); got != wantThink {
		t.Errorf("thinking concat mismatch:\n got %q\nwant %q", got, wantThink)
	}
	wantText := "我是由TestLabs开发的AI助手，乐于为你提供信息与帮助。"
	if got := concatKind(evs, ir.EvTextDelta); got != wantText {
		t.Errorf("text concat mismatch:\n got %q\nwant %q", got, wantText)
	}
	if got := countKind(evs, ir.EvUsage); got != 1 {
		t.Fatalf("EvUsage count = %d, want 1", got)
	}
	for _, e := range evs {
		if e.Kind == ir.EvUsage {
			want := ir.Usage{InputTokens: 34, OutputTokens: 71, CacheReadTokens: 0, ReasoningTokens: 53}
			if e.Usage != want {
				t.Errorf("usage: %+v want %+v", e.Usage, want)
			}
		}
	}
	// exactly one EvFinish(stop), as the very last event, preceded by usage
	if n := countKind(evs, ir.EvFinish); n != 1 {
		t.Fatalf("EvFinish count = %d, want 1", n)
	}
	last := evs[len(evs)-1]
	if last.Kind != ir.EvFinish || last.Finish != ir.FinishStop {
		t.Errorf("last event: %+v", last)
	}
	if prev := evs[len(evs)-2]; prev.Kind != ir.EvUsage {
		t.Errorf("second to last event should be EvUsage: %+v", prev)
	}
	if n := countKind(evs, ir.EvToolStart); n != 0 {
		t.Errorf("unexpected tool events: %d", n)
	}
}

// ---- upstream stream parser vs golden chat_tools_stream.txt ----

func TestUpstreamStreamParserToolsStream(t *testing.T) {
	evs := feedStream(t, "chat_tools_stream.txt")
	const id = "call_00_4zvmdx414q1czyd93kbev19p"

	if got, want := concatKind(evs, ir.EvThinkingDelta),
		"The user asks about Beijing weather. I should call the weather tool."; got != want {
		t.Errorf("thinking: %q", got)
	}
	if n := countKind(evs, ir.EvToolStart); n != 1 {
		t.Fatalf("EvToolStart count = %d, want 1", n)
	}
	var start = evs[indexOf(evs, ir.EvToolStart)]
	if start.ToolID != id || start.ToolName != "get_weather" {
		t.Errorf("EvToolStart: %+v", start)
	}

	var args strings.Builder
	deltaIdx := -1
	for i, e := range evs {
		if e.Kind != ir.EvToolDelta {
			continue
		}
		if e.ToolID != id {
			t.Errorf("EvToolDelta id: %q", e.ToolID)
		}
		args.WriteString(e.Text)
		deltaIdx = i
	}
	if got, want := args.String(), `{"city": "北京"}`; got != want {
		t.Errorf("tool args concat: %q want %q", got, want)
	}
	if got, want := countKind(evs, ir.EvToolDelta), 9; got != want {
		t.Errorf("EvToolDelta count = %d, want %d", got, want)
	}

	endIdx := indexOf(evs, ir.EvToolEnd)
	if endIdx < 0 {
		t.Fatal("missing EvToolEnd")
	}
	if endIdx != deltaIdx+1 {
		t.Errorf("EvToolEnd should directly follow the last EvToolDelta: end=%d last-delta=%d", endIdx, deltaIdx)
	}
	if e := evs[endIdx]; e.ToolID != id {
		t.Errorf("EvToolEnd id: %q", e.ToolID)
	}
	// usage (302 in / 53 out / 256 cached / 14 reasoning) then finish tool_calls
	if u := evs[endIdx+1]; u.Kind != ir.EvUsage || u.Usage != (ir.Usage{InputTokens: 302, OutputTokens: 53, CacheReadTokens: 256, ReasoningTokens: 14}) {
		t.Errorf("usage event: %+v", evs[endIdx+1])
	}
	if f := evs[endIdx+2]; f.Kind != ir.EvFinish || f.Finish != ir.FinishToolCalls {
		t.Errorf("finish event: %+v", evs[endIdx+2])
	}
	if len(evs) != endIdx+3 {
		t.Errorf("events after finish: %+v", evs[endIdx+3:])
	}
}

func indexOf(evs []ir.StreamEvent, k ir.StreamKind) int {
	for i, e := range evs {
		if e.Kind == k {
			return i
		}
	}
	return -1
}

// ---- parser robustness: comments, CRLF, no-space data:, post-DONE, bad JSON ----

func TestUpstreamStreamParserRobustness(t *testing.T) {
	lines := []string{
		": keep-alive",
		"",
		"event: ping",
		`data:{"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}],"model":"m1"}`,
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"!\"}}]}\r",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		"data: [DONE]",
		`data: {"choices":[{"index":0,"delta":{"content":"ignored"}}]}`,
	}
	p := NewUpstreamStreamParser()
	var evs []ir.StreamEvent
	for _, l := range lines {
		got, err := p.Line(l)
		if err != nil {
			t.Fatalf("Line(%q): %v", l, err)
		}
		evs = append(evs, got...)
	}
	kinds := make([]string, len(evs))
	for i, e := range evs {
		kinds[i] = string(e.Kind)
	}
	want := strings.Join([]string{
		string(ir.EvStart), string(ir.EvTextDelta), string(ir.EvTextDelta),
		string(ir.EvUsage), string(ir.EvFinish),
	}, ",")
	if got := strings.Join(kinds, ","); got != want {
		t.Errorf("event kinds:\n got %s\nwant %s", got, want)
	}
	if evs[0].Model != "m1" {
		t.Errorf("EvStart model: %q", evs[0].Model)
	}
	if u := evs[3]; u.Usage.InputTokens != 3 || u.Usage.OutputTokens != 2 {
		t.Errorf("usage: %+v", u.Usage)
	}
}

func TestUpstreamStreamParserBadJSON(t *testing.T) {
	p := NewUpstreamStreamParser()
	evs, err := p.Line("data: {not json")
	if err != nil {
		t.Fatalf("Line should report via EvError, not error: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != ir.EvError || evs[0].ErrCode != ir.ErrUpstream {
		t.Fatalf("evs: %+v", evs)
	}
	// parser is poisoned after the error
	more, err := p.Line(`data: {"choices":[{"delta":{"content":"x"}}]}`)
	if err != nil || len(more) != 0 {
		t.Errorf("after error: evs=%v err=%v", more, err)
	}
}

// Two interleaved-by-index tools are closed in start order at finish.
func TestUpstreamStreamParserMultiTool(t *testing.T) {
	p := NewUpstreamStreamParser()
	lines := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t0","type":"function","function":{"name":"f0","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"t1","type":"function","function":{"name":"f1","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var evs []ir.StreamEvent
	for _, l := range lines {
		got, _ := p.Line(l)
		evs = append(evs, got...)
	}
	// t1 opening closes t0 even though t0 gets another fragment later
	wantKinds := []ir.StreamKind{ir.EvStart, ir.EvToolStart, ir.EvToolEnd, ir.EvToolStart, ir.EvToolDelta, ir.EvToolEnd, ir.EvFinish}
	if len(evs) != len(wantKinds) {
		t.Fatalf("events: %+v", evs)
	}
	for i, k := range wantKinds {
		if evs[i].Kind != k {
			t.Errorf("event %d: %s want %s (%+v)", i, evs[i].Kind, k, evs)
		}
	}
	if evs[5].ToolID != "t1" || evs[2].ToolID != "t0" {
		t.Errorf("tool end order: %+v", evs)
	}
}

// ---- client stream encoder ----

type encChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		PromptDetails    *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// sseLines splits recorded output into decoded JSON chunks (skipping [DONE]).
func sseLines(t *testing.T, body string) (chunks []encChunk, done bool) {
	t.Helper()
	for _, ln := range strings.Split(body, "\n\n") {
		ln = strings.TrimSuffix(ln, "\n")
		if ln == "" {
			continue
		}
		if !strings.HasPrefix(ln, "data: ") {
			t.Fatalf("line is not a data line: %q", ln)
		}
		payload := ln[len("data: "):]
		if payload == "[DONE]" {
			done = true
			continue
		}
		var c encChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		chunks = append(chunks, c)
	}
	return chunks, done
}

func TestClientStreamEncoder(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control: %q", cc)
	}
	if ab := rec.Header().Get("X-Accel-Buffering"); ab != "no" {
		t.Errorf("X-Accel-Buffering: %q", ab)
	}

	events := []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "test-model"},
		{Kind: ir.EvThinkingDelta, Text: "思考中"},
		{Kind: ir.EvTextDelta, Text: "你好"},
		{Kind: ir.EvToolStart, ToolID: "call_a", ToolName: "get_weather"},
		{Kind: ir.EvToolDelta, ToolID: "call_a", Text: `{"city"`},
		{Kind: ir.EvToolDelta, ToolID: "call_a", Text: `:"北京"}`},
		{Kind: ir.EvToolEnd, ToolID: "call_a"},
		{Kind: ir.EvToolStart, ToolID: "call_b", ToolName: "get_time"},
		{Kind: ir.EvToolDelta, ToolID: "call_b", Text: "{}"},
		{Kind: ir.EvUsage, Usage: ir.Usage{InputTokens: 302, OutputTokens: 53, CacheReadTokens: 256, ReasoningTokens: 14}},
		{Kind: ir.EvFinish, Finish: ir.FinishToolCalls},
	}
	for _, ev := range events {
		if err := enc.Emit(ev); err != nil {
			t.Fatalf("Emit(%s): %v", ev.Kind, err)
		}
	}
	if err := enc.Close(ir.FinishToolCalls); err != nil {
		t.Fatal(err)
	}

	chunks, done := sseLines(t, rec.Body.String())
	if !done {
		t.Fatal("missing data: [DONE]")
	}
	if len(chunks) != 10 { // open + think + text + 2 starts + 3 deltas + usage + finish
		t.Fatalf("chunks: %d\n%s", len(chunks), rec.Body.String())
	}
	for i, c := range chunks {
		if c.Object != "chat.completion.chunk" || !strings.HasPrefix(c.ID, "chatcmpl-") ||
			c.Created == 0 || c.Model != "test-model" {
			t.Errorf("chunk %d header: %+v", i, c)
		}
	}
	open := chunks[0]
	if len(open.Choices) != 1 || open.Choices[0].Delta.Role != "assistant" ||
		open.Choices[0].FinishReason != nil || open.Choices[0].Index != 0 {
		t.Errorf("opening chunk: %+v", open)
	}
	if c := chunks[1]; c.Choices[0].Delta.ReasoningContent != "思考中" || c.Choices[0].Delta.Content != "" {
		t.Errorf("thinking chunk: %+v", c.Choices[0].Delta)
	}
	if c := chunks[2]; c.Choices[0].Delta.Content != "你好" || c.Choices[0].Delta.ReasoningContent != "" {
		t.Errorf("text chunk: %+v", c.Choices[0].Delta)
	}
	ts := chunks[3].Choices[0].Delta.ToolCalls
	if len(ts) != 1 || ts[0].Index != 0 || ts[0].ID != "call_a" || ts[0].Type != "function" ||
		ts[0].Function.Name != "get_weather" || ts[0].Function.Arguments != "" {
		t.Errorf("tool start chunk: %+v", ts)
	}
	d1 := chunks[4].Choices[0].Delta.ToolCalls[0]
	if d1.Index != 0 || d1.ID != "" || d1.Type != "" || d1.Function.Name != "" || d1.Function.Arguments != `{"city"` {
		t.Errorf("tool delta chunk: %+v", d1)
	}
	d2 := chunks[5].Choices[0].Delta.ToolCalls[0]
	if d2.Function.Arguments != `:"北京"}` {
		t.Errorf("tool delta chunk 2: %+v", d2)
	}
	// EvToolEnd writes nothing on the wire: chunk 6 is the second tool start,
	// chunk 7 its argument delta.
	ts2 := chunks[6].Choices[0].Delta.ToolCalls[0]
	if ts2.Index != 1 || ts2.ID != "call_b" || ts2.Function.Name != "get_time" {
		t.Errorf("second tool start: %+v", ts2)
	}
	if db := chunks[7].Choices[0].Delta.ToolCalls[0]; db.Index != 1 || db.Function.Arguments != "{}" {
		t.Errorf("second tool delta: %+v", db)
	}
	u := chunks[8]
	if len(u.Choices) != 0 || u.Usage == nil || u.Usage.PromptTokens != 302 || u.Usage.CompletionTokens != 53 ||
		u.Usage.TotalTokens != 355 || u.Usage.PromptDetails == nil || u.Usage.PromptDetails.CachedTokens != 256 ||
		u.Usage.CompletionDetails == nil || u.Usage.CompletionDetails.ReasoningTokens != 14 {
		t.Errorf("usage chunk: %+v", u)
	}
	f := chunks[9]
	if f.Choices[0].FinishReason == nil || *f.Choices[0].FinishReason != "tool_calls" || f.Choices[0].Delta.Content != "" {
		t.Errorf("finish chunk: %+v", f.Choices[0])
	}
}

// Close without a prior EvFinish still terminates the stream properly.
func TestClientStreamEncoderCloseFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	_ = enc.Emit(ir.StreamEvent{Kind: ir.EvStart, Model: "m"})
	_ = enc.Emit(ir.StreamEvent{Kind: ir.EvTextDelta, Text: "部分"})
	if err := enc.Close(ir.FinishLength); err != nil {
		t.Fatal(err)
	}
	chunks, done := sseLines(t, rec.Body.String())
	if !done || len(chunks) != 3 {
		t.Fatalf("chunks=%d done=%v\n%s", len(chunks), done, rec.Body.String())
	}
	last := chunks[len(chunks)-1]
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "length" {
		t.Errorf("fallback finish chunk: %+v", last)
	}
}

func TestClientStreamEncoderError(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	_ = enc.Emit(ir.StreamEvent{Kind: ir.EvStart, Model: "m"})
	if err := enc.Emit(ir.StreamEvent{Kind: ir.EvError, ErrCode: ir.ErrRateLimited, ErrMsg: "slow down"}); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "slow down") {
		t.Errorf("error chunk missing: %s", body)
	}
}

// ---- non-streaming client response / error writers ----

func TestWriteClientResponse(t *testing.T) {
	res, err := ParseUpstreamBody(load(t, "chat_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := WriteClientResponse(rec, res); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: %q", ct)
	}
	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Logprobs     *any   `json:"logprobs"`
			Message      struct {
				Role             string  `json:"role"`
				Content          *string `json:"content"`
				ReasoningContent string  `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			PromptDetails    *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl-") || out.Object != "chat.completion" ||
		out.Model != "test-model" {
		t.Errorf("header fields: %+v", out)
	}
	c := out.Choices[0]
	if c.FinishReason != "tool_calls" || c.Logprobs != nil {
		t.Errorf("choice: %+v", c)
	}
	if c.Message.Role != "assistant" || c.Message.Content != nil {
		t.Errorf("message: %+v", c.Message)
	}
	if c.Message.ReasoningContent != "We need to call the weather tool for Beijing." {
		t.Errorf("reasoning_content: %q", c.Message.ReasoningContent)
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "call_00_94091ex7gxe95toioqwsoltl" || tc.Type != "function" ||
		tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city": "北京"}` {
		t.Errorf("tool_calls: %+v (arguments must be the raw string)", tc)
	}
	if out.Usage.PromptTokens != 302 || out.Usage.CompletionTokens != 49 || out.Usage.TotalTokens != 351 {
		t.Errorf("usage: %+v", out.Usage)
	}
	if out.Usage.PromptDetails != nil {
		t.Errorf("prompt_tokens_details should be omitted when cached=0: %+v", out.Usage)
	}
	if out.Usage.CompletionDetails == nil || out.Usage.CompletionDetails.ReasoningTokens != 10 {
		t.Errorf("completion_tokens_details: %+v", out.Usage.CompletionDetails)
	}
}

func TestWriteClientResponseTextOnly(t *testing.T) {
	res, err := ParseUpstreamBody(load(t, "chat_reasoning.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := WriteClientResponse(rec, res); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          *string `json:"content"`
				ReasoningContent string  `json:"reasoning_content"`
				ToolCalls        []any   `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	c := out.Choices[0]
	if c.FinishReason != "stop" || c.Message.Content == nil || *c.Message.Content != "25" {
		t.Errorf("message: %+v", c.Message)
	}
	if c.Message.ReasoningContent == "" {
		t.Error("reasoning_content should be present")
	}
	if out.Usage.CompletionDetails == nil || out.Usage.CompletionDetails.ReasoningTokens != 23 {
		t.Errorf("usage: %+v", out.Usage)
	}
}

func TestWriteClientError(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := WriteClientError(rec, http.StatusTooManyRequests, ir.ErrRateLimited, "slow down"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status: %d", rec.Code)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error.Message != "slow down" || out.Error.Type != "rate_limit_error" ||
		out.Error.Code != ir.ErrRateLimited || out.Error.Param != "" {
		t.Errorf("error body: %+v", out.Error)
	}
}

// golden stream files are pure data: lines — no "event:" prefixes anywhere.
func TestGoldenStreamShape(t *testing.T) {
	for _, name := range []string{"chat_stream.txt", "chat_tools_stream.txt"} {
		for _, ln := range strings.Split(string(load(t, name)), "\n") {
			if ln == "" || ln == "data: [DONE]" {
				continue
			}
			if !strings.HasPrefix(ln, "data: {") {
				t.Errorf("%s: unexpected line %q", name, ln)
			}
			var probe map[string]any
			if err := json.Unmarshal([]byte(ln[len("data: "):]), &probe); err != nil {
				t.Errorf("%s: bad JSON line: %v", name, err)
			}
		}
	}
}
