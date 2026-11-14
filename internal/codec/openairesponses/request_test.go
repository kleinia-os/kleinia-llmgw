package openairesponses

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

// The plain request form that passed verbatim against a live upstream.
func TestParseClientRequestPlain(t *testing.T) {
	body := `{"model":"test-model",
	          "input":[{"role":"user","content":[{"type":"input_text","text":"用一句话介绍你自己"}]}],
	          "max_output_tokens":128,"temperature":0}`
	req, err := ParseClientRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "test-model" {
		t.Fatalf("model = %q", req.Model)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	m := req.Messages[0]
	if m.Role != ir.RoleUser || len(m.Content) != 1 || m.Content[0].Type != ir.PartText ||
		m.Content[0].Text != "用一句话介绍你自己" {
		t.Fatalf("message = %+v", m)
	}
	if req.Sampling.MaxTokens != 128 {
		t.Fatalf("max_output_tokens = %d", req.Sampling.MaxTokens)
	}
	if req.Sampling.Temperature == nil || *req.Sampling.Temperature != 0 {
		t.Fatalf("temperature = %v", req.Sampling.Temperature)
	}
	if req.Stream {
		t.Fatal("stream should be false")
	}
	if len(req.Tools) != 0 || req.ToolChoice != nil || len(req.Raw) != 0 {
		t.Fatalf("tools/tool_choice/raw should be empty: %+v", req)
	}
}

func TestParseClientRequestStringInput(t *testing.T) {
	req, err := ParseClientRequest([]byte(`{"model":"m","input":"你好"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser ||
		req.Messages[0].Content[0].Text != "你好" {
		t.Fatalf("messages = %+v", req.Messages)
	}
}

// The tool round-trip form verified against a live upstream: easy-input user
// message, reasoning + function_call replay, function_call_output, flat
// function tools (testdata/resp_tools_roundtrip.json).
func TestParseClientRequestToolsRoundtrip(t *testing.T) {
	body := `{
	  "model": "test-model",
	  "instructions": "你是天气助手",
	  "input": [
	    {"role": "user", "content": [{"type": "input_text", "text": "北京天气怎么样？"}]},
	    {"type": "reasoning", "id": "rs_551013db078b43bba1567b477190a6c2",
	     "summary": [{"type": "summary_text", "text": "用户问北京天气。"},
	                 {"type": "summary_text", "text": "调用工具。"}]},
	    {"type": "function_call", "call_id": "call_00_f6cjso6e2ck984bd46iq7nlr",
	     "name": "get_weather", "arguments": "{\"city\": \"北京\"}"},
	    {"type": "function_call_output", "call_id": "call_00_f6cjso6e2ck984bd46iq7nlr",
	     "output": "{\"temp_c\":18,\"condition\":\"晴\"}"}
	  ],
	  "tools": [{"type": "function", "name": "get_weather",
	             "description": "查询指定城市的当前天气",
	             "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}],
	  "tool_choice": "auto",
	  "max_output_tokens": 128,
	  "temperature": 0,
	  "stream": false
	}`
	req, err := ParseClientRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []ir.Message{
		{Role: ir.RoleSystem, Content: []ir.Part{{Type: ir.PartText, Text: "你是天气助手"}}},
		{Role: ir.RoleUser, Content: []ir.Part{{Type: ir.PartText, Text: "北京天气怎么样？"}}},
		{Role: ir.RoleAssistant, Content: []ir.Part{
			{Type: ir.PartThinking, Text: "用户问北京天气。调用工具。"},
			{Type: ir.PartToolUse, ToolCallID: "call_00_f6cjso6e2ck984bd46iq7nlr",
				ToolName: "get_weather", ToolArgsJSON: `{"city": "北京"}`},
		}},
		{Role: ir.RoleTool, Content: []ir.Part{
			{Type: ir.PartToolResult, ToolCallID: "call_00_f6cjso6e2ck984bd46iq7nlr",
				ToolOutput: `{"temp_c":18,"condition":"晴"}`},
		}},
	}
	if !reflect.DeepEqual(req.Messages, want) {
		t.Fatalf("messages mismatch:\n got %+v\nwant %+v", req.Messages, want)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" ||
		req.Tools[0].Description != "查询指定城市的当前天气" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	var schema map[string]any
	if err := json.Unmarshal(req.Tools[0].ParametersJSON, &schema); err != nil {
		t.Fatalf("parameters not valid JSON: %v", err)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != ir.ToolChoiceAuto {
		t.Fatalf("tool_choice = %+v", req.ToolChoice)
	}
}

// Clients replay prior Responses output arrays as input; the goldens' own
// output items must parse back into IR (message items with output_text
// content, function_call items with dual ids).
func TestParseClientRequestReplaysGoldenOutput(t *testing.T) {
	for _, file := range []string{"resp_plain.json", "resp_tools.json"} {
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
		body, _ := json.Marshal(map[string]any{"model": "m", "input": g.Output})
		req, err := ParseClientRequest(body)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, m := range req.Messages {
			if m.Role != ir.RoleAssistant {
				t.Fatalf("%s: unexpected role %q", file, m.Role)
			}
		}
		switch file {
		case "resp_plain.json":
			if len(req.Messages) != 2 {
				t.Fatalf("resp_plain: messages = %+v", req.Messages)
			}
			if p := req.Messages[0].Content[0]; p.Type != ir.PartThinking || p.Text == "" {
				t.Fatalf("resp_plain: thinking part = %+v", p)
			}
			if p := req.Messages[1].Content[0]; p.Type != ir.PartText || p.Text == "" {
				t.Fatalf("resp_plain: text part = %+v", p)
			}
		case "resp_tools.json":
			// reasoning + function_call collapse into one assistant message.
			if len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
				t.Fatalf("resp_tools: messages = %+v", req.Messages)
			}
			tu := req.Messages[0].Content[1]
			if tu.Type != ir.PartToolUse || tu.ToolCallID != "call_00_f6cjso6e2ck984bd46iq7nlr" ||
				tu.ToolName != "get_weather" || tu.ToolArgsJSON != `{"city": "北京"}` {
				t.Fatalf("resp_tools: tool part = %+v", tu)
			}
		}
	}
}

func TestParseClientRequestUnknownFieldsToRaw(t *testing.T) {
	body := `{"model":"m","input":"hi","store":false,"metadata":{"a":"b"},
	          "reasoning":{"effort":"low"},"previous_response_id":"resp_x",
	          "parallel_tool_calls":true,"text":{"format":{"type":"json_object"}}}`
	req, err := ParseClientRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"store":                "false",
		"metadata":             `{"a":"b"}`,
		"reasoning":            `{"effort":"low"}`,
		"previous_response_id": `"resp_x"`,
		"parallel_tool_calls":  "true",
		"text":                 `{"format":{"type":"json_object"}}`,
	}
	if len(req.Raw) != len(want) {
		t.Fatalf("raw = %v", req.Raw)
	}
	for k, v := range want {
		if string(req.Raw[k]) != v {
			t.Fatalf("raw[%s] = %s, want %s", k, req.Raw[k], v)
		}
	}
}

func TestParseClientRequestToolChoiceForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ir.ToolChoice
	}{
		{`"auto"`, ir.ToolChoice{Type: ir.ToolChoiceAuto}},
		{`"none"`, ir.ToolChoice{Type: ir.ToolChoiceNone}},
		{`"required"`, ir.ToolChoice{Type: ir.ToolChoiceRequired}},
		{`{"type":"function","name":"get_weather"}`, ir.ToolChoice{Type: ir.ToolChoiceNamed, Name: "get_weather"}},
	} {
		body, _ := json.Marshal(map[string]any{"model": "m", "input": "hi", "tool_choice": json.RawMessage(tc.in)})
		req, err := ParseClientRequest(body)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if req.ToolChoice == nil || *req.ToolChoice != tc.want {
			t.Fatalf("%s: tool_choice = %+v, want %+v", tc.in, req.ToolChoice, tc.want)
		}
	}
}

func TestParseClientRequestImageAndRoles(t *testing.T) {
	body := `{"model":"m","instructions":"sys","input":[
	  {"role":"developer","content":"dev rules"},
	  {"role":"user","content":[
	    {"type":"input_text","text":"看图"},
	    {"type":"input_image","image_url":"https://example.com/a.png","detail":"high"}]}]}`
	req, err := ParseClientRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[0].Role != ir.RoleSystem || req.Messages[0].Content[0].Text != "sys" {
		t.Fatalf("instructions message = %+v", req.Messages[0])
	}
	if req.Messages[1].Role != ir.RoleSystem || req.Messages[1].Content[0].Text != "dev rules" {
		t.Fatalf("developer message = %+v", req.Messages[1])
	}
	u := req.Messages[2]
	if u.Role != ir.RoleUser || len(u.Content) != 2 {
		t.Fatalf("user message = %+v", u)
	}
	if img := u.Content[1]; img.Type != ir.PartImage || img.ImageURL != "https://example.com/a.png" {
		t.Fatalf("image part = %+v", img)
	}
}

func TestParseClientRequestUnknownItemsSkipped(t *testing.T) {
	body := `{"model":"m","input":[
	  {"type":"web_search_call","id":"ws_1"},
	  {"role":"user","content":"hi"}]}`
	req, err := ParseClientRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser {
		t.Fatalf("messages = %+v", req.Messages)
	}
}

func TestParseClientRequestLenientForms(t *testing.T) {
	// Official-API array form of function_call_output, and the chat-style
	// image_url object — both accepted defensively.
	body := `{"model":"m","input":[
	  {"type":"function_call_output","call_id":"c1",
	   "output":[{"type":"output_text","text":"部分一"},{"type":"output_text","text":"部分二"}]},
	  {"role":"user","content":[{"type":"input_image","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`
	req, err := ParseClientRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out := req.Messages[0].Content[0]
	if out.Type != ir.PartToolResult || out.ToolCallID != "c1" || out.ToolOutput != "部分一\n部分二" {
		t.Fatalf("tool output = %+v", out)
	}
	img := req.Messages[1].Content[0]
	if img.Type != ir.PartImage || img.ImageURL != "data:image/png;base64,AAA" {
		t.Fatalf("image part = %+v", img)
	}
}

func TestParseClientRequestErrors(t *testing.T) {
	cases := []string{
		`{`,
		`[]`,
		`{"model":"m"}`,
		`{"model":"m","input":null}`,
		`{"model":"m","input":123}`,
		`{"model":"m","input":"hi","temperature":"hot"}`,
		`{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`,
		`{"model":"m","input":"hi","tools":[{"type":"function","description":"no name"}]}`,
		`{"model":"m","input":"hi","tool_choice":"sometimes"}`,
		`{"model":"m","input":"hi","tool_choice":{"type":"web_search"}}`,
		`{"model":"m","input":[{"role":"wizard","content":"x"}]}`,
		`{"model":"m","input":[{"type":"function_call","name":"f"}]}`,
		`{"model":"m","input":[{"type":"function_call_output","output":"x"}]}`,
		`{"model":"m","input":[{"type":"message","role":"user","content":42}]}`,
	}
	for _, body := range cases {
		if _, err := ParseClientRequest([]byte(body)); err == nil {
			t.Fatalf("ParseClientRequest(%s) succeeded, want error", body)
		}
	}
}
