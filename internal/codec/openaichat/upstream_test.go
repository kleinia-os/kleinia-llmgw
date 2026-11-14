package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

// ---- ParseUpstreamBody against golden response bodies ----

func TestParseUpstreamBodyGolden(t *testing.T) {
	res, err := ParseUpstreamBody(load(t, "chat_plain.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "test-model" {
		t.Errorf("model: %q", res.Model)
	}
	if res.FinishReason != ir.FinishLength {
		t.Errorf("finish: %q", res.FinishReason)
	}
	wantUsage := ir.Usage{InputTokens: 34, OutputTokens: 128, CacheReadTokens: 0, ReasoningTokens: 121}
	if res.Usage != wantUsage {
		t.Errorf("usage: %+v want %+v", res.Usage, wantUsage)
	}
	if len(res.Content) != 2 || res.Content[0].Type != ir.PartThinking || res.Content[1].Type != ir.PartText ||
		res.Content[1].Text != "我是 TestBot，一个由 TestLabs" {
		t.Errorf("content: %+v", res.Content)
	}

	res, err = ParseUpstreamBody(load(t, "chat_reasoning.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.FinishReason != ir.FinishStop || res.Content[1].Text != "25" {
		t.Errorf("finish=%q content=%+v", res.FinishReason, res.Content)
	}
	wantUsage = ir.Usage{InputTokens: 40, OutputTokens: 25, ReasoningTokens: 23}
	if res.Usage != wantUsage {
		t.Errorf("usage: %+v want %+v", res.Usage, wantUsage)
	}

	res, err = ParseUpstreamBody(load(t, "chat_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.FinishReason != ir.FinishToolCalls {
		t.Errorf("finish: %q", res.FinishReason)
	}
	wantUsage = ir.Usage{InputTokens: 302, OutputTokens: 49, ReasoningTokens: 10}
	if res.Usage != wantUsage {
		t.Errorf("usage: %+v want %+v", res.Usage, wantUsage)
	}
	var use *ir.Part
	for i := range res.Content {
		if res.Content[i].Type == ir.PartToolUse {
			use = &res.Content[i]
		}
	}
	if use == nil || use.ToolCallID != "call_00_94091ex7gxe95toioqwsoltl" || use.ToolName != "get_weather" ||
		use.ToolArgsJSON != `{"city": "北京"}` {
		t.Errorf("tool_use part: %+v", use)
	}
}

// ---- BuildUpstreamRequest ----

func buildForTest(t *testing.T, req ir.Request, stream bool) (*upBody, map[string]json.RawMessage) {
	t.Helper()
	hreq, err := BuildUpstreamRequest(context.Background(), "https://up.test/v1/", "sk-test", "test-model", req, stream)
	if err != nil {
		t.Fatal(err)
	}
	if hreq.Method != "POST" {
		t.Errorf("method: %s", hreq.Method)
	}
	if got := hreq.URL.String(); got != "https://up.test/v1/chat/completions" {
		t.Errorf("url: %s", got)
	}
	if got := hreq.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("authorization: %q", got)
	}
	if got := hreq.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type: %q", got)
	}
	body, err := io.ReadAll(hreq.Body)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("upstream body not JSON: %v\n%s", err, body)
	}
	var b upBody
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	return &b, raw
}

func TestBuildUpstreamRequestNonStream(t *testing.T) {
	req, err := ParseClientRequest(load(t, "request_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, raw := buildForTest(t, req, false)
	if b.Model != "test-model" {
		t.Errorf("model: %q", b.Model)
	}
	if b.Stream || raw["stream"] != nil {
		t.Errorf("stream should be omitted when false: raw=%v", raw["stream"])
	}
	if raw["stream_options"] != nil {
		t.Error("stream_options must be omitted for non-streaming")
	}
	if b.MaxTokens != 512 {
		t.Errorf("max_tokens: %d", b.MaxTokens)
	}
	if b.Temperature == nil || *b.Temperature != 0.2 || b.TopP == nil || *b.TopP != 0.9 ||
		b.TopK == nil || *b.TopK != 50 {
		t.Errorf("sampling: %+v", b)
	}
	if len(b.Messages) != 4 {
		t.Fatalf("messages: %d", len(b.Messages))
	}
	if b.Messages[0].Role != "system" || b.Messages[0].Content != "你是天气助手" {
		t.Errorf("system msg: %+v", b.Messages[0])
	}
	as := b.Messages[2]
	if as.Role != "assistant" || as.ReasoningContent != "We need to call the weather tool for Beijing." ||
		len(as.ToolCalls) != 1 || as.ToolCalls[0].ID != "call_00_94091ex7gxe95toioqwsoltl" ||
		as.ToolCalls[0].Function.Name != "get_weather" ||
		as.ToolCalls[0].Function.Arguments != `{"city": "北京"}` {
		t.Errorf("assistant msg: %+v", as)
	}
	tm := b.Messages[3]
	if tm.Role != "tool" || tm.ToolCallID != "call_00_94091ex7gxe95toioqwsoltl" || tm.Content != `{"temp_c": 18}` {
		t.Errorf("tool msg: %+v", tm)
	}
	if len(b.Tools) != 1 || b.Tools[0].Function.Name != "get_weather" || b.Tools[0].Type != "function" {
		t.Errorf("tools: %+v", b.Tools)
	}
	var schema any
	if err := json.Unmarshal(b.Tools[0].Function.Parameters, &schema); err != nil {
		t.Errorf("tool parameters: %v", err)
	}
	var tc upToolChoice
	if err := json.Unmarshal(raw["tool_choice"], &tc); err != nil || tc.Type != "function" ||
		tc.Function == nil || tc.Function.Name != "get_weather" {
		// OpenAI wire: {"type":"function","function":{"name":...}} — "tool"
		// is the Anthropic spelling and upstreams reject it (400).
		t.Errorf("tool_choice: %s err=%v", raw["tool_choice"], err)
	}
	if b.ResponseFormat == nil || b.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format: %+v", b.ResponseFormat)
	}
	// Raw pass-through (same-format): unknown client fields ride along.
	if raw["frequency_penalty"] == nil || raw["user"] == nil {
		t.Errorf("raw fields not passed through: %v", raw)
	}
}

func TestBuildUpstreamRequestStream(t *testing.T) {
	req, err := ParseClientRequest(load(t, "request_plain.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := buildForTest(t, req, true)
	if !b.Stream {
		t.Error("stream must be true")
	}
	if b.StreamOptions == nil || !b.StreamOptions.IncludeUsage {
		t.Errorf("stream_options: %+v", b.StreamOptions)
	}
	hreq, err := BuildUpstreamRequest(context.Background(), "https://up.test/v1", "k", "m", req, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := hreq.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("accept: %q", got)
	}
}

func TestBuildUpstreamRequestOmitsUnsetMaxTokens(t *testing.T) {
	req := ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Part{{Type: ir.PartText, Text: "hi"}}}}}
	_, raw := buildForTest(t, req, false)
	if raw["max_tokens"] != nil {
		t.Errorf("max_tokens must be omitted when unset: %s", raw["max_tokens"])
	}
	if raw["tools"] != nil || raw["tool_choice"] != nil || raw["temperature"] != nil {
		t.Errorf("unset optional fields leaked: %v", raw)
	}
}

// A RoleTool message holding several results must fan out into several
// role:"tool" wire messages (one tool_call_id each).
func TestBuildUpstreamRequestToolArrayization(t *testing.T) {
	req := ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Part{
			{Type: ir.PartToolUse, ToolCallID: "a", ToolName: "f", ToolArgsJSON: "{}"},
			{Type: ir.PartToolUse, ToolCallID: "b", ToolName: "g", ToolArgsJSON: "{}"},
		}},
		{Role: ir.RoleTool, Content: []ir.Part{
			{Type: ir.PartToolResult, ToolCallID: "a", ToolOutput: "1"},
			{Type: ir.PartToolResult, ToolCallID: "b", ToolOutput: "2"},
		}},
	}}
	b, _ := buildForTest(t, req, false)
	if len(b.Messages) != 3 {
		t.Fatalf("messages: %d (%+v)", len(b.Messages), b.Messages)
	}
	if b.Messages[1].Role != "tool" || b.Messages[1].ToolCallID != "a" || b.Messages[1].Content != "1" {
		t.Errorf("tool msg 1: %+v", b.Messages[1])
	}
	if b.Messages[2].Role != "tool" || b.Messages[2].ToolCallID != "b" || b.Messages[2].Content != "2" {
		t.Errorf("tool msg 2: %+v", b.Messages[2])
	}
}

func TestBuildUpstreamRequestMultimodal(t *testing.T) {
	req, err := ParseClientRequest(load(t, "request_multimodal.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := buildForTest(t, req, true)
	// Content is decoded as generic any; re-check the shape via JSON.
	jb, err := json.Marshal(b.Messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	var parts []upContentPart
	if err := json.Unmarshal(jb, &parts); err != nil {
		t.Fatalf("content should be a parts array: %v (%s)", err, jb)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "这张图里是什么？" ||
		parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/cat.png" {
		t.Errorf("parts: %+v", parts)
	}
}

// ---- error classification (golden envelopes captured from an upstream) ----

func TestParseUpstreamBodyErrors(t *testing.T) {
	cases := []struct {
		file      string
		kind      string
		retryable bool
	}{
		{"err_403_model.json", ir.ErrInvalidKey, false},         // upstream 403: model unknown AND unauthorized share this shape
		{"err_401_key.json", ir.ErrInvalidKey, false},           // upstream 401
		{"err_400_invalid_request.json", ir.ErrUpstream, false}, // code:"invalid_request" — our request is bad
		{"err_400_badjson_body.json", ir.ErrUpstream, true},     // generic envelope, server stamps status
		{"err_400_context.json", ir.ErrContextLength, false},    // OpenAI-style context overflow
		{"err_429_rate.json", ir.ErrRateLimited, true},
		{"err_500_generic.json", ir.ErrUpstream, true},
	}
	for _, c := range cases {
		_, err := ParseUpstreamBody(load(t, c.file))
		var ue *ir.UpstreamError
		if !errors.As(err, &ue) {
			t.Errorf("%s: got %v, want *ir.UpstreamError", c.file, err)
			continue
		}
		if ue.Kind != c.kind {
			t.Errorf("%s: kind = %q, want %q (msg %q)", c.file, ue.Kind, c.kind, ue.Message)
		}
		if ue.Retryable != c.retryable {
			t.Errorf("%s: retryable = %v, want %v", c.file, ue.Retryable, c.retryable)
		}
		if ue.Message == "" {
			t.Errorf("%s: empty message", c.file)
		}
	}
}

func TestParseUpstreamBodyAlternateEnvelope(t *testing.T) {
	body := []byte(`{"new_api_error":{"message":"无效的令牌 (request id: x)","type":"new_api_error"}}`)
	_, err := ParseUpstreamBody(body)
	var ue *ir.UpstreamError
	if !errors.As(err, &ue) || ue.Kind != ir.ErrInvalidKey {
		t.Errorf("new_api_error envelope: %v", err)
	}
}

func TestParseUpstreamBodyBadBody(t *testing.T) {
	for name, body := range map[string][]byte{
		"html":  load(t, "err_badbody.txt"),
		"json":  []byte(`{"foo": 1}`),
		"empty": nil,
	} {
		_, err := ParseUpstreamBody(body)
		var ue *ir.UpstreamError
		if !errors.As(err, &ue) {
			t.Errorf("%s: got %v, want *ir.UpstreamError", name, err)
			continue
		}
		if ue.Kind != ir.ErrUpstream || !ue.Retryable {
			t.Errorf("%s: %+v", name, ue)
		}
		if len(ue.Message) > 220 { // 200-byte snippet plus error prefix slack
			t.Errorf("%s: snippet too long (%d)", name, len(ue.Message))
		}
	}
	// long garbage must be truncated
	long := bytes.Repeat([]byte("x"), 5000)
	_, err := ParseUpstreamBody(long)
	var ue *ir.UpstreamError
	if !errors.As(err, &ue) || len(ue.Message) > 220 {
		t.Errorf("truncation: %v", err)
	}
}

// ---- request round trip: client wire -> IR -> upstream wire -> IR ----

func TestRequestRoundTrip(t *testing.T) {
	req1, err := ParseClientRequest(load(t, "request_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	hreq, err := BuildUpstreamRequest(context.Background(), "https://up.test/v1", "sk", "ds-flash", req1, false)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(hreq.Body)
	req2, err := ParseClientRequest(body)
	if err != nil {
		t.Fatalf("re-parse upstream body: %v\n%s", err, body)
	}
	if req2.Model != req1.Model || len(req2.Messages) != len(req1.Messages) {
		t.Fatalf("shape: %+v vs %+v", req1, req2)
	}
	for i := range req1.Messages {
		if req1.Messages[i].Role != req2.Messages[i].Role {
			t.Errorf("msg %d role: %q vs %q", i, req1.Messages[i].Role, req2.Messages[i].Role)
		}
		for j := range req1.Messages[i].Content {
			a, b := req1.Messages[i].Content[j], req2.Messages[i].Content[j]
			if a.Type != b.Type || a.Text != b.Text || a.ToolCallID != b.ToolCallID ||
				a.ToolName != b.ToolName || a.ToolArgsJSON != b.ToolArgsJSON || a.ToolOutput != b.ToolOutput ||
				a.ImageURL != b.ImageURL {
				t.Errorf("msg %d part %d:\n got %+v\nwant %+v", i, j, b, a)
			}
		}
	}
	if len(req2.Tools) != 1 || req2.Tools[0].Name != "get_weather" {
		t.Fatalf("tools: %+v", req2.Tools)
	}
	var s1, s2 any
	json.Unmarshal(req1.Tools[0].ParametersJSON, &s1)
	json.Unmarshal(req2.Tools[0].ParametersJSON, &s2)
	if !reflect.DeepEqual(s1, s2) {
		t.Errorf("parameters: %v vs %v", s2, s1)
	}
	if req2.ToolChoice == nil || *req2.ToolChoice != *req1.ToolChoice {
		t.Errorf("tool_choice: %+v vs %+v", req2.ToolChoice, req1.ToolChoice)
	}
	if !reflect.DeepEqual(req2.Sampling.Stop, req1.Sampling.Stop) ||
		req2.Sampling.MaxTokens != req1.Sampling.MaxTokens ||
		*req2.Sampling.Temperature != *req1.Sampling.Temperature ||
		*req2.Sampling.TopK != *req1.Sampling.TopK {
		t.Errorf("sampling: %+v vs %+v", req2.Sampling, req1.Sampling)
	}
	if req2.ResponseFormat == nil || *req2.ResponseFormat != *req1.ResponseFormat {
		t.Errorf("response_format: %+v", req2.ResponseFormat)
	}
	if req2.Raw["frequency_penalty"] == nil || req2.Raw["user"] == nil {
		t.Errorf("raw passthrough lost fields: %v", req2.Raw)
	}
}
