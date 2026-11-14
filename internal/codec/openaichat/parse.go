package openaichat

import (
	"encoding/json"
	"errors"
	"fmt"

	"kleinia.io/llmgw/internal/ir"
)

// clientMessage is the wire shape of one incoming message. Content stays raw
// because it is either a string or an array of typed parts.
type clientMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	Name             string          `json:"name"`
	ToolCallID       string          `json:"tool_call_id"`
	ToolCalls        []clientToolRef `json:"tool_calls"`
}

type clientToolRef struct {
	ID       string                `json:"id"`
	Type     string                `json:"type"`
	Function clientToolRefFunction `json:"function"`
}

type clientToolRefFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Arguments   string          `json:"arguments"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ParseClientRequest decodes an OpenAI Chat Completions request body into IR.
// Unknown well-formed JSON fields are preserved verbatim in req.Raw.
func ParseClientRequest(body []byte) (ir.Request, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return ir.Request{}, fmt.Errorf("openaichat: malformed request JSON: %w", err)
	}
	if top == nil {
		return ir.Request{}, errors.New("openaichat: request body must be a JSON object")
	}
	req := ir.Request{Raw: make(map[string]json.RawMessage)}
	for k, v := range top {
		var err error
		switch k {
		case "model":
			err = json.Unmarshal(v, &req.Model)
		case "messages":
			req.Messages, err = parseClientMessages(v)
		case "tools":
			req.Tools, err = parseClientTools(v)
		case "tool_choice":
			req.ToolChoice, err = parseClientToolChoice(v)
		case "response_format":
			var rf struct {
				Type string `json:"type"`
			}
			if err = json.Unmarshal(v, &rf); err == nil {
				req.ResponseFormat = &ir.ResponseFormat{Type: rf.Type}
			}
		case "stream":
			err = json.Unmarshal(v, &req.Stream)
		case "temperature":
			req.Sampling.Temperature, err = decodeFloat(v)
		case "top_p":
			req.Sampling.TopP, err = decodeFloat(v)
		case "max_tokens":
			err = json.Unmarshal(v, &req.Sampling.MaxTokens)
		case "stop":
			req.Sampling.Stop, err = decodeStop(v)
		case "seed":
			req.Sampling.Seed, err = decodeInt(v)
		case "top_k":
			// Non-standard for OpenAI but IR understands it; new-api
			// upstreams accept it, so keep it first-class.
			req.Sampling.TopK, err = decodeInt(v)
		default:
			req.Raw[k] = v
		}
		if err != nil {
			return ir.Request{}, fmt.Errorf("openaichat: bad field %q: %w", k, err)
		}
	}
	if len(req.Raw) == 0 {
		req.Raw = nil
	}
	return req, nil
}

func decodeFloat(v json.RawMessage) (*float64, error) {
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func decodeInt(v json.RawMessage) (*int64, error) {
	var n int64
	if err := json.Unmarshal(v, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// decodeStop accepts the wire's "string or array of strings" union.
func decodeStop(v json.RawMessage) ([]string, error) {
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(v, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func parseClientMessages(raw json.RawMessage) ([]ir.Message, error) {
	var in []clientMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]ir.Message, 0, len(in))
	for _, m := range in {
		msg, err := parseClientMessage(m)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, nil
}

func parseClientMessage(m clientMessage) (ir.Message, error) {
	role := roleFromWire(m.Role)
	msg := ir.Message{Role: role}
	if role == ir.RoleTool {
		out, err := toolOutputFromContent(m.Content)
		if err != nil {
			return msg, err
		}
		if m.ToolCallID != "" || out != "" {
			msg.Content = []ir.Part{{Type: ir.PartToolResult, ToolCallID: m.ToolCallID, ToolOutput: out}}
		}
		return msg, nil
	}
	// thinking first: reasoning always precedes visible content in practice.
	if m.ReasoningContent != "" {
		msg.Content = append(msg.Content, ir.Part{Type: ir.PartThinking, Text: m.ReasoningContent})
	}
	parts, err := partsFromContent(m.Content)
	if err != nil {
		return msg, err
	}
	msg.Content = append(msg.Content, parts...)
	for _, tc := range m.ToolCalls {
		msg.Content = append(msg.Content, ir.Part{
			Type:         ir.PartToolUse,
			ToolCallID:   tc.ID,
			ToolName:     tc.Function.Name,
			ToolArgsJSON: tc.Function.Arguments, // raw JSON string, kept verbatim
		})
	}
	return msg, nil
}

func roleFromWire(s string) ir.Role {
	switch s {
	case "developer", "system":
		return ir.RoleSystem
	case "user":
		return ir.RoleUser
	case "assistant":
		return ir.RoleAssistant
	case "tool", "function":
		return ir.RoleTool
	default:
		return ir.Role(s)
	}
}

// partsFromContent converts string-or-array content into IR parts.
func partsFromContent(raw json.RawMessage) ([]ir.Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []ir.Part{{Type: ir.PartText, Text: s}}, nil
	}
	var in []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	var out []ir.Part
	for _, p := range in {
		switch p.Type {
		case "text", "input_text":
			if p.Text != "" {
				out = append(out, ir.Part{Type: ir.PartText, Text: p.Text})
			}
		case "image_url":
			if p.ImageURL != nil {
				out = append(out, ir.Part{Type: ir.PartImage, ImageURL: p.ImageURL.URL})
			}
		}
	}
	return out, nil
}

// toolOutputFromContent flattens a tool message's string-or-array content to
// plain text for PartToolResult.ToolOutput.
func toolOutputFromContent(raw json.RawMessage) (string, error) {
	parts, err := partsFromContent(raw)
	if err != nil {
		return "", err
	}
	out := ""
	for _, p := range parts {
		if p.Type == ir.PartText {
			out += p.Text
		}
	}
	return out, nil
}

func parseClientTools(raw json.RawMessage) ([]ir.Tool, error) {
	var in []struct {
		Type     string                 `json:"type"`
		Function *clientToolRefFunction `json:"function"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	var out []ir.Tool
	for _, t := range in {
		if t.Function == nil || (t.Type != "" && t.Type != "function") {
			continue // custom/unsupported tool types are not representable in IR
		}
		out = append(out, ir.Tool{
			Name:           t.Function.Name,
			Description:    t.Function.Description,
			ParametersJSON: t.Function.Parameters,
		})
	}
	return out, nil
}

func parseClientToolChoice(raw json.RawMessage) (*ir.ToolChoice, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch ir.ToolChoiceType(s) {
		case ir.ToolChoiceAuto, ir.ToolChoiceNone, ir.ToolChoiceRequired:
			return &ir.ToolChoice{Type: ir.ToolChoiceType(s)}, nil
		}
		return nil, fmt.Errorf("openaichat: unsupported tool_choice %q", s)
	}
	var obj struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	switch ir.ToolChoiceType(obj.Type) {
	case ir.ToolChoiceAuto, ir.ToolChoiceNone, ir.ToolChoiceRequired:
		return &ir.ToolChoice{Type: ir.ToolChoiceType(obj.Type)}, nil
	case ir.ToolChoiceNamed, "function":
		name := ""
		if obj.Function != nil {
			name = obj.Function.Name
		}
		return &ir.ToolChoice{Type: ir.ToolChoiceNamed, Name: name}, nil
	}
	return nil, fmt.Errorf("openaichat: unsupported tool_choice type %q", obj.Type)
}
