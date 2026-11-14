package openairesponses

import (
	"encoding/json"
	"strings"

	"kleinia.io/llmgw/internal/ir"
)

// UpstreamStreamParser converts a Responses named-event SSE stream into IR
// stream events. Feed it one raw line at a time (as read from the socket,
// with or without trailing newline): "event: <name>" lines, "data: {json}"
// payloads, blank separators, ": keep-alive" comments and the "data: [DONE]"
// sentinel.
//
// Event mapping (everything else — *.done, content_part.*, in_progress,
// annotation events — is close-out framing with no IR spelling and ignored):
//
//	response.created                        → EvStart(model)
//	response.reasoning_summary_text.delta   → EvThinkingDelta
//	response.output_text.delta              → EvTextDelta
//	response.output_item.added (function_call) → EvToolStart
//	response.function_call_arguments.delta  → EvToolDelta
//	response.function_call_arguments.done   → EvToolEnd
//	response.completed                      → EvUsage + EvFinish
//	response.incomplete                     → EvUsage + EvFinish(length|content_filter)
//	response.failed / response.error        → EvError
//
// Tool events address items by item_id — the item's id (fc_...), not the
// call_id — so the parser keeps an item-id → call_id table from each
// output_item.added. Tool calls still open when a terminal event arrives are
// closed first (IR contract: EvToolEnd precedes EvUsage/EvFinish). Malformed
// data frames are skipped rather than fatal; everything after a terminal
// event is ignored.
type UpstreamStreamParser struct {
	eventName string // pending "event:" line, used when data lacks "type"

	started bool
	done    bool
	sawTool bool

	itemTool  map[string]string // item id (fc_...) -> call_id
	knownTool map[string]bool   // every call_id ever opened
	open      []string          // open call_ids in start order
	closed    map[string]bool
}

// NewUpstreamStreamParser returns a parser in its initial state.
func NewUpstreamStreamParser() *UpstreamStreamParser {
	return &UpstreamStreamParser{
		itemTool:  make(map[string]string),
		knownTool: make(map[string]bool),
		closed:    make(map[string]bool),
	}
}

// Line consumes one raw SSE line and returns the IR events it produced
// (0..n). It never fails: unparseable frames are skipped so the rest of the
// stream usually still parses.
func (p *UpstreamStreamParser) Line(line string) ([]ir.StreamEvent, error) {
	if p.done {
		return nil, nil
	}
	l := strings.TrimSpace(line)
	switch {
	case l == "", strings.HasPrefix(l, ":"): // separator or keep-alive comment
		return nil, nil
	case strings.HasPrefix(l, "event:"):
		p.eventName = strings.TrimSpace(strings.TrimPrefix(l, "event:"))
		return nil, nil
	case strings.HasPrefix(l, "data:"):
		payload := strings.TrimSpace(strings.TrimPrefix(l, "data:"))
		if payload == "" {
			return nil, nil
		}
		if payload == "[DONE]" {
			p.done = true
			return p.closeOpenTools(), nil
		}
		return p.handleData(payload), nil
	default:
		return nil, nil // unknown line; tolerate trailing garbage
	}
}

type upStreamResponse struct {
	Model             string          `json:"model"`
	Status            string          `json:"status"`
	Usage             *upUsage        `json:"usage"`
	Error             json.RawMessage `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

type upStreamFrame struct {
	Type     string            `json:"type"`
	Code     string            `json:"code"`    // response.error frames
	Message  string            `json:"message"` // response.error frames
	ItemID   string            `json:"item_id"`
	Delta    string            `json:"delta"`
	Response *upStreamResponse `json:"response"`
	Item     *struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item"`
}

func (p *UpstreamStreamParser) handleData(payload string) []ir.StreamEvent {
	var f upStreamFrame
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return nil // malformed frame: skip, keep the stream alive
	}
	etype := f.Type
	if etype == "" {
		etype = p.eventName // data-only framing: identify by payload type
	}
	p.eventName = "" // a data frame consumes the pending event name

	switch etype {
	case "response.created":
		if f.Response == nil {
			return nil
		}
		p.started = true
		return []ir.StreamEvent{{Kind: ir.EvStart, Model: f.Response.Model}}
	case "response.reasoning_summary_text.delta":
		if f.Delta == "" {
			return nil
		}
		return p.ensureStarted(ir.StreamEvent{Kind: ir.EvThinkingDelta, Text: f.Delta})
	case "response.output_text.delta":
		if f.Delta == "" {
			return nil
		}
		return p.ensureStarted(ir.StreamEvent{Kind: ir.EvTextDelta, Text: f.Delta})
	case "response.output_item.added":
		if f.Item == nil || f.Item.Type != "function_call" {
			return nil // message / reasoning items open implicitly on first delta
		}
		callID := f.Item.CallID
		if callID == "" {
			callID = f.Item.ID
		}
		if f.Item.ID != "" && callID != "" {
			p.itemTool[f.Item.ID] = callID
		}
		p.knownTool[callID] = true
		p.sawTool = true
		p.open = append(p.open, callID)
		return p.ensureStarted(ir.StreamEvent{Kind: ir.EvToolStart, ToolID: callID, ToolName: f.Item.Name})
	case "response.function_call_arguments.delta":
		id := p.resolveTool(f.ItemID)
		if id == "" || f.Delta == "" {
			return nil
		}
		return []ir.StreamEvent{{Kind: ir.EvToolDelta, ToolID: id, Text: f.Delta}}
	case "response.function_call_arguments.done":
		return p.closeTool(p.resolveTool(f.ItemID))
	case "response.output_item.done":
		// function_call items are normally closed by arguments.done; this is
		// the safety net for streams that skip it.
		if f.Item == nil || f.Item.Type != "function_call" {
			return nil
		}
		id := f.Item.CallID
		if id == "" {
			id = p.itemTool[f.Item.ID]
		}
		if id == "" {
			id = f.Item.ID
		}
		return p.closeTool(id)
	case "response.completed":
		p.done = true
		return p.finishEvents(f.Response, ir.FinishStop)
	case "response.incomplete":
		p.done = true
		fin := ir.FinishLength
		if f.Response != nil && f.Response.IncompleteDetails != nil &&
			f.Response.IncompleteDetails.Reason == "content_filter" {
			fin = ir.FinishContentFilter
		}
		return p.finishEvents(f.Response, fin)
	case "response.failed":
		p.done = true
		return p.errorEvents(f, etype)
	case "response.error", "error":
		p.done = true
		return p.errorEvents(f, etype)
	default:
		return nil
	}
}

// finishEvents closes still-open tool calls, then emits usage (when carried)
// and the finish reason.
func (p *UpstreamStreamParser) finishEvents(resp *upStreamResponse, fin ir.Finish) []ir.StreamEvent {
	var evs []ir.StreamEvent
	evs = append(evs, p.closeOpenTools()...)
	if fin == ir.FinishStop && p.sawTool {
		fin = ir.FinishToolCalls
	}
	if resp != nil && resp.Usage != nil {
		evs = append(evs, ir.StreamEvent{Kind: ir.EvUsage, Usage: resp.Usage.toIR()})
	}
	evs = append(evs, ir.StreamEvent{Kind: ir.EvFinish, Finish: fin})
	return evs
}

// errorEvents converts a failed/error terminal event into EvError, classified
// from the carried error payload (response.error object, or the frame-level
// code/message of bare response.error events) when present.
func (p *UpstreamStreamParser) errorEvents(f upStreamFrame, etype string) []ir.StreamEvent {
	ue := &ir.UpstreamError{Status: 502, Kind: ir.ErrUpstream,
		Message: "upstream stream failed (" + etype + ")", Retryable: true}
	switch {
	case f.Response != nil && !isNull(f.Response.Error):
		var env struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(f.Response.Error, &env) == nil && env.Message != "" {
			ue = classifyUpstreamError(env.Message, "", env.Code)
		}
	case f.Message != "":
		ue = classifyUpstreamError(f.Message, "", f.Code)
	}
	return []ir.StreamEvent{{
		Kind:       ir.EvError,
		ErrCode:    ue.Kind,
		ErrMsg:     ue.Message,
		Retryable:  ue.Retryable,
		HTTPStatus: ue.Status,
	}}
}

// resolveTool maps an event's item_id to the IR ToolID (the call_id). Some
// implementations put the call_id there directly; both spellings are accepted.
func (p *UpstreamStreamParser) resolveTool(itemID string) string {
	if itemID == "" {
		return ""
	}
	if id, ok := p.itemTool[itemID]; ok {
		return id
	}
	if p.knownTool[itemID] {
		return itemID
	}
	return ""
}

func (p *UpstreamStreamParser) closeTool(id string) []ir.StreamEvent {
	if id == "" || p.closed[id] || !p.knownTool[id] {
		return nil
	}
	p.closed[id] = true
	for i, open := range p.open {
		if open == id {
			p.open = append(p.open[:i], p.open[i+1:]...)
			break
		}
	}
	return []ir.StreamEvent{{Kind: ir.EvToolEnd, ToolID: id}}
}

func (p *UpstreamStreamParser) closeOpenTools() []ir.StreamEvent {
	if len(p.open) == 0 {
		return nil
	}
	evs := make([]ir.StreamEvent, 0, len(p.open))
	for _, id := range p.open {
		evs = append(evs, ir.StreamEvent{Kind: ir.EvToolEnd, ToolID: id})
		p.closed[id] = true
	}
	p.open = nil
	return evs
}

// ensureStarted synthesizes an EvStart for streams whose first event is not
// response.created (defensive: IR expects the stream to open with EvStart).
func (p *UpstreamStreamParser) ensureStarted(ev ir.StreamEvent) []ir.StreamEvent {
	if p.started {
		return []ir.StreamEvent{ev}
	}
	p.started = true
	return []ir.StreamEvent{{Kind: ir.EvStart}, ev}
}
