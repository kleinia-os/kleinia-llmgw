package openairesponses

import (
	"encoding/json"
	"fmt"
	"strings"

	"kleinia.io/llmgw/internal/ir"
)

// isNull reports whether a raw JSON value is null (or absent).
func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || strings.TrimSpace(string(raw)) == "null"
}

// ParseClientRequest decodes a /v1/responses request body into IR.
//
// Supported fields: model, input (string or item array), instructions, tools
// (flat function format), tool_choice, max_output_tokens, temperature, top_p,
// stream. Every other well-formed field (store, metadata, reasoning, ...) is
// preserved verbatim in req.Raw for same-format pass-through.
func ParseClientRequest(body []byte) (ir.Request, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return ir.Request{}, fmt.Errorf("responses: invalid JSON body: %w", err)
	}
	if v, ok := top["input"]; !ok || isNull(v) {
		return ir.Request{}, fmt.Errorf("responses: field input is required")
	}

	var req ir.Request
	var instructions string
	raw := map[string]json.RawMessage{}
	for k, v := range top {
		var err error
		switch k {
		case "model":
			err = json.Unmarshal(v, &req.Model)
		case "input":
			req.Messages, err = parseInput(v)
		case "instructions":
			err = json.Unmarshal(v, &instructions)
		case "tools":
			req.Tools, err = parseTools(v)
		case "tool_choice":
			req.ToolChoice, err = parseToolChoice(v)
		case "max_output_tokens":
			err = json.Unmarshal(v, &req.Sampling.MaxTokens)
		case "temperature":
			var t *float64
			if err = json.Unmarshal(v, &t); err == nil {
				req.Sampling.Temperature = t
			}
		case "top_p":
			var p *float64
			if err = json.Unmarshal(v, &p); err == nil {
				req.Sampling.TopP = p
			}
		case "stream":
			err = json.Unmarshal(v, &req.Stream)
		default:
			raw[k] = v
		}
		if err != nil {
			return ir.Request{}, fmt.Errorf("responses: field %s: %w", k, err)
		}
	}

	// instructions become a leading system message.
	if instructions != "" {
		sys := ir.Message{Role: ir.RoleSystem, Content: []ir.Part{{Type: ir.PartText, Text: instructions}}}
		req.Messages = append([]ir.Message{sys}, req.Messages...)
	}
	if len(raw) > 0 {
		req.Raw = raw
	}
	return req, nil
}

// parseInput accepts the two input forms: a plain string (one user text
// message) or an array of items (message / function_call /
// function_call_output / reasoning). Item types the IR cannot express
// (web_search_call, ...) are skipped instead of rejecting the whole replay.
func parseInput(raw json.RawMessage) ([]ir.Message, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ir.Message{{Role: ir.RoleUser, Content: []ir.Part{{Type: ir.PartText, Text: s}}}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("responses: field input must be a string or an array of items")
	}

	var msgs []ir.Message
	lastAssistant := -1
	ensureAssistant := func() int {
		if lastAssistant < 0 {
			msgs = append(msgs, ir.Message{Role: ir.RoleAssistant})
			lastAssistant = len(msgs) - 1
		}
		return lastAssistant
	}

	for _, rawItem := range items {
		var it struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(rawItem, &it); err != nil {
			return nil, fmt.Errorf("responses: bad input item: %w", err)
		}
		switch it.Type {
		case "", "message": // easy-input items omit "type"
			role, err := parseRole(it.Role)
			if err != nil {
				return nil, err
			}
			parts, err := parseContent(it.Content)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, ir.Message{Role: role, Content: parts})
			if role == ir.RoleAssistant {
				lastAssistant = len(msgs) - 1
			}
		case "function_call":
			if it.CallID == "" || it.Name == "" {
				return nil, fmt.Errorf("responses: function_call input item requires call_id and name")
			}
			idx := ensureAssistant()
			msgs[idx].Content = append(msgs[idx].Content, ir.Part{
				Type: ir.PartToolUse, ToolCallID: it.CallID,
				ToolName: it.Name, ToolArgsJSON: it.Arguments,
			})
		case "function_call_output":
			if it.CallID == "" {
				return nil, fmt.Errorf("responses: function_call_output input item requires call_id")
			}
			out, err := parseToolOutput(it.Output)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, ir.Message{Role: ir.RoleTool, Content: []ir.Part{{
				Type: ir.PartToolResult, ToolCallID: it.CallID, ToolOutput: out,
			}}})
		case "reasoning":
			var sb strings.Builder
			for _, sum := range it.Summary {
				sb.WriteString(sum.Text)
			}
			idx := ensureAssistant()
			msgs[idx].Content = append(msgs[idx].Content, ir.Part{Type: ir.PartThinking, Text: sb.String()})
		default:
			// Unknown item type: history artifact we cannot express; skip.
		}
	}
	return msgs, nil
}

func parseRole(role string) (ir.Role, error) {
	switch role {
	case "", "user":
		return ir.RoleUser, nil
	case "assistant":
		return ir.RoleAssistant, nil
	case "system", "developer":
		return ir.RoleSystem, nil
	default:
		return "", fmt.Errorf("responses: unsupported input role %q", role)
	}
}

// parseContent accepts content as a string or an array of parts
// (input_text / output_text / input_image).
func parseContent(raw json.RawMessage) ([]ir.Part, error) {
	if isNull(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []ir.Part{{Type: ir.PartText, Text: s}}, nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("responses: content must be a string or an array of parts")
	}
	out := make([]ir.Part, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "refusal":
			out = append(out, ir.Part{Type: ir.PartText, Text: p.Text})
		case "input_image":
			u, err := parseImageURL(p.ImageURL)
			if err != nil {
				return nil, err
			}
			out = append(out, ir.Part{Type: ir.PartImage, ImageURL: u})
		default:
			// Unknown part type: skip.
		}
	}
	return out, nil
}

// parseImageURL accepts the Responses string form ("https://..." or a data:
// URL) and, defensively, the chat-style {"url": ...} object form.
func parseImageURL(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("responses: input_image.image_url must be a string")
	}
	return obj.URL, nil
}

// parseToolOutput accepts the string form used by every golden sample and the
// array-of-parts form the official API allows.
func parseToolOutput(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("responses: function_call_output.output must be a string")
	}
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// parseTools accepts the flat Responses function-tool format.
func parseTools(raw json.RawMessage) ([]ir.Tool, error) {
	var tools []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("responses: field tools must be an array")
	}
	out := make([]ir.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("responses: unsupported tool type %q (only function tools are supported)", t.Type)
		}
		if t.Name == "" {
			return nil, fmt.Errorf("responses: tool definition requires name")
		}
		out = append(out, ir.Tool{Name: t.Name, Description: t.Description, ParametersJSON: t.Parameters})
	}
	return out, nil
}

// parseToolChoice accepts "auto" | "none" | "required" or the named object
// form {"type":"function","name":...}.
func parseToolChoice(raw json.RawMessage) (*ir.ToolChoice, error) {
	if isNull(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return &ir.ToolChoice{Type: ir.ToolChoiceType(s)}, nil
		default:
			return nil, fmt.Errorf("responses: unsupported tool_choice %q", s)
		}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("responses: field tool_choice must be a string or an object")
	}
	if obj.Type != "function" || obj.Name == "" {
		return nil, fmt.Errorf("responses: unsupported tool_choice object (want {\"type\":\"function\",\"name\":...})")
	}
	return &ir.ToolChoice{Type: ir.ToolChoiceNamed, Name: obj.Name}, nil
}
