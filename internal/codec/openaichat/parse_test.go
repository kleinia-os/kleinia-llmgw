package openaichat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

func TestParseClientRequestPlain(t *testing.T) {
	req, err := ParseClientRequest(load(t, "request_plain.json"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "ds-flash" {
		t.Errorf("model = %q", req.Model)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages: %d", len(req.Messages))
	}
	if req.Messages[0].Role != ir.RoleSystem || req.Messages[0].Content[0].Type != ir.PartText ||
		req.Messages[0].Content[0].Text != "你是测试助手" {
		t.Errorf("system message: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != ir.RoleUser || req.Messages[1].Content[0].Text != "3的平方加4的平方等于几？" {
		t.Errorf("user message: %+v", req.Messages[1])
	}
	if req.Stream {
		t.Error("stream should be false")
	}
	if req.Sampling.Temperature == nil || *req.Sampling.Temperature != 0 {
		t.Errorf("temperature: %v (0 must be preserved, not treated as unset)", req.Sampling.Temperature)
	}
	if req.Sampling.MaxTokens != 128 {
		t.Errorf("max_tokens: %d", req.Sampling.MaxTokens)
	}
	if req.Sampling.Seed == nil || *req.Sampling.Seed != 42 {
		t.Errorf("seed: %v", req.Sampling.Seed)
	}
	if !reflect.DeepEqual(req.Sampling.Stop, []string{"\n"}) {
		t.Errorf("stop: %q", req.Sampling.Stop)
	}
	if req.Raw["user"] == nil || string(req.Raw["user"]) != `"klein"` {
		t.Errorf("unknown field user not preserved in Raw: %v", req.Raw)
	}
}

func TestParseClientRequestTools(t *testing.T) {
	req, err := ParseClientRequest(load(t, "request_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages: %d", len(req.Messages))
	}
	as := req.Messages[2]
	if as.Role != ir.RoleAssistant {
		t.Fatalf("assistant role: %q", as.Role)
	}
	want := []ir.Part{
		{Type: ir.PartThinking, Text: "We need to call the weather tool for Beijing."},
		{Type: ir.PartToolUse, ToolCallID: "call_00_94091ex7gxe95toioqwsoltl",
			ToolName: "get_weather", ToolArgsJSON: `{"city": "北京"}`},
	}
	if !reflect.DeepEqual(as.Content, want) {
		t.Errorf("assistant parts:\n got %+v\nwant %+v", as.Content, want)
	}
	tool := req.Messages[3]
	if tool.Role != ir.RoleTool || len(tool.Content) != 1 {
		t.Fatalf("tool message: %+v", tool)
	}
	tr := tool.Content[0]
	if tr.Type != ir.PartToolResult || tr.ToolCallID != "call_00_94091ex7gxe95toioqwsoltl" || tr.ToolOutput != `{"temp_c": 18}` {
		t.Errorf("tool result part: %+v", tr)
	}

	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" || req.Tools[0].Description != "query current weather" {
		t.Fatalf("tools: %+v", req.Tools)
	}
	var schema map[string]any
	if err := json.Unmarshal(req.Tools[0].ParametersJSON, &schema); err != nil {
		t.Fatalf("parameters not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("parameters schema: %v", schema)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != ir.ToolChoiceNamed || req.ToolChoice.Name != "get_weather" {
		t.Errorf("tool_choice: %+v", req.ToolChoice)
	}
	if req.Sampling.TopK == nil || *req.Sampling.TopK != 50 {
		t.Errorf("top_k: %v", req.Sampling.TopK)
	}
	if req.Sampling.Temperature == nil || *req.Sampling.Temperature != 0.2 {
		t.Errorf("temperature: %v", req.Sampling.Temperature)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format: %+v", req.ResponseFormat)
	}
	if req.Raw["frequency_penalty"] == nil {
		t.Errorf("unknown field frequency_penalty not preserved: %v", req.Raw)
	}
}

func TestParseClientRequestMultimodal(t *testing.T) {
	req, err := ParseClientRequest(load(t, "request_multimodal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !req.Stream {
		t.Error("stream should be true")
	}
	parts := req.Messages[0].Content
	want := []ir.Part{
		{Type: ir.PartText, Text: "这张图里是什么？"},
		{Type: ir.PartImage, ImageURL: "https://example.com/cat.png"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Errorf("parts:\n got %+v\nwant %+v", parts, want)
	}
}

func TestParseClientRequestStopString(t *testing.T) {
	req, err := ParseClientRequest([]byte(`{"model":"m","messages":[],"stop":"END"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Sampling.Stop, []string{"END"}) {
		t.Errorf("stop: %q", req.Sampling.Stop)
	}
}

func TestParseClientRequestToolChoiceString(t *testing.T) {
	for _, s := range []string{"auto", "none", "required"} {
		req, err := ParseClientRequest([]byte(`{"tool_choice":"` + s + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		if req.ToolChoice.Type != ir.ToolChoiceType(s) {
			t.Errorf("tool_choice %q: %+v", s, req.ToolChoice)
		}
	}
	if _, err := ParseClientRequest([]byte(`{"tool_choice":"bogus"}`)); err == nil {
		t.Error("bogus tool_choice should error")
	}
}

func TestParseClientRequestMalformed(t *testing.T) {
	for _, body := range []string{`not json`, `[1,2]`, `null`, `{"model": 3}`} {
		if _, err := ParseClientRequest([]byte(body)); err == nil {
			t.Errorf("body %q should fail", body)
		}
	}
}
