package anthropic

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

// BuildUpstreamRequest builds the outbound POST {baseURL}/messages request
// with anthropic auth headers set. model is the concrete upstream model name
// (the client alias is already resolved by the router).
func BuildUpstreamRequest(ctx context.Context, baseURL, apiKey, model string, req ir.Request, stream bool) (*http.Request, error) {
	body, err := marshalUpstreamBody(model, req, stream)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build upstream body: %w", err)
	}
	url := strings.TrimRight(baseURL, "/") + "/messages"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	h := hreq.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	}
	h.Set("x-api-key", apiKey)
	// new-api upstreams do not enforce the version header, but real Anthropic does.
	h.Set("anthropic-version", anthropicVersion)
	return hreq, nil
}

// upstreamBody is the outbound request body. Extra carries pass-through
// fields (thinking, metadata, ...) merged in at marshal time.
type upstreamBody struct {
	Model         string              `json:"model"`
	System        string              `json:"system,omitempty"`
	Messages      []upstreamMessage   `json:"messages"`
	MaxTokens     int64               `json:"max_tokens"`
	StopSequences []string            `json:"stop_sequences,omitempty"`
	Temperature   *float64            `json:"temperature,omitempty"`
	TopP          *float64            `json:"top_p,omitempty"`
	TopK          *int64              `json:"top_k,omitempty"`
	Tools         []wireTool          `json:"tools,omitempty"`
	ToolChoice    *upstreamToolChoice `json:"tool_choice,omitempty"`
	Stream        bool                `json:"stream,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// MarshalJSON merges the Extra pass-through fields after the owned fields;
// owned fields always win on collision.
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

type upstreamMessage struct {
	Role    string `json:"role"` // "user" | "assistant"
	Content []any  `json:"content"`
}

type upstreamToolChoice struct {
	Type string `json:"type"`           // "auto" | "any" | "tool"
	Name string `json:"name,omitempty"` // only for "tool"
}

// upstreamOwnedFields are the body fields this codec serializes itself; Raw
// entries with these keys are never passed through (they would collide).
var upstreamOwnedFields = map[string]bool{
	"model": true, "system": true, "messages": true, "tools": true,
	"tool_choice": true, "max_tokens": true, "temperature": true,
	"top_p": true, "top_k": true, "stop_sequences": true, "stream": true,
}

func marshalUpstreamBody(model string, req ir.Request, stream bool) ([]byte, error) {
	msgs, system := upstreamMessages(req.Messages)
	if len(msgs) == 0 {
		return nil, errors.New("no user/assistant messages to send")
	}
	body := upstreamBody{
		Model:         model,
		Messages:      msgs,
		MaxTokens:     req.Sampling.MaxTokens,
		StopSequences: req.Sampling.Stop,
		Temperature:   req.Sampling.Temperature,
		TopP:          req.Sampling.TopP,
		TopK:          req.Sampling.TopK,
		Stream:        stream,
	}
	// max_tokens is required by the anthropic spec; the parser defaults
	// missing values to 4096, this catches hand-built IR too.
	if body.MaxTokens <= 0 {
		body.MaxTokens = defaultMaxTokens
	}
	if len(system) > 0 {
		body.System = strings.Join(system, "\n")
	}
	for _, t := range req.Tools {
		if t.Name == "" {
			continue
		}
		body.Tools = append(body.Tools, wireTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.ParametersJSON,
		})
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case ir.ToolChoiceAuto:
			body.ToolChoice = &upstreamToolChoice{Type: "auto"}
		case ir.ToolChoiceRequired: // anthropic spells "required" as "any"
			body.ToolChoice = &upstreamToolChoice{Type: "any"}
		case ir.ToolChoiceNamed:
			body.ToolChoice = &upstreamToolChoice{Type: "tool", Name: tc.Name}
			// "none" has no anthropic spelling; it is omitted.
		}
	}
	body.Extra = passthroughExtras(req.Raw)
	return json.Marshal(body)
}

// upstreamMessages reassembles IR messages into anthropic messages:
// RoleSystem texts are collected for the top-level system field, RoleTool
// messages are merged back into the next user message as leading tool_result
// blocks (a trailing RoleTool with no following user turn gets its own user
// message), and assistant thinking parts are dropped — they cannot be
// replayed without a cryptographic signature, which the upstream does not produce.
func upstreamMessages(msgs []ir.Message) ([]upstreamMessage, []string) {
	var out []upstreamMessage
	var system []string
	var pendingTools []any
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
				if p.Type == ir.PartToolResult {
					pendingTools = append(pendingTools, toolResultBlock{
						Type:      "tool_result",
						ToolUseID: p.ToolCallID,
						Content:   p.ToolOutput,
						IsError:   p.IsError,
					})
				}
			}
		default:
			role := "user"
			if m.Role == ir.RoleAssistant {
				role = "assistant"
			}
			var blocks []any
			if role == "user" && len(pendingTools) > 0 {
				blocks = append(blocks, pendingTools...)
				pendingTools = nil
			}
			for _, p := range m.Content {
				switch p.Type {
				case ir.PartText:
					blocks = append(blocks, textBlock{Type: "text", Text: p.Text})
				case ir.PartImage:
					if blk, ok := imageBlockFromURL(p.ImageURL); ok {
						blocks = append(blocks, blk)
					}
				case ir.PartToolUse:
					blocks = append(blocks, toolUseBlock{
						Type:  "tool_use",
						ID:    p.ToolCallID,
						Name:  p.ToolName,
						Input: rawToolInput(p.ToolArgsJSON),
					})
				case ir.PartToolResult:
					blocks = append(blocks, toolResultBlock{
						Type:      "tool_result",
						ToolUseID: p.ToolCallID,
						Content:   p.ToolOutput,
						IsError:   p.IsError,
					})
				case ir.PartThinking:
					// dropped: not replayable without a signature
				}
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, upstreamMessage{Role: role, Content: blocks})
		}
	}
	if len(pendingTools) > 0 {
		out = append(out, upstreamMessage{Role: "user", Content: pendingTools})
	}
	return out, system
}

// passthroughExtras selects Raw fields that are safe to re-send to an
// anthropic upstream (thinking, metadata, ...): valid non-null JSON whose
// key does not collide with a field this codec serializes itself.
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

// imageBlockFromURL converts an IR image part back into an anthropic image
// block: data: URLs become base64 sources, http(s) URLs become url sources.
func imageBlockFromURL(url string) (imageBlock, bool) {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		const marker = ";base64,"
		if m := strings.Index(rest, marker); m >= 0 {
			media, b64 := rest[:m], rest[m+len(marker):]
			if media != "" && b64 != "" {
				return imageBlock{
					Type:   "image",
					Source: imageSource{Type: "base64", MediaType: media, Data: b64},
				}, true
			}
		}
		return imageBlock{}, false
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return imageBlock{Type: "image", Source: imageSource{Type: "url", URL: url}}, true
	}
	return imageBlock{}, false
}

// ParseUpstreamBody parses a complete non-streaming upstream response body.
// Error bodies (both envelopes the upstream emits — the OpenAI-style {"error":{...}}
// and the official {"type":"error",...}) return *ir.UpstreamError.
func ParseUpstreamBody(body []byte) (ir.Response, error) {
	var envelope struct {
		Type       string          `json:"type"`
		Error      json.RawMessage `json:"error"`
		Model      string          `json:"model"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      *upstreamUsage  `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ir.Response{}, fmt.Errorf("anthropic: upstream body is not JSON: %w", err)
	}
	if (len(envelope.Error) > 0 && string(envelope.Error) != "null") || envelope.Type == "error" {
		return ir.Response{}, upstreamErrorFromBody(envelope.Error)
	}
	if envelope.Type != "message" {
		return ir.Response{}, fmt.Errorf("anthropic: upstream body is not a message (type %q)", envelope.Type)
	}
	res := ir.Response{
		Model:        envelope.Model,
		Content:      partsFromBlocks(envelope.Content),
		FinishReason: finishToIR(envelope.StopReason),
	}
	if envelope.Usage != nil {
		res.Usage = envelope.Usage.toIR()
	}
	return res, nil
}

// partsFromBlocks converts anthropic content blocks (or a bare string) into
// IR parts. Unknown block types (redacted_thinking, server tool blocks)
// have no IR spelling and are dropped.
func partsFromBlocks(raw json.RawMessage) []ir.Part {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if s, ok := contentString(raw); ok {
		return []ir.Part{{Type: ir.PartText, Text: s}}
	}
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	parts := make([]ir.Part, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, ir.Part{Type: ir.PartText, Text: b.Text})
		case "thinking":
			parts = append(parts, ir.Part{Type: ir.PartThinking, Text: b.Thinking})
		case "tool_use":
			parts = append(parts, ir.Part{
				Type:         ir.PartToolUse,
				ToolCallID:   b.ID,
				ToolName:     b.Name,
				ToolArgsJSON: compactArgs(b.Input),
			})
		case "image", "image_url":
			if p, ok := imagePartFromBlock(b); ok {
				parts = append(parts, p)
			}
		}
	}
	return parts
}

// upstreamErrorFromBody classifies an error envelope into *ir.UpstreamError.
// the upstream's anthropic endpoint answers with the OpenAI-style envelope whose
// type is always "new_api_error", so the real class is recovered from the
// message text; the official anthropic error types are mapped directly.
func upstreamErrorFromBody(raw json.RawMessage) *ir.UpstreamError {
	var inner struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    any    `json:"code"`
	}
	_ = json.Unmarshal(raw, &inner)
	code, _ := inner.Code.(string)
	msg := inner.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	return classifyUpstreamError(inner.Type, code, msg)
}

func classifyUpstreamError(etype, code, message string) *ir.UpstreamError {
	switch {
	case strings.Contains(message, "无效的令牌"):
		return &ir.UpstreamError{Status: http.StatusUnauthorized, Kind: ir.ErrInvalidKey, Message: message}
	case strings.Contains(message, "无权访问模型"):
		// the upstream merges "model unknown" and "token lacks access" into one 403;
		// both are key problems from the gateway's point of view.
		return &ir.UpstreamError{Status: http.StatusForbidden, Kind: ir.ErrInvalidKey, Message: message}
	case strings.Contains(message, "is required"),
		strings.Contains(message, "invalid JSON"),
		strings.Contains(message, "无效的请求"):
		return &ir.UpstreamError{Status: http.StatusBadRequest, Kind: ir.ErrUpstream, Message: message}
	}
	if code == "context_length_exceeded" || contextLengthMessage(message) {
		return &ir.UpstreamError{Status: http.StatusBadRequest, Kind: ir.ErrContextLength, Message: message}
	}
	switch etype {
	case "authentication_error":
		return &ir.UpstreamError{Status: http.StatusUnauthorized, Kind: ir.ErrInvalidKey, Message: message}
	case "permission_error":
		return &ir.UpstreamError{Status: http.StatusForbidden, Kind: ir.ErrInvalidKey, Message: message}
	case "rate_limit_error":
		return &ir.UpstreamError{Status: http.StatusTooManyRequests, Kind: ir.ErrRateLimited, Message: message, Retryable: true}
	case "not_found_error":
		return &ir.UpstreamError{Status: http.StatusNotFound, Kind: ir.ErrNotFound, Message: message}
	case "invalid_request_error":
		return &ir.UpstreamError{Status: http.StatusBadRequest, Kind: ir.ErrUpstream, Message: message}
	case "overloaded_error":
		return &ir.UpstreamError{Status: 529, Kind: ir.ErrUpstream, Message: message, Retryable: true}
	case "api_error":
		return &ir.UpstreamError{Status: http.StatusInternalServerError, Kind: ir.ErrUpstream, Message: message, Retryable: true}
	case "timeout_error":
		return &ir.UpstreamError{Status: http.StatusGatewayTimeout, Kind: ir.ErrUpstream, Message: message, Retryable: true}
	}
	// Unknown envelope: assume a transient gateway problem; the server
	// overrides status/retryable from the real HTTP response anyway.
	return &ir.UpstreamError{Status: http.StatusBadGateway, Kind: ir.ErrUpstream, Message: message, Retryable: true}
}

// contextLengthMessage recognizes token-length complaints in either
// language; the upstream relays its provider's text verbatim.
func contextLengthMessage(msg string) bool {
	m := strings.ToLower(msg)
	for _, hint := range []string{
		"context length", "context window", "maximum context",
		"too long", "too many tokens", "token limit",
	} {
		if strings.Contains(m, hint) {
			return true
		}
	}
	return false
}

// ---- upstream SSE parser ----

// UpstreamStreamParser converts an anthropic SSE stream into IR events.
// The canonical chain is message_start → (content_block_start → deltas →
// content_block_stop)×N → message_delta (authoritative cumulative usage +
// stop_reason) → message_stop. message_start's input_tokens is only an
// estimate on the sampled upstream (observed 10 vs a final 34), so usage is taken from
// message_delta with last-wins semantics. Everything after message_stop is
// ignored.
type UpstreamStreamParser struct {
	eventName string // pending "event:" line, used when data lacks "type"
	blocks    map[int]*streamBlock
	stopped   bool
}

type streamBlock struct {
	kind     string // "text" | "thinking" | "tool_use" | other
	toolID   string
	toolName string
}

func NewUpstreamStreamParser() *UpstreamStreamParser {
	return &UpstreamStreamParser{blocks: make(map[int]*streamBlock)}
}

// Line consumes one raw SSE line — event/data framing, comments, keep-alives
// and blank separators included — and returns the IR events it produced
// (0..n). Malformed data frames are skipped rather than fatal: the rest of
// the stream usually still parses.
func (p *UpstreamStreamParser) Line(line string) ([]ir.StreamEvent, error) {
	switch {
	case p.stopped, line == "":
		return nil, nil
	case strings.HasPrefix(line, ":"):
		return nil, nil // comment / keep-alive
	case strings.HasPrefix(line, "event:"):
		p.eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return nil, nil
	case strings.HasPrefix(line, "data:"):
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			return nil, nil // terminal sentinel / empty frame
		}
		return p.handleData(payload), nil
	default:
		return nil, nil // unknown line; tolerate trailing garbage
	}
}

func (p *UpstreamStreamParser) handleData(payload string) []ir.StreamEvent {
	var frame struct {
		Type    string    `json:"type"`
		Message *struct { // message_start
			Model string         `json:"model"`
			Usage *upstreamUsage `json:"usage"`
		} `json:"message"`
		Index        int             `json:"index"` // content_block_*
		ContentBlock json.RawMessage `json:"content_block"`
		Delta        *struct {
			Type        string  `json:"type"`
			Text        string  `json:"text"`
			Thinking    string  `json:"thinking"`
			PartialJSON string  `json:"partial_json"`
			StopReason  *string `json:"stop_reason"`
		} `json:"delta"`
		Usage json.RawMessage `json:"usage"` // message_delta (cumulative, authoritative)
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return nil
	}
	etype := frame.Type
	if etype == "" {
		etype = p.eventName
	}
	switch etype {
	case "message_start":
		if frame.Message == nil {
			return nil
		}
		return []ir.StreamEvent{{Kind: ir.EvStart, Model: frame.Message.Model}}
	case "content_block_start":
		var blk struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(frame.ContentBlock, &blk)
		p.blocks[frame.Index] = &streamBlock{kind: blk.Type, toolID: blk.ID, toolName: blk.Name}
		if blk.Type == "tool_use" {
			return []ir.StreamEvent{{Kind: ir.EvToolStart, ToolID: blk.ID, ToolName: blk.Name}}
		}
		return nil
	case "content_block_delta":
		if frame.Delta == nil {
			return nil
		}
		switch frame.Delta.Type {
		case "text_delta":
			return []ir.StreamEvent{{Kind: ir.EvTextDelta, Text: frame.Delta.Text}}
		case "thinking_delta":
			return []ir.StreamEvent{{Kind: ir.EvThinkingDelta, Text: frame.Delta.Thinking}}
		case "input_json_delta":
			if blk := p.blocks[frame.Index]; blk != nil && blk.kind == "tool_use" {
				return []ir.StreamEvent{{Kind: ir.EvToolDelta, ToolID: blk.toolID, Text: frame.Delta.PartialJSON}}
			}
			return nil
		default: // signature_delta and future kinds
			return nil
		}
	case "content_block_stop":
		blk := p.blocks[frame.Index]
		delete(p.blocks, frame.Index)
		if blk != nil && blk.kind == "tool_use" {
			return []ir.StreamEvent{{Kind: ir.EvToolEnd, ToolID: blk.toolID}}
		}
		return nil
	case "message_delta":
		var out []ir.StreamEvent
		if len(frame.Usage) > 0 && string(frame.Usage) != "null" {
			var u upstreamUsage
			if err := json.Unmarshal(frame.Usage, &u); err == nil {
				out = append(out, ir.StreamEvent{Kind: ir.EvUsage, Usage: u.toIR()})
			}
		}
		if frame.Delta != nil && frame.Delta.StopReason != nil {
			out = append(out, ir.StreamEvent{Kind: ir.EvFinish, Finish: finishToIR(*frame.Delta.StopReason)})
		}
		return out
	case "message_stop":
		p.stopped = true
		return nil
	case "error":
		if frame.Error == nil {
			return nil
		}
		ue := classifyUpstreamError(frame.Error.Type, "", frame.Error.Message)
		return []ir.StreamEvent{{
			Kind:       ir.EvError,
			ErrCode:    ue.Kind,
			ErrMsg:     ue.Message,
			Retryable:  ue.Retryable,
			HTTPStatus: ue.Status,
		}}
	default: // ping and future event kinds
		return nil
	}
}
