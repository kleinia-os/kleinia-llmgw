package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return string(data)
}

// ---- BuildUpstreamRequest ----

// The full circle: a Responses client request (the round-trip form the upstream
// accepted verbatim, plus store/reasoning extras) parses into IR and rebuilds
// as an upstream /responses request.
const upstreamRoundtripClientBody = `{
  "model": "alias-ds",
  "instructions": "你是天气助手",
  "input": [
    {"role": "user", "content": [{"type": "input_text", "text": "北京天气怎么样？"}]},
    {"type": "reasoning", "id": "rs_1",
     "summary": [{"type": "summary_text", "text": "用户问北京天气。"}]},
    {"type": "function_call", "call_id": "call_00_f6cjso6e2ck984bd46iq7nlr",
     "name": "get_weather", "arguments": "{\"city\": \"北京\"}"},
    {"type": "function_call_output", "call_id": "call_00_f6cjso6e2ck984bd46iq7nlr",
     "output": "{\"temp_c\":18,\"condition\":\"晴\"}"}
  ],
  "tools": [{"type": "function", "name": "get_weather", "description": "查询指定城市的当前天气",
             "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}],
  "tool_choice": "auto",
  "max_output_tokens": 128,
  "temperature": 0,
  "store": false,
  "reasoning": {"effort": "low"}
}`

func TestBuildUpstreamRequestRoundtrip(t *testing.T) {
	req, err := ParseClientRequest([]byte(upstreamRoundtripClientBody))
	if err != nil {
		t.Fatal(err)
	}
	hreq, err := BuildUpstreamRequest(context.Background(), "https://upstream.example.test/v1/", "sk-test", "ds-flash-upstream", req, false)
	if err != nil {
		t.Fatal(err)
	}
	if hreq.Method != http.MethodPost {
		t.Fatalf("method = %s", hreq.Method)
	}
	if got := hreq.URL.String(); got != "https://upstream.example.test/v1/responses" {
		t.Fatalf("url = %s", got)
	}
	h := hreq.Header
	if h.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("authorization = %q", h.Get("Authorization"))
	}
	if h.Get("Content-Type") != "application/json" || h.Get("Accept") != "application/json" {
		t.Fatalf("headers = %v", h)
	}

	body, err := io.ReadAll(hreq.Body)
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Model           string            `json:"model"`
		Instructions    string            `json:"instructions"`
		Input           []json.RawMessage `json:"input"`
		MaxOutputTokens int64             `json:"max_output_tokens"`
		Temperature     *float64          `json:"temperature"`
		ToolChoice      string            `json:"tool_choice"`
		Stream          bool              `json:"stream"`
		Tools           []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"tools"`
		Store     *bool           `json:"store"`
		Reasoning json.RawMessage `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("body: %v\n%s", err, body)
	}
	if b.Model != "ds-flash-upstream" {
		t.Fatalf("model = %q, want resolved upstream name", b.Model)
	}
	if b.Instructions != "你是天气助手" {
		t.Fatalf("instructions = %q", b.Instructions)
	}
	if b.MaxOutputTokens != 128 || b.Temperature == nil || *b.Temperature != 0 {
		t.Fatalf("sampling = %d / %v", b.MaxOutputTokens, b.Temperature)
	}
	if b.ToolChoice != "auto" {
		t.Fatalf("tool_choice = %q", b.ToolChoice)
	}
	if b.Stream || strings.Contains(string(body), `"stream"`) {
		t.Fatalf("stream must be omitted for non-streaming: %s", body)
	}
	if len(b.Input) != 3 {
		t.Fatalf("input items = %s", body)
	}
	// item 0: user message with input_text
	var msg struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b.Input[0], &msg); err != nil ||
		msg.Type != "message" || msg.Role != "user" ||
		len(msg.Content) != 1 || msg.Content[0].Type != "input_text" ||
		msg.Content[0].Text != "北京天气怎么样？" {
		t.Fatalf("input[0] = %s", b.Input[0])
	}
	// item 1: function_call (assistant thinking dropped — no signature to
	// replay), item 2: its function_call_output, immediately after.
	var call struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(b.Input[1], &call); err != nil ||
		call.Type != "function_call" || call.CallID != "call_00_f6cjso6e2ck984bd46iq7nlr" ||
		call.Name != "get_weather" || call.Arguments != `{"city": "北京"}` {
		t.Fatalf("input[1] = %s", b.Input[1])
	}
	var out struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(b.Input[2], &out); err != nil ||
		out.Type != "function_call_output" || out.CallID != "call_00_f6cjso6e2ck984bd46iq7nlr" ||
		out.Output != `{"temp_c":18,"condition":"晴"}` {
		t.Fatalf("input[2] = %s", b.Input[2])
	}
	// tools keep the flat Responses format, parameters verbatim
	if len(b.Tools) != 1 || b.Tools[0].Type != "function" || b.Tools[0].Name != "get_weather" ||
		b.Tools[0].Description != "查询指定城市的当前天气" {
		t.Fatalf("tools = %+v", b.Tools)
	}
	var schema struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b.Tools[0].Parameters, &schema); err != nil || schema.Type != "object" {
		t.Fatalf("parameters = %s", b.Tools[0].Parameters)
	}
	// same-format extras pass through
	if b.Store == nil || *b.Store {
		t.Fatalf("store passthrough = %v", b.Store)
	}
	if string(b.Reasoning) != `{"effort":"low"}` {
		t.Fatalf("reasoning passthrough = %s", b.Reasoning)
	}
}

func TestBuildUpstreamRequestDirectIR(t *testing.T) {
	temp := 0.7
	req := ir.Request{
		Messages: []ir.Message{
			{Role: ir.RoleSystem, Content: []ir.Part{{Type: ir.PartText, Text: "第一条系统"}}},
			{Role: ir.RoleSystem, Content: []ir.Part{{Type: ir.PartText, Text: "第二条系统"}}},
			{Role: ir.RoleUser, Content: []ir.Part{
				{Type: ir.PartImage, ImageURL: "https://x/a.png"},
				{Type: ir.PartText, Text: "看图说话"},
			}},
			{Role: ir.RoleAssistant, Content: []ir.Part{
				{Type: ir.PartThinking, Text: "思考不回传"},
				{Type: ir.PartText, Text: "我来查一下"},
				{Type: ir.PartToolUse, ToolCallID: "c1", ToolName: "fa", ToolArgsJSON: `{"q":1}`},
			}},
			{Role: ir.RoleTool, Content: []ir.Part{
				{Type: ir.PartToolResult, ToolCallID: "c1", ToolOutput: `{"r":2}`},
			}},
			{Role: ir.RoleAssistant, Content: []ir.Part{{Type: ir.PartText, Text: "结论"}}},
		},
		Sampling: ir.Sampling{Temperature: &temp},
		Raw: map[string]json.RawMessage{
			"model":             json.RawMessage(`"evil"`),
			"input":             json.RawMessage(`[]`),
			"max_output_tokens": json.RawMessage(`1`),
			"metadata":          json.RawMessage(`{"u":"k"}`),
			"junk":              json.RawMessage(`null`),
		},
	}
	hreq, err := BuildUpstreamRequest(context.Background(), "https://x/v1", "k", "m", req, true)
	if err != nil {
		t.Fatal(err)
	}
	if acc := hreq.Header.Get("Accept"); acc != "text/event-stream" {
		t.Fatalf("accept = %q", acc)
	}
	body, _ := io.ReadAll(hreq.Body)
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatal(err)
	}
	if generic["instructions"] != "第一条系统\n第二条系统" {
		t.Fatalf("instructions = %v", generic["instructions"])
	}
	if _, ok := generic["max_output_tokens"]; ok {
		t.Fatal("unset max_output_tokens must be omitted (official spec: optional)")
	}
	if _, ok := generic["junk"]; ok {
		t.Fatalf("null raw value leaked: %s", body)
	}
	if generic["metadata"].(map[string]any)["u"] != "k" {
		t.Fatalf("metadata passthrough missing: %s", body)
	}
	if generic["model"] != "m" || strings.Count(string(body), `"model"`) != 1 ||
		strings.Count(string(body), `"input"`) != 1 {
		t.Fatalf("owned field duplicated by passthrough: %s", body)
	}
	if v, ok := generic["stream"].(bool); !ok || !v {
		t.Fatalf("stream flag missing: %s", body)
	}
	// input item order: user(img+text), assistant message, function_call,
	// function_call_output, assistant message.
	items := generic["input"].([]any)
	if len(items) != 5 {
		t.Fatalf("input = %s", body)
	}
	wantTypes := []string{"message", "message", "function_call", "function_call_output", "message"}
	for i, want := range wantTypes {
		if items[i].(map[string]any)["type"] != want {
			t.Fatalf("input[%d].type = %v, want %s (%s)", i, items[i], want, body)
		}
	}
	userContent := items[0].(map[string]any)["content"].([]any)
	if userContent[0].(map[string]any)["type"] != "input_image" ||
		userContent[0].(map[string]any)["image_url"] != "https://x/a.png" ||
		userContent[1].(map[string]any)["type"] != "input_text" {
		t.Fatalf("user content = %v", userContent)
	}
	asst := items[1].(map[string]any)
	if asst["role"] != "assistant" ||
		asst["content"].([]any)[0].(map[string]any)["type"] != "output_text" {
		t.Fatalf("assistant message = %v", asst)
	}
	if items[3].(map[string]any)["call_id"] != "c1" || items[2].(map[string]any)["call_id"] != "c1" {
		t.Fatalf("function_call / output not adjacent and paired: %s", body)
	}
}

func TestBuildUpstreamRequestToolChoiceAndErrors(t *testing.T) {
	for _, tc := range []struct {
		choice ir.ToolChoice
		want   string
	}{
		{ir.ToolChoice{Type: ir.ToolChoiceAuto}, `"tool_choice":"auto"`},
		{ir.ToolChoice{Type: ir.ToolChoiceNone}, `"tool_choice":"none"`},
		{ir.ToolChoice{Type: ir.ToolChoiceRequired}, `"tool_choice":"required"`},
		{ir.ToolChoice{Type: ir.ToolChoiceNamed, Name: "get_weather"}, `"tool_choice":{"type":"function","name":"get_weather"}`},
	} {
		req := ir.Request{
			Messages:   []ir.Message{{Role: ir.RoleUser, Content: []ir.Part{{Type: ir.PartText, Text: "hi"}}}},
			ToolChoice: &tc.choice,
		}
		hreq, err := BuildUpstreamRequest(context.Background(), "https://x", "k", "m", req, false)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(hreq.Body)
		if !strings.Contains(string(body), tc.want) {
			t.Fatalf("tool_choice %v: body = %s", tc.choice, body)
		}
	}
	// system-only conversation has no input items to send
	req := ir.Request{Messages: []ir.Message{{Role: ir.RoleSystem, Content: []ir.Part{{Type: ir.PartText, Text: "sys"}}}}}
	if _, err := BuildUpstreamRequest(context.Background(), "https://x", "k", "m", req, false); err == nil {
		t.Fatal("expected error for empty input")
	}
}

// ---- ParseUpstreamBody ----

func TestParseUpstreamBodyGolden(t *testing.T) {
	t.Run("resp_plain", func(t *testing.T) {
		res, err := ParseUpstreamBody([]byte(readTestdata(t, "resp_plain.json")))
		if err != nil {
			t.Fatal(err)
		}
		if res.Model != "test-model" || res.FinishReason != ir.FinishStop {
			t.Fatalf("model=%q finish=%q", res.Model, res.FinishReason)
		}
		if len(res.Content) != 2 || res.Content[0].Type != ir.PartThinking || res.Content[1].Type != ir.PartText {
			t.Fatalf("content = %+v", res.Content)
		}
		if !strings.HasPrefix(res.Content[0].Text, "我们需要回答中文一句话自我介绍") {
			t.Fatalf("thinking = %q", res.Content[0].Text)
		}
		want := ir.Usage{InputTokens: 34, OutputTokens: 114, ReasoningTokens: 93}
		if res.Usage != want {
			t.Fatalf("usage = %+v, want %+v", res.Usage, want)
		}
	})
	t.Run("resp_tools", func(t *testing.T) {
		res, err := ParseUpstreamBody([]byte(readTestdata(t, "resp_tools.json")))
		if err != nil {
			t.Fatal(err)
		}
		if res.FinishReason != ir.FinishToolCalls {
			t.Fatalf("finish = %q", res.FinishReason)
		}
		if len(res.Content) != 2 || res.Content[0].Type != ir.PartThinking || res.Content[1].Type != ir.PartToolUse {
			t.Fatalf("content = %+v", res.Content)
		}
		tu := res.Content[1]
		if tu.ToolCallID != "call_00_f6cjso6e2ck984bd46iq7nlr" || tu.ToolName != "get_weather" ||
			tu.ToolArgsJSON != `{"city": "北京"}` {
			t.Fatalf("tool part = %+v", tu)
		}
		want := ir.Usage{InputTokens: 302, OutputTokens: 47, CacheReadTokens: 256, ReasoningTokens: 8}
		if res.Usage != want {
			t.Fatalf("usage = %+v, want %+v", res.Usage, want)
		}
	})
	t.Run("resp_tools_roundtrip", func(t *testing.T) {
		res, err := ParseUpstreamBody([]byte(readTestdata(t, "resp_tools_roundtrip.json")))
		if err != nil {
			t.Fatal(err)
		}
		if res.FinishReason != ir.FinishStop {
			t.Fatalf("finish = %q", res.FinishReason)
		}
		want := ir.Usage{InputTokens: 371, OutputTokens: 49, CacheReadTokens: 256, ReasoningTokens: 18}
		if res.Usage != want {
			t.Fatalf("usage = %+v, want %+v", res.Usage, want)
		}
		if len(res.Content) != 2 || res.Content[1].Text == "" {
			t.Fatalf("content = %+v", res.Content)
		}
	})
}

func TestParseUpstreamBodyIncomplete(t *testing.T) {
	res, err := ParseUpstreamBody([]byte(`{"id":"resp_x","object":"response","status":"incomplete",
	  "incomplete_details":{"reason":"max_output_tokens"},"model":"m",
	  "output":[{"id":"msg_1","type":"message","role":"assistant","status":"incomplete",
	             "content":[{"type":"output_text","text":"截断","annotations":[],"logprobs":[]}]}],
	  "usage":{"input_tokens":10,"output_tokens":128,"total_tokens":138}}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.FinishReason != ir.FinishLength || len(res.Content) != 1 ||
		res.Content[0].Text != "截断" ||
		res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 128 {
		t.Fatalf("incomplete parse = %+v", res)
	}
	res, err = ParseUpstreamBody([]byte(`{"id":"resp_x","status":"incomplete",
	  "incomplete_details":{"reason":"content_filter"},"model":"m","output":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.FinishReason != ir.FinishContentFilter {
		t.Fatalf("content_filter finish = %q", res.FinishReason)
	}
}

func TestParseUpstreamBodyErrors(t *testing.T) {
	// Bodies copied verbatim from captured upstream responses plus the
	// failed-response shape.
	tests := []struct {
		name       string
		body       string
		wantKind   string
		wantStatus int
		retryable  bool
	}{
		{
			name:       "upstream invalid key 401",
			body:       `{"error": {"code": "", "message": "无效的令牌 (request id: 202610280744446814284898268d9d6d8IB5Jhx)", "type": "new_api_error"}}`,
			wantKind:   ir.ErrInvalidKey,
			wantStatus: 401,
		},
		{
			name:       "upstream model forbidden 403",
			body:       `{"error": {"code": "", "message": "该令牌无权访问模型 nonexistent-model-xyz (request id: 2026102807444460800504682694d6t83qwcu4)", "type": "new_api_error"}}`,
			wantKind:   ir.ErrInvalidKey,
			wantStatus: 403,
		},
		{
			name:       "upstream missing field 400",
			body:       `{"error": {"message": "field messages is required (request id: 202610280745215914284898268d9d6dJbo0KEpz)", "type": "new_api_error", "param": "", "code": "invalid_request"}}`,
			wantKind:   ir.ErrUpstream,
			wantStatus: 400,
		},
		{
			name:       "upstream invalid json 400 (no code)",
			body:       `{"error": {"code": "", "message": "无效的请求，invalid JSON request body (request id: 202610280745216322209218268d9d6G7oXo0s8)", "type": "new_api_error"}}`,
			wantKind:   ir.ErrUpstream,
			wantStatus: 400,
		},
		{
			name:       "upstream context length",
			body:       `{"error": {"code": "", "message": "This model's maximum context length is 8192 tokens, however you requested 20000 tokens", "type": "new_api_error"}}`,
			wantKind:   ir.ErrContextLength,
			wantStatus: 400,
		},
		{
			name:       "new_api_error top-level variant",
			body:       `{"new_api_error": {"message": "无效的令牌", "type": "new_api_error"}}`,
			wantKind:   ir.ErrInvalidKey,
			wantStatus: 401,
		},
		{
			name:       "status failed response object",
			body:       `{"id":"resp_x","object":"response","status":"failed","error":{"code":"server_error","message":"The model is overloaded"},"model":"m","output":[]}`,
			wantKind:   ir.ErrUpstream,
			wantStatus: 502,
			retryable:  true,
		},
		{
			name:       "unknown envelope falls back to retryable upstream error",
			body:       `{"error":{"message":"something odd","type":"weird"}}`,
			wantKind:   ir.ErrUpstream,
			wantStatus: 502,
			retryable:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseUpstreamBody([]byte(tt.body))
			if err == nil {
				t.Fatal("expected error")
			}
			var ue *ir.UpstreamError
			if !errors.As(err, &ue) {
				t.Fatalf("error %v is not *ir.UpstreamError", err)
			}
			if ue.Kind != tt.wantKind || ue.Status != tt.wantStatus || ue.Retryable != tt.retryable {
				t.Fatalf("got kind=%q status=%d retryable=%v, want kind=%q status=%d retryable=%v",
					ue.Kind, ue.Status, ue.Retryable, tt.wantKind, tt.wantStatus, tt.retryable)
			}
			if ue.Message == "" {
				t.Fatal("empty message")
			}
		})
	}

	if _, err := ParseUpstreamBody([]byte("<html>gateway timeout</html>")); err == nil {
		t.Fatal("non-JSON body must error")
	}
	if _, err := ParseUpstreamBody([]byte(`{"ok":true}`)); err == nil {
		t.Fatal("non-response JSON body must error")
	}
}

// ---- UpstreamStreamParser ----

func feedLines(t *testing.T, p *UpstreamStreamParser, raw string) []ir.StreamEvent {
	t.Helper()
	var evs []ir.StreamEvent
	for _, line := range strings.Split(raw, "\n") {
		got, err := p.Line(line)
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		evs = append(evs, got...)
	}
	return evs
}

func countKinds(evs []ir.StreamEvent) map[ir.StreamKind]int {
	c := map[ir.StreamKind]int{}
	for _, ev := range evs {
		c[ev.Kind]++
	}
	return c
}

// The real upstream stream (46 SSE events, testdata resp_stream.txt): created →
// 27 thinking deltas → 6 text deltas → completed with usage.
func TestUpstreamStreamParserGolden(t *testing.T) {
	evs := feedLines(t, NewUpstreamStreamParser(), readTestdata(t, "resp_stream.txt"))

	want := map[ir.StreamKind]int{
		ir.EvStart:         1,
		ir.EvThinkingDelta: 27,
		ir.EvTextDelta:     6,
		ir.EvUsage:         1,
		ir.EvFinish:        1,
	}
	if got := countKinds(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("event census = %v, want %v", got, want)
	}
	if evs[0].Kind != ir.EvStart || evs[0].Model != "test-model" {
		t.Fatalf("first event = %+v", evs[0])
	}
	// all thinking deltas precede all text deltas
	lastThink, firstText := -1, -1
	for i, ev := range evs {
		if ev.Kind == ir.EvThinkingDelta {
			lastThink = i
		}
		if ev.Kind == ir.EvTextDelta && firstText < 0 {
			firstText = i
		}
	}
	if !(lastThink < firstText) {
		t.Fatalf("thinking/text interleave: lastThink=%d firstText=%d", lastThink, firstText)
	}
	// joined thinking text equals the golden's own done text
	names, datas := parseSSE(t, readTestdata(t, "resp_stream.txt"))
	wantThink, _ := lastData(t, names, datas, "response.reasoning_summary_text.done")["text"].(string)
	var gotThink strings.Builder
	for _, ev := range evs {
		if ev.Kind == ir.EvThinkingDelta {
			gotThink.WriteString(ev.Text)
		}
	}
	if gotThink.String() != wantThink {
		t.Fatalf("joined thinking = %q, want %q", gotThink.String(), wantThink)
	}
	// terminal pair: usage then finish(stop)
	if evs[len(evs)-2].Kind != ir.EvUsage || evs[len(evs)-1].Kind != ir.EvFinish ||
		evs[len(evs)-1].Finish != ir.FinishStop {
		t.Fatalf("terminal events = %+v %+v", evs[len(evs)-2], evs[len(evs)-1])
	}
	wantUsage := ir.Usage{InputTokens: 34, OutputTokens: 90, ReasoningTokens: 71}
	if evs[len(evs)-2].Usage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", evs[len(evs)-2].Usage, wantUsage)
	}
}

// No golden sample exists for streamed tool calls, so testdata carries a
// stream built from the official event shapes (item ids fc_…, call ids
// call_00_…, arguments.done followed by output_item.done — the parser must
// close the tool exactly once).
func TestUpstreamStreamParserToolCall(t *testing.T) {
	evs := feedLines(t, NewUpstreamStreamParser(), readTestdata(t, "resp_tools_stream.txt"))
	want := []ir.StreamEvent{
		{Kind: ir.EvStart, Model: "test-model"},
		{Kind: ir.EvThinkingDelta, Text: "用户问北京天气。调用工具。"},
		{Kind: ir.EvToolStart, ToolID: "call_00_testtool00000000000001", ToolName: "get_weather"},
		{Kind: ir.EvToolDelta, ToolID: "call_00_testtool00000000000001", Text: `{"city"`},
		{Kind: ir.EvToolDelta, ToolID: "call_00_testtool00000000000001", Text: `: "北京"}`},
		{Kind: ir.EvToolEnd, ToolID: "call_00_testtool00000000000001"},
		{Kind: ir.EvUsage, Usage: ir.Usage{InputTokens: 302, OutputTokens: 47, CacheReadTokens: 256, ReasoningTokens: 8}},
		{Kind: ir.EvFinish, Finish: ir.FinishToolCalls},
	}
	if !reflect.DeepEqual(evs, want) {
		t.Fatalf("events mismatch:\n got %+v\nwant %+v", evs, want)
	}
}

func TestUpstreamStreamParserVariants(t *testing.T) {
	t.Run("data-only frames and lazy start", func(t *testing.T) {
		evs := feedLines(t, NewUpstreamStreamParser(),
			`data: {"type":"response.output_text.delta","delta":"你好","sequence_number":0}`)
		want := []ir.StreamEvent{{Kind: ir.EvStart}, {Kind: ir.EvTextDelta, Text: "你好"}}
		if !reflect.DeepEqual(evs, want) {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("incomplete finishes length", func(t *testing.T) {
		evs := feedLines(t, NewUpstreamStreamParser(), strings.Join([]string{
			`event: response.created`,
			`data: {"response":{"model":"m"},"type":"response.created","sequence_number":0}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"截","sequence_number":1}`,
			``,
			`event: response.incomplete`,
			`data: {"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}},"type":"response.incomplete","sequence_number":2}`,
			``,
		}, "\n"))
		if len(evs) != 4 || evs[3].Kind != ir.EvFinish || evs[3].Finish != ir.FinishLength ||
			evs[2].Kind != ir.EvUsage || evs[2].Usage.InputTokens != 5 {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("failed classifies from error payload", func(t *testing.T) {
		evs := feedLines(t, NewUpstreamStreamParser(), strings.Join([]string{
			`event: response.created`,
			`data: {"response":{"model":"m"},"type":"response.created"}`,
			``,
			`event: response.failed`,
			`data: {"response":{"status":"failed","error":{"code":"server_error","message":"upstream exploded"}},"type":"response.failed"}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"late"}`,
		}, "\n"))
		if len(evs) != 2 || evs[1].Kind != ir.EvError {
			t.Fatalf("events = %+v", evs)
		}
		ev := evs[1]
		if ev.ErrCode != ir.ErrUpstream || ev.ErrMsg != "upstream exploded" || !ev.Retryable || ev.HTTPStatus != 502 {
			t.Fatalf("error event = %+v", ev)
		}
	})
	t.Run("bare error frame classifies rate limit", func(t *testing.T) {
		evs := feedLines(t, NewUpstreamStreamParser(),
			`data: {"type":"response.error","code":"rate_limited","message":"slow down"}`)
		if len(evs) != 1 || evs[0].Kind != ir.EvError || evs[0].ErrCode != ir.ErrRateLimited ||
			!evs[0].Retryable || evs[0].HTTPStatus != 429 {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("tool left open is closed at completed", func(t *testing.T) {
		evs := feedLines(t, NewUpstreamStreamParser(), strings.Join([]string{
			`event: response.output_item.added`,
			`data: {"output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"c1","name":"fa","arguments":""},"type":"response.output_item.added"}`,
			``,
			`data: {"item_id":"fc_1","delta":"{}","type":"response.function_call_arguments.delta"}`,
			``,
			`event: response.completed`,
			`data: {"response":{"status":"completed","model":"m","usage":{"input_tokens":1,"output_tokens":1}},"type":"response.completed"}`,
			``,
		}, "\n"))
		kinds := countKinds(evs)
		if kinds[ir.EvToolEnd] != 1 {
			t.Fatalf("tool end count = %d, events = %+v", kinds[ir.EvToolEnd], evs)
		}
		// order: start, tool_start, tool_delta, tool_end, usage, finish(tool_calls)
		if len(evs) != 6 || evs[0].Kind != ir.EvStart || evs[3].Kind != ir.EvToolEnd ||
			evs[4].Kind != ir.EvUsage || evs[5].Kind != ir.EvFinish || evs[5].Finish != ir.FinishToolCalls {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("DONE sentinel closes open tools and ends the stream", func(t *testing.T) {
		p := NewUpstreamStreamParser()
		evs := feedLines(t, p, strings.Join([]string{
			`event: response.output_item.added`,
			`data: {"output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"c1","name":"fa"},"type":"response.output_item.added"}`,
			``,
			`data: [DONE]`,
		}, "\n"))
		// lazy EvStart is synthesized on the tool start (no response.created)
		if len(evs) != 3 || evs[2].Kind != ir.EvToolEnd || evs[2].ToolID != "c1" {
			t.Fatalf("events = %+v", evs)
		}
		if more, _ := p.Line(`data: {"type":"response.output_text.delta","delta":"x"}`); len(more) != 0 {
			t.Fatalf("events after DONE = %+v", more)
		}
	})
	t.Run("malformed frames and comments tolerated", func(t *testing.T) {
		p := NewUpstreamStreamParser()
		evs := feedLines(t, p, strings.Join([]string{
			`: ping`,
			``,
			`data: {not json}`,
			``,
			`event: response.output_text.delta`,
			`data: {"item_id":"msg_1","delta":"ok"}`, // type from event: line
			``,
		}, "\n"))
		// lazy EvStart synthesized at the first delta
		want := []ir.StreamEvent{{Kind: ir.EvStart}, {Kind: ir.EvTextDelta, Text: "ok"}}
		if !reflect.DeepEqual(evs, want) {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("call_id used as item_id is accepted", func(t *testing.T) {
		evs := feedLines(t, NewUpstreamStreamParser(), strings.Join([]string{
			`event: response.output_item.added`,
			`data: {"output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"c1","name":"fa"},"type":"response.output_item.added"}`,
			``,
			`data: {"item_id":"c1","delta":"{}","type":"response.function_call_arguments.delta"}`,
			``,
			`data: {"item_id":"c1","arguments":"{}","type":"response.function_call_arguments.done"}`,
			``,
		}, "\n"))
		// lazy EvStart synthesized; delta and end both resolve c1 by call_id
		if len(evs) != 4 || evs[2].Kind != ir.EvToolDelta || evs[2].ToolID != "c1" ||
			evs[3].Kind != ir.EvToolEnd || evs[3].ToolID != "c1" {
			t.Fatalf("events = %+v", evs)
		}
	})
}

// End-to-end within the package: parse a golden upstream body, stream it back
// through the client encoder, and confirm the SSE shape the gateway would
// serve a Responses client.
func TestUpstreamBodyToClientStream(t *testing.T) {
	res, err := ParseUpstreamBody([]byte(readTestdata(t, "resp_tools.json")))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	enc := NewClientStreamEncoder(rec)
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EvStart, Model: res.Model},
		{Kind: ir.EvThinkingDelta, Text: res.Content[0].Text},
		{Kind: ir.EvToolStart, ToolID: res.Content[1].ToolCallID, ToolName: res.Content[1].ToolName},
		{Kind: ir.EvToolDelta, ToolID: res.Content[1].ToolCallID, Text: res.Content[1].ToolArgsJSON},
		{Kind: ir.EvToolEnd, ToolID: res.Content[1].ToolCallID},
		{Kind: ir.EvUsage, Usage: res.Usage},
	} {
		if err := enc.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(res.FinishReason); err != nil {
		t.Fatal(err)
	}
	names, datas := parseSSE(t, rec.Body.String())
	if names[0] != "response.created" || names[len(names)-1] != "response.completed" {
		t.Fatalf("names = %v", names)
	}
	resp := datas[len(datas)-1]["response"].(map[string]any)
	out := resp["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["type"] != "reasoning" ||
		out[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("completed output = %v", out)
	}
	if resp["usage"].(map[string]any)["input_tokens"] != float64(302) {
		t.Fatalf("usage = %v", resp["usage"])
	}
}
