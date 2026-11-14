package anthropic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

func TestParseClientRequest(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		verify  func(*testing.T, ir.Request)
		wantErr string
	}{
		{
			name: "plain string content",
			body: `{"model":"m1","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
			verify: func(t *testing.T, req ir.Request) {
				if req.Model != "m1" {
					t.Fatalf("model = %q", req.Model)
				}
				if req.Sampling.MaxTokens != 100 {
					t.Fatalf("max_tokens = %d", req.Sampling.MaxTokens)
				}
				if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser {
					t.Fatalf("messages = %+v", req.Messages)
				}
				if p := req.Messages[0].Content; len(p) != 1 || p[0].Type != ir.PartText || p[0].Text != "hi" {
					t.Fatalf("content = %+v", p)
				}
				if len(req.Raw) != 0 {
					t.Fatalf("raw = %+v", req.Raw)
				}
			},
		},
		{
			name: "missing max_tokens defaults to 4096",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			verify: func(t *testing.T, req ir.Request) {
				if req.Sampling.MaxTokens != 4096 {
					t.Fatalf("max_tokens = %d, want 4096", req.Sampling.MaxTokens)
				}
			},
		},
		{
			name: "system as string becomes leading RoleSystem message",
			body: `{"system":"You are K.","messages":[{"role":"user","content":"hi"}]}`,
			verify: func(t *testing.T, req ir.Request) {
				if len(req.Messages) != 2 {
					t.Fatalf("messages = %+v", req.Messages)
				}
				sys := req.Messages[0]
				if sys.Role != ir.RoleSystem || len(sys.Content) != 1 ||
					sys.Content[0].Type != ir.PartText || sys.Content[0].Text != "You are K." {
					t.Fatalf("system message = %+v", sys)
				}
				if req.Messages[1].Role != ir.RoleUser {
					t.Fatalf("second message role = %q", req.Messages[1].Role)
				}
			},
		},
		{
			name: "system as text block array is joined",
			body: `{"system":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"messages":[{"role":"user","content":"hi"}]}`,
			verify: func(t *testing.T, req ir.Request) {
				sys := req.Messages[0]
				if sys.Role != ir.RoleSystem || sys.Content[0].Text != "A\nB" {
					t.Fatalf("system message = %+v", sys)
				}
			},
		},
		{
			name: "assistant tool_use block with spaced input is compacted",
			body: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_00_x","name":"get_weather","input":{"city": "北京"}}]}]}`,
			verify: func(t *testing.T, req ir.Request) {
				p := req.Messages[0].Content[0]
				if p.Type != ir.PartToolUse || p.ToolCallID != "call_00_x" || p.ToolName != "get_weather" {
					t.Fatalf("part = %+v", p)
				}
				if p.ToolArgsJSON != `{"city":"北京"}` {
					t.Fatalf("args = %q", p.ToolArgsJSON)
				}
			},
		},
		{
			name: "tool_use without input gets empty object args",
			body: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"f"}]}]}`,
			verify: func(t *testing.T, req ir.Request) {
				if got := req.Messages[0].Content[0].ToolArgsJSON; got != "{}" {
					t.Fatalf("args = %q, want {}", got)
				}
			},
		},
		{
			name: "user tool_result is extracted into a RoleTool message",
			body: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_00_x","content":"{\"temp_c\":18}","is_error":true},{"type":"text","text":"and this"}]}]}`,
			verify: func(t *testing.T, req ir.Request) {
				if len(req.Messages) != 2 {
					t.Fatalf("messages = %+v", req.Messages)
				}
				tool := req.Messages[0]
				if tool.Role != ir.RoleTool || len(tool.Content) != 1 {
					t.Fatalf("tool message = %+v", tool)
				}
				p := tool.Content[0]
				if p.Type != ir.PartToolResult || p.ToolCallID != "call_00_x" ||
					p.ToolOutput != `{"temp_c":18}` || !p.IsError {
					t.Fatalf("tool result part = %+v", p)
				}
				if req.Messages[1].Role != ir.RoleUser || req.Messages[1].Content[0].Text != "and this" {
					t.Fatalf("remaining user message = %+v", req.Messages[1])
				}
			},
		},
		{
			name: "tool_result with block content joins texts",
			body: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"text","text":"r1"},{"type":"text","text":"r2"}]}]}]}`,
			verify: func(t *testing.T, req ir.Request) {
				if got := req.Messages[0].Content[0].ToolOutput; got != "r1\nr2" {
					t.Fatalf("tool output = %q", got)
				}
			},
		},
		{
			name: "base64 image becomes a data URL part",
			body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},{"type":"text","text":"what is this"}]}]}`,
			verify: func(t *testing.T, req ir.Request) {
				p := req.Messages[0].Content[0]
				if p.Type != ir.PartImage || p.ImageURL != "data:image/png;base64,aGk=" {
					t.Fatalf("image part = %+v", p)
				}
				if req.Messages[0].Content[1].Type != ir.PartText {
					t.Fatal("text part missing")
				}
			},
		},
		{
			name: "url image source and non-standard image_url variant",
			body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x/p.png"}},{"type":"image_url","image_url":{"url":"https://y/q.png"}}]}]}`,
			verify: func(t *testing.T, req ir.Request) {
				if got := req.Messages[0].Content[0].ImageURL; got != "https://x/p.png" {
					t.Fatalf("url image = %q", got)
				}
				if got := req.Messages[0].Content[1].ImageURL; got != "https://y/q.png" {
					t.Fatalf("image_url variant = %q", got)
				}
			},
		},
		{
			name: "tools, sampling, stop sequences, stream, raw passthrough",
			body: `{"model":"m","max_tokens":7,"temperature":0.5,"top_p":0.9,"top_k":40,` +
				`"stop_sequences":["\n\nUser:"],"stream":true,` +
				`"tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],` +
				`"thinking":{"type":"enabled","budget_tokens":1024},"metadata":{"user_id":"u1"},` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			verify: func(t *testing.T, req ir.Request) {
				s := req.Sampling
				if s.MaxTokens != 7 || s.Temperature == nil || *s.Temperature != 0.5 ||
					s.TopP == nil || *s.TopP != 0.9 || s.TopK == nil || *s.TopK != 40 {
					t.Fatalf("sampling = %+v", s)
				}
				if len(s.Stop) != 1 || s.Stop[0] != "\n\nUser:" {
					t.Fatalf("stop = %+v", s.Stop)
				}
				if !req.Stream {
					t.Fatal("stream not set")
				}
				if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" || req.Tools[0].Description != "weather" {
					t.Fatalf("tools = %+v", req.Tools)
				}
				var schema struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(req.Tools[0].ParametersJSON, &schema); err != nil || schema.Type != "object" {
					t.Fatalf("input_schema = %s (%v)", req.Tools[0].ParametersJSON, err)
				}
				var thinking struct {
					Type string `json:"type"`
				}
				if raw, ok := req.Raw["thinking"]; !ok || json.Unmarshal(raw, &thinking) != nil || thinking.Type != "enabled" {
					t.Fatalf("raw thinking = %s", req.Raw["thinking"])
				}
				if _, ok := req.Raw["metadata"]; !ok {
					t.Fatalf("raw metadata missing: %+v", req.Raw)
				}
				if len(req.Raw) != 2 {
					t.Fatalf("raw = %+v", req.Raw)
				}
			},
		},
		{
			name: "tool_choice variants",
			body: `{"tool_choice":{"type":"any","disable_parallel_tool_use":true},"messages":[{"role":"user","content":"x"}]}`,
			verify: func(t *testing.T, req ir.Request) {
				if req.ToolChoice == nil || req.ToolChoice.Type != ir.ToolChoiceRequired {
					t.Fatalf("tool_choice = %+v", req.ToolChoice)
				}
			},
		},
		{
			name: "tool_choice named",
			body: `{"tool_choice":{"type":"tool","name":"get_weather"},"messages":[{"role":"user","content":"x"}]}`,
			verify: func(t *testing.T, req ir.Request) {
				if req.ToolChoice == nil || req.ToolChoice.Type != ir.ToolChoiceNamed || req.ToolChoice.Name != "get_weather" {
					t.Fatalf("tool_choice = %+v", req.ToolChoice)
				}
			},
		},
		{
			name:    "malformed json",
			body:    `{"messages":[`,
			wantErr: "not a JSON object",
		},
		{
			name:    "messages missing",
			body:    `{"model":"m","max_tokens":10}`,
			wantErr: "messages is required",
		},
		{
			name:    "messages not an array",
			body:    `{"messages":"hi"}`,
			wantErr: "must be an array",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := ParseClientRequest([]byte(tt.body))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.verify(t, req)
		})
	}
}

func TestWriteClientResponse(t *testing.T) {
	res := ir.Response{
		Model: "test-model",
		Content: []ir.Part{
			{Type: ir.PartThinking, Text: "hmm"},
			{Type: ir.PartText, Text: "hello"},
			{Type: ir.PartToolUse, ToolCallID: "call_00_x", ToolName: "get_weather", ToolArgsJSON: `{"a":1}`},
			{Type: ir.PartToolResult, ToolCallID: "ignored"}, // not renderable in an assistant message
		},
		FinishReason: ir.FinishToolCalls,
		Usage:        ir.Usage{InputTokens: 302, OutputTokens: 53, CacheReadTokens: 256, CacheWriteTokens: 100},
	}
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
	var got struct {
		ID           string            `json:"id"`
		Type         string            `json:"type"`
		Role         string            `json:"role"`
		Model        string            `json:"model"`
		Content      []json.RawMessage `json:"content"`
		StopReason   string            `json:"stop_reason"`
		StopSequence *string           `json:"stop_sequence"`
		Usage        struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body.String())
	}
	if !strings.HasPrefix(got.ID, "msg_") || len(got.ID) != len("msg_")+24 {
		t.Fatalf("id = %q", got.ID)
	}
	if got.Type != "message" || got.Role != "assistant" || got.Model != "test-model" {
		t.Fatalf("envelope = %+v", got)
	}
	if got.StopReason != "tool_use" || got.StopSequence != nil {
		t.Fatalf("stop_reason=%q stop_sequence=%v", got.StopReason, got.StopSequence)
	}
	if len(got.Content) != 3 {
		t.Fatalf("content blocks = %d: %s", len(got.Content), rec.Body.String())
	}
	var blockTypes []string
	for _, raw := range got.Content {
		var b struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		blockTypes = append(blockTypes, b.Type)
	}
	if strings.Join(blockTypes, ",") != "thinking,text,tool_use" {
		t.Fatalf("block types = %v", blockTypes)
	}
	var tool struct {
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(got.Content[2], &tool); err != nil || len(tool.Input) != 1 {
		t.Fatalf("tool input = %s (%v)", got.Content[2], err)
	}
	// usage conversion: IR input includes cache read; anthropic's excludes it.
	if got.Usage.InputTokens != 46 || got.Usage.CacheReadInputTokens != 256 ||
		got.Usage.CacheCreationInputTokens != 100 || got.Usage.OutputTokens != 53 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestWriteClientResponseFinishAndBadArgs(t *testing.T) {
	for _, tc := range []struct {
		finish ir.Finish
		want   string
	}{
		{ir.FinishStop, "end_turn"},
		{ir.FinishLength, "max_tokens"},
		{ir.FinishContentFilter, "refusal"},
	} {
		res := ir.Response{
			Model:        "m",
			Content:      []ir.Part{{Type: ir.PartToolUse, ToolCallID: "c", ToolName: "f", ToolArgsJSON: "{oops"}},
			FinishReason: tc.finish,
		}
		rec := httptest.NewRecorder()
		if err := WriteClientResponse(rec, res); err != nil {
			t.Fatal(err)
		}
		var got struct {
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.StopReason != tc.want {
			t.Fatalf("finish %q -> stop_reason %q, want %q", tc.finish, got.StopReason, tc.want)
		}
		if string(got.Content[0].Input) != "{}" {
			t.Fatalf("bad args input = %s, want {}", got.Content[0].Input)
		}
	}
}

func TestWriteClientError(t *testing.T) {
	tests := []struct {
		status      int
		kind        string
		msg         string
		wantStatus  int
		wantType    string
		msgContains string
	}{
		{http.StatusUnauthorized, ir.ErrInvalidKey, "missing API key", 401, "authentication_error", ""},
		{http.StatusTooManyRequests, ir.ErrRateLimited, "rpm exceeded", 429, "rate_limit_error", ""},
		{http.StatusTooManyRequests, ir.ErrQuota, "quota exhausted", 429, "rate_limit_error", ""},
		{http.StatusBadRequest, ir.ErrContextLength, "prompt 200000 tokens", 400, "invalid_request_error", "context window"},
		{http.StatusNotFound, "model_not_found", "no source provides model 'x'", 404, "not_found_error", ""},
		{http.StatusBadGateway, ir.ErrUpstream, "all candidates failed", 502, "api_error", ""},
		{http.StatusBadRequest, "invalid_request", "bad json", 400, "invalid_request_error", ""},
		{http.StatusForbidden, "model_not_allowed", "key may not use model", 403, "permission_error", ""},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		if err := WriteClientError(rec, tt.status, tt.kind, tt.msg); err != nil {
			t.Fatal(err)
		}
		if rec.Code != tt.wantStatus {
			t.Fatalf("%s: status = %d, want %d", tt.kind, rec.Code, tt.wantStatus)
		}
		var got struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: body: %v", tt.kind, err)
		}
		if got.Type != "error" || got.Error.Type != tt.wantType {
			t.Fatalf("%s: envelope = %+v, want error type %q", tt.kind, got, tt.wantType)
		}
		if tt.msgContains != "" && !strings.Contains(got.Error.Message, tt.msgContains) {
			t.Fatalf("%s: message = %q, want containing %q", tt.kind, got.Error.Message, tt.msgContains)
		}
		if tt.msgContains == "" && got.Error.Message != tt.msg {
			t.Fatalf("%s: message = %q, want %q", tt.kind, got.Error.Message, tt.msg)
		}
	}
}
