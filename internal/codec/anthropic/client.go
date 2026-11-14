package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"kleinia.io/llmgw/internal/ir"
)

// knownRequestFields are the top-level request fields ParseClientRequest
// consumes. Every other well-formed field (thinking, metadata, ...) is
// preserved verbatim in req.Raw for same-format upstream pass-through.
var knownRequestFields = map[string]bool{
	"model":          true,
	"system":         true,
	"messages":       true,
	"tools":          true,
	"tool_choice":    true,
	"max_tokens":     true,
	"temperature":    true,
	"top_p":          true,
	"top_k":          true,
	"stop_sequences": true,
	"stream":         true,
}

// ParseClientRequest decodes an anthropic /v1/messages request body into IR.
// Malformed JSON and a missing messages array are errors; everything else is
// tolerated (the upstream does not enforce max_tokens, and neither do we: a missing
// max_tokens defaults to 4096 instead of failing).
func ParseClientRequest(body []byte) (ir.Request, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return ir.Request{}, fmt.Errorf("anthropic: request body is not a JSON object: %w", err)
	}

	var req ir.Request
	if raw, ok := fields["model"]; ok {
		if err := json.Unmarshal(raw, &req.Model); err != nil {
			return ir.Request{}, errors.New("anthropic: model must be a string")
		}
	}

	// Top-level system (string or text-block array) becomes one leading
	// RoleSystem message.
	if raw, ok := fields["system"]; ok {
		if text := systemText(raw); text != "" {
			req.Messages = append(req.Messages, ir.Message{
				Role:    ir.RoleSystem,
				Content: []ir.Part{{Type: ir.PartText, Text: text}},
			})
		}
	}

	rawMsgs, ok := fields["messages"]
	if !ok || string(rawMsgs) == "null" {
		return ir.Request{}, errors.New("anthropic: field messages is required")
	}
	var wireMsgs []wireMessage
	if err := json.Unmarshal(rawMsgs, &wireMsgs); err != nil {
		return ir.Request{}, fmt.Errorf("anthropic: field messages must be an array: %w", err)
	}
	for _, wm := range wireMsgs {
		role := normalizeRole(wm.Role)
		var toolMsgs []ir.Message
		var parts []ir.Part
		if s, isStr := contentString(wm.Content); isStr {
			parts = []ir.Part{{Type: ir.PartText, Text: s}}
		} else {
			var blocks []wireBlock
			if err := json.Unmarshal(wm.Content, &blocks); err == nil {
				for _, b := range blocks {
					switch b.Type {
					case "text":
						parts = append(parts, ir.Part{Type: ir.PartText, Text: b.Text})
					case "thinking":
						parts = append(parts, ir.Part{Type: ir.PartThinking, Text: b.Thinking})
					case "image", "image_url":
						if p, valid := imagePartFromBlock(b); valid {
							parts = append(parts, p)
						}
					case "tool_use":
						parts = append(parts, ir.Part{
							Type:         ir.PartToolUse,
							ToolCallID:   b.ID,
							ToolName:     b.Name,
							ToolArgsJSON: compactArgs(b.Input),
						})
					case "tool_result":
						// Tool results leave the user turn as standalone
						// RoleTool messages (they must lead the next user
						// message on the wire, which the upstream builder
						// restores).
						toolMsgs = append(toolMsgs, ir.Message{
							Role: ir.RoleTool,
							Content: []ir.Part{{
								Type:       ir.PartToolResult,
								ToolCallID: b.ToolUseID,
								ToolOutput: toolResultOutput(b.Content),
								IsError:    b.IsError,
							}},
						})
					}
				}
			}
		}
		req.Messages = append(req.Messages, toolMsgs...)
		if len(parts) > 0 {
			req.Messages = append(req.Messages, ir.Message{Role: role, Content: parts})
		}
	}

	if raw, ok := fields["tools"]; ok && string(raw) != "null" {
		var wire []wireTool
		if err := json.Unmarshal(raw, &wire); err != nil {
			return ir.Request{}, fmt.Errorf("anthropic: field tools must be an array: %w", err)
		}
		for _, t := range wire {
			if t.Name == "" {
				continue // server tools (web_search_*) have no IR spelling
			}
			req.Tools = append(req.Tools, ir.Tool{
				Name:           t.Name,
				Description:    t.Description,
				ParametersJSON: t.InputSchema,
			})
		}
	}
	if raw, ok := fields["tool_choice"]; ok {
		if tc, valid := parseToolChoice(raw); valid {
			req.ToolChoice = &tc
		}
	}

	req.Sampling.MaxTokens = defaultMaxTokens
	if err := decodeOpt(fields, "max_tokens", &req.Sampling.MaxTokens); err != nil {
		return ir.Request{}, err
	}
	if err := decodeOpt(fields, "temperature", &req.Sampling.Temperature); err != nil {
		return ir.Request{}, err
	}
	if err := decodeOpt(fields, "top_p", &req.Sampling.TopP); err != nil {
		return ir.Request{}, err
	}
	if err := decodeOpt(fields, "top_k", &req.Sampling.TopK); err != nil {
		return ir.Request{}, err
	}
	if raw, ok := fields["stop_sequences"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &req.Sampling.Stop); err != nil {
			return ir.Request{}, errors.New("anthropic: stop_sequences must be an array of strings")
		}
	}
	if err := decodeOpt(fields, "stream", &req.Stream); err != nil {
		return ir.Request{}, err
	}

	for k, v := range fields {
		if knownRequestFields[k] || len(v) == 0 || string(v) == "null" || !json.Valid(v) {
			continue
		}
		if req.Raw == nil {
			req.Raw = map[string]json.RawMessage{}
		}
		req.Raw[k] = v
	}
	return req, nil
}

// wireMessage is one messages[] entry with the content kept raw: content is
// either a plain string or an array of blocks.
type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// decodeOpt unmarshals an optional top-level field into dst; absent and null
// leave dst untouched.
func decodeOpt[T any](fields map[string]json.RawMessage, key string, dst *T) error {
	raw, ok := fields[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("anthropic: field %s has the wrong type: %w", key, err)
	}
	return nil
}

// systemText accepts both anthropic system shapes — a plain string or an
// array of content blocks — and returns the text (multiple blocks joined
// with newlines).
func systemText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// normalizeRole maps a wire role to an IR role; anything unrecognized is
// treated as user (anthropic only defines user/assistant here, and a stray
// "system" role still reads correctly).
func normalizeRole(role string) ir.Role {
	switch role {
	case "assistant":
		return ir.RoleAssistant
	case "system":
		return ir.RoleSystem
	default:
		return ir.RoleUser
	}
}

// contentString reports whether a message content field was a plain string.
func contentString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// imagePartFromBlock converts an anthropic image block (base64 or URL
// source; a non-standard image_url variant is tolerated too) into an IR
// image part. Base64 images become data: URLs.
func imagePartFromBlock(b wireBlock) (ir.Part, bool) {
	if b.Source != nil {
		switch b.Source.Type {
		case "base64":
			if b.Source.MediaType != "" && b.Source.Data != "" {
				return ir.Part{
					Type:     ir.PartImage,
					ImageURL: "data:" + b.Source.MediaType + ";base64," + b.Source.Data,
				}, true
			}
		case "url":
			if b.Source.URL != "" {
				return ir.Part{Type: ir.PartImage, ImageURL: b.Source.URL}, true
			}
		}
		return ir.Part{}, false
	}
	if b.ImageURL != nil && b.ImageURL.URL != "" {
		return ir.Part{Type: ir.PartImage, ImageURL: b.ImageURL.URL}, true
	}
	return ir.Part{}, false
}

// parseToolChoice maps anthropic tool_choice (auto | any | tool | none) to
// the IR ToolChoice; unrecognized shapes are dropped.
func parseToolChoice(raw json.RawMessage) (ir.ToolChoice, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return toolChoiceFor(s, "")
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil || tc.Type == "" {
		return ir.ToolChoice{}, false
	}
	return toolChoiceFor(tc.Type, tc.Name)
}

func toolChoiceFor(typ, name string) (ir.ToolChoice, bool) {
	switch typ {
	case "auto":
		return ir.ToolChoice{Type: ir.ToolChoiceAuto}, true
	case "any":
		return ir.ToolChoice{Type: ir.ToolChoiceRequired}, true
	case "tool":
		return ir.ToolChoice{Type: ir.ToolChoiceNamed, Name: name}, true
	case "none":
		return ir.ToolChoice{Type: ir.ToolChoiceNone}, true
	}
	return ir.ToolChoice{}, false
}

// clientMessage is the complete non-streaming response envelope.
type clientMessage struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"` // "message"
	Role         string    `json:"role"` // "assistant"
	Model        string    `json:"model"`
	Content      []any     `json:"content"`
	StopReason   string    `json:"stop_reason"`
	StopSequence *string   `json:"stop_sequence"`
	Usage        wireUsage `json:"usage"`
}

// WriteClientResponse writes a complete non-streaming anthropic message.
// IR input tokens include the cache-read carve-out, anthropic's do not: the
// difference lands in cache_read_input_tokens.
func WriteClientResponse(w http.ResponseWriter, res ir.Response) error {
	content := make([]any, 0, len(res.Content))
	for _, p := range res.Content {
		switch p.Type {
		case ir.PartText:
			content = append(content, textBlock{Type: "text", Text: p.Text})
		case ir.PartThinking:
			// the upstream carries no signature on thinking blocks; the field stays
			// absent.
			content = append(content, thinkingBlock{Type: "thinking", Thinking: p.Text})
		case ir.PartToolUse:
			content = append(content, toolUseBlock{
				Type:  "tool_use",
				ID:    p.ToolCallID,
				Name:  p.ToolName,
				Input: rawToolInput(p.ToolArgsJSON),
			})
		}
	}
	body, err := json.Marshal(clientMessage{
		ID:           newMessageID(),
		Type:         "message",
		Role:         "assistant",
		Model:        res.Model,
		Content:      content,
		StopReason:   finishFromIR(res.FinishReason),
		StopSequence: nil,
		Usage:        usageFromIR(res.Usage),
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	return err
}

type errorEnvelope struct {
	Type  string    `json:"type"` // "error"
	Error errorBody `json:"error"`
}

type errorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// WriteClientError writes a non-streaming error in the anthropic envelope,
// keeping the caller's HTTP status semantics (401/429/400/404/5xx).
func WriteClientError(w http.ResponseWriter, status int, kind, msg string) error {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	etype, message := clientErrorType(status, kind, msg)
	body, err := json.Marshal(errorEnvelope{
		Type:  "error",
		Error: errorBody{Type: etype, Message: message},
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}

// clientErrorType maps a normalized error kind — with the HTTP status as
// fallback for unrecognized kinds — to the anthropic error type. IR kinds
// whose anthropic rendering differs from the normalized kind get a message
// note.
func clientErrorType(status int, kind, msg string) (string, string) {
	switch kind {
	case ir.ErrInvalidKey:
		return "authentication_error", msg
	case ir.ErrRateLimited, ir.ErrQuota:
		return "rate_limit_error", msg
	case ir.ErrContextLength:
		return "invalid_request_error", "prompt is too long for this model's context window: " + msg
	case ir.ErrNotFound, "model_not_found":
		return "not_found_error", msg
	}
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error", msg
	case http.StatusUnauthorized:
		return "authentication_error", msg
	case http.StatusForbidden:
		return "permission_error", msg
	case http.StatusNotFound:
		return "not_found_error", msg
	case http.StatusTooManyRequests:
		return "rate_limit_error", msg
	default:
		return "api_error", msg
	}
}

// ---- client-side SSE encoder ----

// ClientStreamEncoder writes anthropic SSE. The constructor only sets
// headers; message_start is written lazily on the first Emit so a request
// that fails before any output can still be answered with a plain error.
type ClientStreamEncoder struct {
	w         http.ResponseWriter
	id        string
	model     string
	opened    bool
	closed    bool
	sawError  bool
	blocks    int    // content blocks opened so far
	openKind  string // "", "text", "thinking", "tool_use"
	openIndex int

	usage      ir.Usage
	haveUsage  bool
	finish     ir.Finish
	wroteDelta bool
}

// NewClientStreamEncoder prepares the SSE response headers. The stream opens
// (message_start) on the first Emit.
func NewClientStreamEncoder(w http.ResponseWriter) *ClientStreamEncoder {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	return &ClientStreamEncoder{w: w, id: newMessageID()}
}

// Emit writes one IR event in anthropic stream form. Text/thinking/tool
// fragments are grouped into indexed content blocks; a change of kind closes
// the current block and opens the next one.
func (e *ClientStreamEncoder) Emit(ev ir.StreamEvent) error {
	if e.closed {
		return nil
	}
	if ev.Model != "" && !e.opened {
		e.model = ev.Model
	}
	if err := e.ensureStarted(); err != nil {
		return err
	}
	switch ev.Kind {
	case ir.EvStart:
		return nil
	case ir.EvTextDelta:
		if e.openKind != "text" {
			if err := e.startBlock(textBlock{Type: "text"}, "text"); err != nil {
				return err
			}
		}
		return e.writeSSE("content_block_delta", contentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: e.openIndex,
			Delta: deltaPayload{Type: "text_delta", Text: ev.Text},
		})
	case ir.EvThinkingDelta:
		if e.openKind != "thinking" {
			if err := e.startBlock(thinkingBlock{Type: "thinking"}, "thinking"); err != nil {
				return err
			}
		}
		return e.writeSSE("content_block_delta", contentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: e.openIndex,
			Delta: deltaPayload{Type: "thinking_delta", Thinking: ev.Text},
		})
	case ir.EvToolStart:
		if err := e.startBlock(toolUseBlock{
			Type:  "tool_use",
			ID:    ev.ToolID,
			Name:  ev.ToolName,
			Input: json.RawMessage("{}"),
		}, "tool_use"); err != nil {
			return err
		}
		return nil
	case ir.EvToolDelta:
		if e.openKind != "tool_use" {
			return nil // orphan fragment with no open tool block
		}
		return e.writeSSE("content_block_delta", contentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: e.openIndex,
			Delta: deltaPayload{Type: "input_json_delta", PartialJSON: ev.Text},
		})
	case ir.EvToolEnd:
		if e.openKind != "tool_use" {
			return nil
		}
		return e.stopBlock()
	case ir.EvUsage:
		e.usage = ev.Usage
		e.haveUsage = true
		return nil
	case ir.EvFinish:
		e.finish = ev.Finish
		return e.writeDelta()
	case ir.EvError:
		e.sawError = true
		etype, msg := clientErrorType(ev.HTTPStatus, ev.ErrCode, ev.ErrMsg)
		return e.writeSSE("error", errorEnvelope{Type: "error", Error: errorBody{Type: etype, Message: msg}})
	default:
		return nil
	}
}

// Close terminates the stream: it closes any open content block, writes
// message_delta (unless EvFinish already did, or the stream errored) and
// message_stop.
func (e *ClientStreamEncoder) Close(finish ir.Finish) error {
	if e.closed {
		return nil
	}
	if err := e.ensureStarted(); err != nil { // empty stream stays well-formed
		return err
	}
	e.closed = true
	if e.sawError {
		// The error event already terminated the message for the client.
		if err := e.stopBlock(); err != nil {
			return err
		}
		return e.writeSSE("message_stop", messageStopEvent{Type: "message_stop"})
	}
	if !e.wroteDelta {
		e.finish = finish
		if err := e.writeDelta(); err != nil {
			return err
		}
	}
	return e.writeSSE("message_stop", messageStopEvent{Type: "message_stop"})
}

// writeSSE frames one named event, "event: <name>\ndata: <json>\n\n".
func (e *ClientStreamEncoder) writeSSE(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, data)
	return err
}

func (e *ClientStreamEncoder) ensureStarted() error {
	if e.opened {
		return nil
	}
	e.opened = true
	return e.writeSSE("message_start", messageStartEvent{
		Type: "message_start",
		Message: startMessage{
			ID:      e.id,
			Type:    "message",
			Role:    "assistant",
			Model:   e.model,
			Content: []any{},
			// input_tokens is still unknown here (anthropic itself only
			// sends an estimate at this point); message_delta carries the
			// authoritative numbers at the end.
			Usage: wireUsage{},
		},
	})
}

// startBlock closes any open block, then opens the next indexed one.
func (e *ClientStreamEncoder) startBlock(block any, kind string) error {
	if err := e.stopBlock(); err != nil {
		return err
	}
	e.openIndex = e.blocks
	e.blocks++
	e.openKind = kind
	return e.writeSSE("content_block_start", contentBlockStartEvent{
		Type:         "content_block_start",
		Index:        e.openIndex,
		ContentBlock: block,
	})
}

func (e *ClientStreamEncoder) stopBlock() error {
	if e.openKind == "" {
		return nil
	}
	index := e.openIndex
	e.openKind = ""
	return e.writeSSE("content_block_stop", contentBlockStopEvent{
		Type:  "content_block_stop",
		Index: index,
	})
}

// writeDelta closes the open block and writes message_delta with the finish
// reason and the converted usage (zeros when the upstream never reported
// any — the server decides on fallback estimates).
func (e *ClientStreamEncoder) writeDelta() error {
	if e.wroteDelta {
		return nil
	}
	if err := e.stopBlock(); err != nil {
		return err
	}
	e.wroteDelta = true
	u := wireUsage{}
	if e.haveUsage {
		u = usageFromIR(e.usage)
	}
	return e.writeSSE("message_delta", messageDeltaEvent{
		Type:  "message_delta",
		Usage: u,
		Delta: messageDeltaBody{StopReason: finishFromIR(e.finish), StopSequence: nil},
	})
}

// ---- stream wire event shapes ----

type messageStartEvent struct {
	Type    string       `json:"type"`
	Message startMessage `json:"message"`
}

type startMessage struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`
	Role    string    `json:"role"`
	Model   string    `json:"model"`
	Content []any     `json:"content"`
	Usage   wireUsage `json:"usage"`
}

type contentBlockStartEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type contentBlockDeltaEvent struct {
	Type  string       `json:"type"`
	Index int          `json:"index"`
	Delta deltaPayload `json:"delta"`
}

type deltaPayload struct {
	Type        string `json:"type"` // "text_delta" | "thinking_delta" | "input_json_delta"
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type contentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type messageDeltaEvent struct {
	Type  string           `json:"type"`
	Usage wireUsage        `json:"usage"`
	Delta messageDeltaBody `json:"delta"`
}

type messageDeltaBody struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type messageStopEvent struct {
	Type string `json:"type"`
}
