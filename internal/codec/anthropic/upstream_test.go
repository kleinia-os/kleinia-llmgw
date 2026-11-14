package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

// clientRoundtripBody mirrors the anthropic request shape used in the
// round-trip sample: system block array, assistant tool_use turn, and a
// user turn mixing a tool_result block with text, plus thinking/metadata
// extras the IR does not model.
const clientRoundtripBody = `{
  "model": "test-model",
  "max_tokens": 128,
  "temperature": 0,
  "system": [{"type":"text","text":"You are Klein."},{"type":"text","text":"Answer in Chinese."}],
  "tools": [{"name":"get_weather","description":"查询城市天气",
             "input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
  "thinking": {"type":"disabled"},
  "metadata": {"user_id":"klein-1"},
  "messages": [
    {"role":"user","content":"北京现在天气怎么样？"},
    {"role":"assistant","content":[
      {"type":"thinking","thinking":"先调工具"},
      {"type":"tool_use","id":"call_00_3ns4lald22nukgy9dkeu1mcl","name":"get_weather","input":{"city": "北京"}}
    ]},
    {"role":"user","content":[
      {"type":"tool_result","tool_use_id":"call_00_3ns4lald22nukgy9dkeu1mcl","content":"{\"temp_c\":18,\"cond\":\"晴\"}"},
      {"type":"text","text":"用中文简短回答"}
    ]}
  ]
}`

func TestBuildUpstreamRequestRoundtrip(t *testing.T) {
	req, err := ParseClientRequest([]byte(clientRoundtripBody))
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
	if got := hreq.URL.String(); got != "https://upstream.example.test/v1/messages" {
		t.Fatalf("url = %s", got)
	}
	h := hreq.Header
	if h.Get("x-api-key") != "sk-test" {
		t.Fatalf("x-api-key = %q", h.Get("x-api-key"))
	}
	if v := h.Get("anthropic-version"); v != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", v)
	}
	if ct := h.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if acc := h.Get("Accept"); acc != "application/json" {
		t.Fatalf("accept = %q", acc)
	}
	body, err := io.ReadAll(hreq.Body)
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Model       string   `json:"model"`
		System      string   `json:"system"`
		MaxTokens   int64    `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
		Stream      bool     `json:"stream"`
		Messages    []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		Thinking json.RawMessage `json:"thinking"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("body: %v\n%s", err, body)
	}
	if b.Model != "ds-flash-upstream" {
		t.Fatalf("model = %q, want resolved upstream name", b.Model)
	}
	if b.System != "You are Klein.\nAnswer in Chinese." {
		t.Fatalf("system = %q", b.System)
	}
	if b.MaxTokens != 128 || b.Temperature == nil || *b.Temperature != 0 {
		t.Fatalf("max_tokens=%d temperature=%v", b.MaxTokens, b.Temperature)
	}
	if b.Stream {
		t.Fatal("stream must be omitted for non-streaming")
	}
	if len(b.Messages) != 3 {
		t.Fatalf("messages = %s", body)
	}
	// message 1: plain user text
	if b.Messages[0].Role != "user" {
		t.Fatalf("role[0] = %s", b.Messages[0].Role)
	}
	var firstBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b.Messages[0].Content[0], &firstBlock); err != nil ||
		firstBlock.Type != "text" || firstBlock.Text != "北京现在天气怎么样？" {
		t.Fatalf("messages[0] = %s", b.Messages[0].Content[0])
	}
	// message 2: assistant tool_use, thinking dropped (no signature to replay)
	if b.Messages[1].Role != "assistant" || len(b.Messages[1].Content) != 1 {
		t.Fatalf("messages[1] = %s", body)
	}
	var tool toolUseBlock
	if err := json.Unmarshal(b.Messages[1].Content[0], &tool); err != nil {
		t.Fatal(err)
	}
	if tool.Type != "tool_use" || tool.ID != "call_00_3ns4lald22nukgy9dkeu1mcl" ||
		tool.Name != "get_weather" || string(tool.Input) != `{"city":"北京"}` {
		t.Fatalf("tool_use block = %+v", tool)
	}
	// message 3: tool_result blocks lead the merged user turn, then text
	if b.Messages[2].Role != "user" || len(b.Messages[2].Content) != 2 {
		t.Fatalf("messages[2] = %s", body)
	}
	var result toolResultBlock
	if err := json.Unmarshal(b.Messages[2].Content[0], &result); err != nil {
		t.Fatal(err)
	}
	if result.Type != "tool_result" || result.ToolUseID != "call_00_3ns4lald22nukgy9dkeu1mcl" ||
		result.Content != `{"temp_c":18,"cond":"晴"}` {
		t.Fatalf("tool_result block = %+v", result)
	}
	var trailing textBlock
	if err := json.Unmarshal(b.Messages[2].Content[1], &trailing); err != nil ||
		trailing.Text != "用中文简短回答" {
		t.Fatalf("trailing text = %s", b.Messages[2].Content[1])
	}
	// tools carry input_schema verbatim
	if len(b.Tools) != 1 || b.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", b.Tools)
	}
	var schema struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b.Tools[0].InputSchema, &schema); err != nil || schema.Type != "object" {
		t.Fatalf("input_schema = %s", b.Tools[0].InputSchema)
	}
	// unknown extras pass through
	var thinking struct {
		Type string `json:"type"`
	}
	if len(b.Thinking) == 0 || json.Unmarshal(b.Thinking, &thinking) != nil || thinking.Type != "disabled" {
		t.Fatalf("thinking passthrough = %s", b.Thinking)
	}
	if string(b.Metadata) != `{"user_id":"klein-1"}` {
		t.Fatalf("metadata passthrough = %s", b.Metadata)
	}
}

func TestBuildUpstreamRequestStreamAndDirect(t *testing.T) {
	req, err := ParseClientRequest([]byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`))
	if err != nil {
		t.Fatal(err)
	}
	hreq, err := BuildUpstreamRequest(context.Background(), "https://upstream.example.test/v1", "k", "up-model", req, true)
	if err != nil {
		t.Fatal(err)
	}
	if acc := hreq.Header.Get("Accept"); acc != "text/event-stream" {
		t.Fatalf("accept = %q", acc)
	}
	body, _ := io.ReadAll(hreq.Body)
	if !strings.Contains(string(body), `"stream":true`) {
		t.Fatalf("stream flag missing: %s", body)
	}
	if !strings.Contains(string(body), `"thinking"`) {
		t.Fatalf("thinking passthrough missing: %s", body)
	}

	// hand-built IR: max_tokens default, stop sequences, tool choice,
	// trailing RoleTool synthesis, image round-trip, Raw collision guard.
	temp := 0.2
	var mkp int64 = 40
	req = ir.Request{
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Part{
				{Type: ir.PartImage, ImageURL: "data:image/png;base64,aGk="},
				{Type: ir.PartImage, ImageURL: "https://cdn.example/p.png"},
				{Type: ir.PartText, Text: "look"},
			}},
			{Role: ir.RoleTool, Content: []ir.Part{
				{Type: ir.PartToolResult, ToolCallID: "c1", ToolOutput: "oops", IsError: true},
			}},
		},
		Sampling: ir.Sampling{Temperature: &temp, TopK: &mkp, Stop: []string{"END"}},
	}
	hreq, err = BuildUpstreamRequest(context.Background(), "https://x", "k", "m", req, false)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(hreq.Body)
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatal(err)
	}
	if generic["max_tokens"].(float64) != 4096 {
		t.Fatalf("max_tokens = %v, want default 4096", generic["max_tokens"])
	}
	if _, ok := generic["stop_sequences"]; !ok {
		t.Fatal("stop_sequences missing")
	}
	msgs := generic["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %s", body)
	}
	last := msgs[1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("trailing tool message not synthesized as user: %s", body)
	}
	blocks := last["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("trailing user message = %s", body)
	}
	first := msgs[0].(map[string]any)["content"].([]any)
	img1 := first[0].(map[string]any)["source"].(map[string]any)
	if img1["type"] != "base64" || img1["media_type"] != "image/png" || img1["data"] != "aGk=" {
		t.Fatalf("base64 image = %v", img1)
	}
	img2 := first[1].(map[string]any)["source"].(map[string]any)
	if img2["type"] != "url" || img2["url"] != "https://cdn.example/p.png" {
		t.Fatalf("url image = %v", img2)
	}
	if first[2].(map[string]any)["type"] != "text" {
		t.Fatalf("first message blocks = %s", body)
	}
}

func TestBuildUpstreamRequestBodyEdge(t *testing.T) {
	// Raw keys colliding with owned body fields must not be passed through.
	req := ir.Request{
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Part{{Type: ir.PartText, Text: "hi"}}}},
		Raw: map[string]json.RawMessage{
			"max_tokens": json.RawMessage(`1`),
			"messages":   json.RawMessage(`[]`),
			"thinking":   json.RawMessage(`{"type":"enabled"}`),
			"junk":       json.RawMessage(`null`),
		},
	}
	hreq, err := BuildUpstreamRequest(context.Background(), "https://x", "k", "m", req, false)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(hreq.Body)
	if !strings.Contains(string(body), `"thinking":{"type":"enabled"}`) {
		t.Fatalf("thinking passthrough missing: %s", body)
	}
	if strings.Count(string(body), `"max_tokens"`) != 1 || strings.Count(string(body), `"messages"`) != 1 {
		t.Fatalf("owned field duplicated by passthrough: %s", body)
	}
	if strings.Contains(string(body), "junk") {
		t.Fatalf("null raw value leaked: %s", body)
	}

	// tool_choice mapping
	for _, tc := range []struct {
		choice ir.ToolChoice
		want   string
	}{
		{ir.ToolChoice{Type: ir.ToolChoiceAuto}, `{"type":"auto"}`},
		{ir.ToolChoice{Type: ir.ToolChoiceRequired}, `{"type":"any"}`},
		{ir.ToolChoice{Type: ir.ToolChoiceNamed, Name: "get_weather"}, `{"type":"tool","name":"get_weather"}`},
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

	// no user/assistant messages at all -> build error, not a broken request
	if _, err := BuildUpstreamRequest(context.Background(), "https://x", "k", "m", ir.Request{}, false); err == nil {
		t.Fatal("expected error for empty message list")
	}
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return data
}

func TestParseUpstreamBodyGolden(t *testing.T) {
	t.Run("anth_plain", func(t *testing.T) {
		res, err := ParseUpstreamBody(readGolden(t, "anth_plain.json"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Model != "test-model" || res.FinishReason != ir.FinishStop {
			t.Fatalf("model=%q finish=%q", res.Model, res.FinishReason)
		}
		if len(res.Content) != 2 || res.Content[0].Type != ir.PartThinking || res.Content[1].Type != ir.PartText {
			t.Fatalf("content = %+v", res.Content)
		}
		if !strings.HasPrefix(res.Content[0].Text, "我们需要回答中文。") {
			t.Fatalf("thinking = %q", res.Content[0].Text)
		}
		if res.Content[1].Text != "我是由TestBot开发的AI助手，能帮你解答问题、处理信息并完成各类任务。" {
			t.Fatalf("text = %q", res.Content[1].Text)
		}
		// input_tokens has no cache read here, so IR input == wire input.
		if res.Usage != (ir.Usage{InputTokens: 34, OutputTokens: 90, ReasoningTokens: 68}) {
			t.Fatalf("usage = %+v", res.Usage)
		}
	})
	t.Run("anth_tools", func(t *testing.T) {
		res, err := ParseUpstreamBody(readGolden(t, "anth_tools.json"))
		if err != nil {
			t.Fatal(err)
		}
		if res.FinishReason != ir.FinishToolCalls {
			t.Fatalf("finish = %q", res.FinishReason)
		}
		var tool *ir.Part
		for i := range res.Content {
			if res.Content[i].Type == ir.PartToolUse {
				tool = &res.Content[i]
			}
		}
		if tool == nil {
			t.Fatalf("no tool_use part: %+v", res.Content)
		}
		if tool.ToolCallID != "call_00_3ns4lald22nukgy9dkeu1mcl" || tool.ToolName != "get_weather" ||
			tool.ToolArgsJSON != `{"city":"北京"}` {
			t.Fatalf("tool part = %+v", tool)
		}
		// 46 wire input + 256 cache read = 302 IR input (matches the nested
		// openai prompt_tokens in the golden).
		want := ir.Usage{InputTokens: 302, OutputTokens: 53, CacheReadTokens: 256, ReasoningTokens: 14}
		if res.Usage != want {
			t.Fatalf("usage = %+v, want %+v", res.Usage, want)
		}
	})
	t.Run("anth_system", func(t *testing.T) {
		res, err := ParseUpstreamBody(readGolden(t, "anth_system.json"))
		if err != nil {
			t.Fatal(err)
		}
		if res.FinishReason != ir.FinishStop {
			t.Fatalf("finish = %q", res.FinishReason)
		}
		if len(res.Content) != 2 || res.Content[1].Text == "" || !strings.HasPrefix(res.Content[1].Text, "[Klein]") {
			t.Fatalf("content = %+v", res.Content)
		}
		if res.Usage != (ir.Usage{InputTokens: 71, OutputTokens: 102, ReasoningTokens: 73}) {
			t.Fatalf("usage = %+v", res.Usage)
		}
	})
	t.Run("anth_tools_roundtrip", func(t *testing.T) {
		res, err := ParseUpstreamBody(readGolden(t, "anth_tools_roundtrip.json"))
		if err != nil {
			t.Fatal(err)
		}
		if res.FinishReason != ir.FinishStop {
			t.Fatalf("finish = %q", res.FinishReason)
		}
		want := ir.Usage{InputTokens: 366, OutputTokens: 27, CacheReadTokens: 256}
		if res.Usage != want {
			t.Fatalf("usage = %+v, want %+v", res.Usage, want)
		}
		if !strings.Contains(res.Content[0].Text, "北京现在是晴天") {
			t.Fatalf("text = %q", res.Content[0].Text)
		}
	})
}

func TestParseUpstreamBodyErrors(t *testing.T) {
	// Bodies copied verbatim from captured upstream responses plus the
	// official anthropic envelope shapes.
	tests := []struct {
		name         string
		body         string
		wantKind     string
		wantStatus   int
		wantRetryble bool
	}{
		{
			name:       "upstream invalid key 401",
			body:       `{"error": {"code": "", "message": "无效的令牌 (request id: 202610280744446814284898268d9d6d8IB5Jhx)", "type": "new_api_error"}}`,
			wantKind:   ir.ErrInvalidKey,
			wantStatus: 401,
		},
		{
			name:       "upstream model forbidden 403",
			body:       `{"error": {"code": "", "message": "该令牌无权访问模型 nonexistent-model-xyz (request id: 202610280744446080050468268d9d6t83qwcu4)", "type": "new_api_error"}}`,
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
			name:       "upstream invalid json 400",
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
			name:       "anthropic authentication_error",
			body:       `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			wantKind:   ir.ErrInvalidKey,
			wantStatus: 401,
		},
		{
			name:         "anthropic rate_limit_error",
			body:         `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests too high"}}`,
			wantKind:     ir.ErrRateLimited,
			wantStatus:   429,
			wantRetryble: true,
		},
		{
			name:       "anthropic not_found_error",
			body:       `{"type":"error","error":{"type":"not_found_error","message":"model: not-found"}}`,
			wantKind:   ir.ErrNotFound,
			wantStatus: 404,
		},
		{
			name:         "anthropic overloaded_error",
			body:         `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantKind:     ir.ErrUpstream,
			wantStatus:   529,
			wantRetryble: true,
		},
		{
			name:         "unknown envelope falls back to retryable upstream error",
			body:         `{"error":{"message":"something odd","type":"weird"}}`,
			wantKind:     ir.ErrUpstream,
			wantStatus:   502,
			wantRetryble: true,
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
			if ue.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", ue.Kind, tt.wantKind)
			}
			if ue.Status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", ue.Status, tt.wantStatus)
			}
			if ue.Retryable != tt.wantRetryble {
				t.Fatalf("retryable = %v, want %v", ue.Retryable, tt.wantRetryble)
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
		t.Fatal("non-message JSON body must error")
	}
}

func TestUsageConversion(t *testing.T) {
	t.Run("IR to wire subtracts cache read", func(t *testing.T) {
		got := usageFromIR(ir.Usage{InputTokens: 302, OutputTokens: 53, CacheReadTokens: 256, CacheWriteTokens: 100})
		if got.InputTokens != 46 || got.CacheReadInputTokens != 256 ||
			got.CacheCreationInputTokens != 100 || got.OutputTokens != 53 {
			t.Fatalf("wire usage = %+v", got)
		}
	})
	t.Run("clamps negative remainder", func(t *testing.T) {
		got := usageFromIR(ir.Usage{InputTokens: 10, CacheReadTokens: 20})
		if got.InputTokens != 0 || got.CacheReadInputTokens != 20 {
			t.Fatalf("wire usage = %+v", got)
		}
	})
	t.Run("zero cache fields are omitted on the wire", func(t *testing.T) {
		data, err := json.Marshal(usageFromIR(ir.Usage{InputTokens: 34, OutputTokens: 90}))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "cache") {
			t.Fatalf("zero cache fields should be omitted: %s", data)
		}
	})
	t.Run("wire to IR adds cache read back", func(t *testing.T) {
		var u upstreamUsage
		if err := json.Unmarshal([]byte(`{
			"input_tokens": 46, "output_tokens": 53,
			"cache_creation_input_tokens": 8, "cache_read_input_tokens": 256,
			"billing_usage": {"openai_usage": {"completion_tokens_details": {"reasoning_tokens": 14}}}
		}`), &u); err != nil {
			t.Fatal(err)
		}
		got := u.toIR()
		if got.InputTokens != 302 || got.CacheReadTokens != 256 || got.CacheWriteTokens != 8 ||
			got.OutputTokens != 53 || got.ReasoningTokens != 14 {
			t.Fatalf("IR usage = %+v", got)
		}
	})
	t.Run("roundtrip is lossless", func(t *testing.T) {
		ir1 := ir.Usage{InputTokens: 302, OutputTokens: 53, CacheReadTokens: 256, CacheWriteTokens: 8, ReasoningTokens: 14}
		wire := usageFromIR(ir1)
		var u upstreamUsage
		raw, _ := json.Marshal(wire)
		if err := json.Unmarshal(raw, &u); err != nil {
			t.Fatal(err)
		}
		// reasoning tokens ride only in the upstream's billing extension and do not
		// survive the plain-wire roundtrip; everything else must.
		ir2 := u.toIR()
		ir1.ReasoningTokens = ir2.ReasoningTokens
		if ir1 != ir2 {
			t.Fatalf("roundtrip mismatch: %+v vs %+v", ir1, ir2)
		}
	})
}
