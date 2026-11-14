package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"kleinia.io/llmgw/internal/ir"
)

// ---- outbound request building ----

// upstreamBody is the outbound /responses request body. Extra carries Raw
// pass-through fields (store, metadata, reasoning, ...) merged in at marshal
// time; owned fields always win on collision.
type upstreamBody struct {
	Model           string    `json:"model"`
	Instructions    string    `json:"instructions,omitempty"`
	Input           []any     `json:"input"`
	MaxOutputTokens int64     `json:"max_output_tokens,omitempty"` // 0 = unset: official spec makes it optional, the upstream accepts omission
	Temperature     *float64  `json:"temperature,omitempty"`
	TopP            *float64  `json:"top_p,omitempty"`
	Tools           []upFnDef `json:"tools,omitempty"`
	ToolChoice      any       `json:"tool_choice,omitempty"` // string or {"type":"function","name":...}
	Stream          bool      `json:"stream,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// MarshalJSON merges the Extra pass-through fields after the owned fields.
func (b upstreamBody) MarshalJSON() ([]byte, error) {
	type plain upstreamBody
	base, err := json.Marshal(plain(b))
	if err != nil {
		return nil, err
	}
	if len(b.Extra) == 0 {
		return base, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(base, &obj); err != nil {
		return nil, err
	}
	for k, v := range b.Extra {
		if _, taken := obj[k]; taken {
			continue
		}
		obj[k] = v
	}
	return json.Marshal(obj)
}

// upstreamOwnedFields are the body fields this codec serializes itself; Raw
// entries with these keys are never passed through (they would collide).
var upstreamOwnedFields = map[string]bool{
	"model": true, "instructions": true, "input": true, "tools": true,
	"tool_choice": true, "max_output_tokens": true, "temperature": true,
	"top_p": true, "stream": true,
}

type upFnDef struct {
	Type        string          `json:"type"` // "function" (flat Responses format)
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type upNamedToolChoice struct {
	Type string `json:"type"` // "function"
	Name string `json:"name"`
}

type upInputPart struct {
	Type     string `json:"type"` // input_text | output_text | input_image
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type upMessageItem struct {
	Type    string        `json:"type"` // "message"
	Role    string        `json:"role"`
	Content []upInputPart `json:"content"`
}

type upFunctionCallItem struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type upFunctionCallOutputItem struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// BuildUpstreamRequest builds the outbound POST {baseURL}/responses request
// with Bearer auth. model is the concrete upstream model name (the client
// alias is already resolved by the router).
func BuildUpstreamRequest(ctx context.Context, baseURL, apiKey, model string, req ir.Request, stream bool) (*http.Request, error) {
	items, instructions := upstreamInput(req.Messages)
	if len(items) == 0 {
		return nil, errors.New("responses: no input items to send")
	}
	body := upstreamBody{
		Model:           model,
		Instructions:    strings.Join(instructions, "\n"),
		Input:           items,
		MaxOutputTokens: req.Sampling.MaxTokens,
		Temperature:     req.Sampling.Temperature,
		TopP:            req.Sampling.TopP,
		Stream:          stream,
	}
	for _, t := range req.Tools {
		if t.Name == "" {
			continue
		}
		body.Tools = append(body.Tools, upFnDef{
			Type: "function", Name: t.Name, Description: t.Description, Parameters: t.ParametersJSON,
		})
	}
	switch {
	case req.ToolChoice == nil:
	case req.ToolChoice.Type == ir.ToolChoiceNamed:
		body.ToolChoice = upNamedToolChoice{Type: "function", Name: req.ToolChoice.Name}
	default: // auto / none / required share the Responses spellings
		body.ToolChoice = string(req.ToolChoice.Type)
	}
	body.Extra = passthroughExtras(req.Raw)

	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("responses: build upstream body: %w", err)
	}
	url := strings.TrimRight(baseURL, "/") + "/responses"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	h := hreq.Header
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("Authorization", "Bearer "+apiKey)
	return hreq, nil
}

// upstreamInput flattens IR messages into Responses input items and collects
// system texts for the top-level instructions field. IR message order is
// preserved, so a PartToolUse item always lands directly before the
// function_call_output item carrying its result. Assistant PartThinking is
// dropped: the upstream produces no reasoning signatures, so replaying the raw text
// would be rejected or poison the next turn.
func upstreamInput(msgs []ir.Message) ([]any, []string) {
	var items []any
	var system []string
	for _, m := range msgs {
		switch m.Role {
		case ir.RoleSystem:
			for _, p := range m.Content {
				if p.Type == ir.PartText && p.Text != "" {
					system = append(system, p.Text)
				}
			}
		case ir.RoleTool:
			for _, p := range m.Content {
				if p.Type != ir.PartToolResult {
					continue
				}
				items = append(items, upFunctionCallOutputItem{
					Type: "function_call_output", CallID: p.ToolCallID, Output: p.ToolOutput,
				})
			}
		case ir.RoleAssistant:
			var parts []upInputPart
			var calls []any
			for _, p := range m.Content {
				switch p.Type {
				case ir.PartThinking:
					// dropped: not replayable without a signature
				case ir.PartText:
					if p.Text != "" {
						parts = append(parts, upInputPart{Type: "output_text", Text: p.Text})
					}
				case ir.PartImage:
					parts = append(parts, upInputPart{Type: "input_image", ImageURL: p.ImageURL})
				case ir.PartToolUse:
					calls = append(calls, upFunctionCallItem{
						Type: "function_call", CallID: p.ToolCallID,
						Name: p.ToolName, Arguments: p.ToolArgsJSON,
					})
				}
			}
			if len(parts) > 0 {
				items = append(items, upMessageItem{Type: "message", Role: "assistant", Content: parts})
			}
			items = append(items, calls...)
		default: // user and pass-through roles
			role := m.Role
			if role == "" {
				role = ir.RoleUser
			}
			var parts []upInputPart
			for _, p := range m.Content {
				switch p.Type {
				case ir.PartText:
					if p.Text != "" {
						parts = append(parts, upInputPart{Type: "input_text", Text: p.Text})
					}
				case ir.PartImage:
					parts = append(parts, upInputPart{Type: "input_image", ImageURL: p.ImageURL})
				}
			}
			if len(parts) > 0 {
				items = append(items, upMessageItem{Type: "message", Role: string(role), Content: parts})
			}
		}
	}
	return items, system
}

// passthroughExtras selects Raw fields safe to re-send to a Responses
// upstream (store, metadata, reasoning, ...): valid non-null JSON whose key
// does not collide with a field this codec serializes itself.
func passthroughExtras(raw map[string]json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]json.RawMessage
	for k, v := range raw {
		if upstreamOwnedFields[k] || len(v) == 0 || string(v) == "null" || !json.Valid(v) {
			continue
		}
		if out == nil {
			out = map[string]json.RawMessage{}
		}
		out[k] = v
	}
	return out
}

// ---- inbound response body parsing ----

type upUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// toIR normalizes wire usage: input_tokens (cache-inclusive) → InputTokens,
// cached_tokens → CacheReadTokens, output_tokens (reasoning-inclusive) →
// OutputTokens, reasoning_tokens → ReasoningTokens. total_tokens is ignored
// (it is always input+output).
func (u *upUsage) toIR() ir.Usage {
	if u == nil {
		return ir.Usage{}
	}
	out := ir.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

type upOutputItem struct {
	Type string `json:"type"`
	// reasoning
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	// message
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type upResponse struct {
	ID                string          `json:"id"`
	Model             string          `json:"model"`
	Status            string          `json:"status"`
	Error             json.RawMessage `json:"error"`
	NewAPIError       json.RawMessage `json:"new_api_error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []upOutputItem `json:"output"`
	Usage  *upUsage       `json:"usage"`
}

// ParseUpstreamBody parses a complete non-streaming upstream body. Error
// bodies (the new-api envelope {"error":{...}} / {"new_api_error":{...}} the upstream
// emits, or status:"failed" responses) return *ir.UpstreamError with the best
// Kind/status guess from the envelope text; the server stamps the real HTTP
// status over it.
func ParseUpstreamBody(body []byte) (ir.Response, error) {
	var r upResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return ir.Response{}, unparseableUpstreamError(body, err)
	}
	if !isNull(r.Error) || !isNull(r.NewAPIError) || r.Status == "failed" {
		return ir.Response{}, errorFromEnvelope(firstRaw(r.Error, r.NewAPIError), body)
	}
	if r.Output == nil && r.Model == "" && r.ID == "" && r.Usage == nil && r.Status == "" {
		return ir.Response{}, unparseableUpstreamError(body, nil)
	}

	res := ir.Response{Model: r.Model, Usage: r.Usage.toIR()}
	sawTool := false
	for _, it := range r.Output {
		switch it.Type {
		case "reasoning":
			var sb strings.Builder
			for _, s := range it.Summary {
				sb.WriteString(s.Text)
			}
			if sb.Len() > 0 {
				res.Content = append(res.Content, ir.Part{Type: ir.PartThinking, Text: sb.String()})
			}
		case "message":
			var sb strings.Builder
			for _, c := range it.Content {
				if c.Type == "output_text" {
					sb.WriteString(c.Text)
				}
			}
			if sb.Len() > 0 {
				res.Content = append(res.Content, ir.Part{Type: ir.PartText, Text: sb.String()})
			}
		case "function_call":
			sawTool = true
			res.Content = append(res.Content, ir.Part{
				Type: ir.PartToolUse, ToolCallID: it.CallID,
				ToolName: it.Name, ToolArgsJSON: it.Arguments,
			})
		default:
			// unknown item types (web_search_call, ...) have no IR spelling
		}
	}

	res.FinishReason = ir.FinishStop
	switch {
	case r.Status == "incomplete" && r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "content_filter":
		res.FinishReason = ir.FinishContentFilter
	case r.Status == "incomplete":
		res.FinishReason = ir.FinishLength
	case sawTool:
		res.FinishReason = ir.FinishToolCalls
	}
	return res, nil
}

func firstRaw(a, b json.RawMessage) json.RawMessage {
	if !isNull(a) {
		return a
	}
	return b
}
