package openairesponses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"kleinia.io/llmgw/internal/ir"
)

// ---- SSE event payload shapes ----

type evResponse struct {
	Response       *responseEnvelope `json:"response"`
	Type           string            `json:"type"`
	SequenceNumber int               `json:"sequence_number"`
}

type evOutputItem struct {
	OutputIndex    int    `json:"output_index"`
	Item           any    `json:"item"`
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
}

type evSummaryPart struct {
	ItemID         string      `json:"item_id"`
	OutputIndex    int         `json:"output_index"`
	SummaryIndex   int         `json:"summary_index"`
	Part           summaryText `json:"part"`
	Type           string      `json:"type"`
	SequenceNumber int         `json:"sequence_number"`
}

type evSummaryDelta struct {
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	Delta          string `json:"delta"`
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
}

type evSummaryDone struct {
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	Text           string `json:"text"`
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
}

type evContentPart struct {
	ItemID         string         `json:"item_id"`
	OutputIndex    int            `json:"output_index"`
	ContentIndex   int            `json:"content_index"`
	Part           outputTextPart `json:"part"`
	Type           string         `json:"type"`
	SequenceNumber int            `json:"sequence_number"`
}

type evTextDelta struct {
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Delta          string `json:"delta"`
	Logprobs       []any  `json:"logprobs"`
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
}

type evTextDone struct {
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Text           string `json:"text"`
	Logprobs       []any  `json:"logprobs"`
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
}

type evArgsDelta struct {
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Delta          string `json:"delta"`
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
}

type evArgsDone struct {
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Arguments      string `json:"arguments"`
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
}

// encItem is one output item accumulated across the stream.
type encItem struct {
	kind   string // "reasoning" | "message" | "function_call"
	idx    int    // output_index within the stream
	id     string // rs_ / msg_ / fc_ id
	callID string // function_call call_id
	name   string
	text   strings.Builder
	args   strings.Builder
	closed bool
}

// ClientStreamEncoder writes Responses SSE. The stream opens lazily: the
// response.created / response.in_progress pair is written on the first Emit
// (or Close), so a failed-before-output upstream costs nothing.
//
// The encoder normalizes IR events into the canonical event order — in
// particular a reasoning block is fully closed (text.done + part.done +
// output_item.done) before the next item opens. The sampled upstream interleaves
// these (see testdata/resp_stream.txt); we never forward that interleave.
type ClientStreamEncoder struct {
	w     http.ResponseWriter
	flush http.Flusher

	opened   bool
	failed   bool
	closed   bool
	seq      int
	model    string
	finish   ir.Finish
	usage    ir.Usage
	env      *responseEnvelope
	items    []*encItem
	curThink *encItem
	curMsg   *encItem
}

// NewClientStreamEncoder sets the SSE headers and returns a lazy encoder.
func NewClientStreamEncoder(w http.ResponseWriter) *ClientStreamEncoder {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	e := &ClientStreamEncoder{w: w}
	if f, ok := w.(http.Flusher); ok {
		e.flush = f
	}
	return e
}

// Emit forwards one IR event. EvStart records the serving model; EvUsage /
// EvFinish are recorded and surface in the terminal event written by Close;
// EvError terminates the stream with response.failed.
func (e *ClientStreamEncoder) Emit(ev ir.StreamEvent) error {
	if e.failed || e.closed {
		return nil
	}
	if ev.Kind == ir.EvStart && ev.Model != "" {
		e.model = ev.Model
	}
	if err := e.ensureOpen(); err != nil {
		return err
	}
	switch ev.Kind {
	case ir.EvStart:
		// model captured above; nothing else to write.
	case ir.EvThinkingDelta:
		return e.thinkingDelta(ev.Text)
	case ir.EvTextDelta:
		return e.textDelta(ev.Text)
	case ir.EvToolStart:
		return e.toolStart(ev.ToolID, ev.ToolName)
	case ir.EvToolDelta:
		return e.toolDelta(ev.ToolID, ev.Text)
	case ir.EvToolEnd:
		return e.toolEnd(ev.ToolID)
	case ir.EvUsage:
		e.usage = ev.Usage // last one wins, matching the server accountant
	case ir.EvFinish:
		e.finish = ev.Finish
	case ir.EvError:
		return e.streamFailed(ev)
	}
	return nil
}

// Close closes any open items, then writes response.completed (or
// response.incomplete for length / content_filter). A stream terminated by
// EvError is already finished; Close is a no-op then.
func (e *ClientStreamEncoder) Close(finish ir.Finish) error {
	if e.failed || e.closed {
		return nil
	}
	e.closed = true
	if err := e.ensureOpen(); err != nil {
		return err
	}
	for _, it := range e.items {
		if !it.closed {
			if err := e.closeItem(it); err != nil {
				return err
			}
		}
	}

	f := e.finish // EvFinish is the wire truth; Close's arg is the fallback.
	if f == "" {
		f = finish
	}
	switch f {
	case ir.FinishLength:
		env := e.finalEnv("incomplete")
		env.IncompleteDetails = &incompleteDetails{Reason: "max_output_tokens"}
		env.Usage = usageOf(e.usage)
		return e.writeEvent("response.incomplete", evResponse{Response: env, Type: "response.incomplete", SequenceNumber: e.seq})
	case ir.FinishContentFilter:
		env := e.finalEnv("incomplete")
		env.IncompleteDetails = &incompleteDetails{Reason: "content_filter"}
		env.Usage = usageOf(e.usage)
		return e.writeEvent("response.incomplete", evResponse{Response: env, Type: "response.incomplete", SequenceNumber: e.seq})
	default:
		env := e.finalEnv("completed")
		env.Usage = usageOf(e.usage)
		return e.writeEvent("response.completed", evResponse{Response: env, Type: "response.completed", SequenceNumber: e.seq})
	}
}

func (e *ClientStreamEncoder) ensureOpen() error {
	if e.opened {
		return nil
	}
	e.opened = true
	e.env = newResponseEnvelope(randID("resp"), e.model, "in_progress")
	if err := e.writeEvent("response.created", evResponse{Response: e.env, Type: "response.created", SequenceNumber: e.seq}); err != nil {
		return err
	}
	return e.writeEvent("response.in_progress", evResponse{Response: e.env, Type: "response.in_progress", SequenceNumber: e.seq})
}

func (e *ClientStreamEncoder) writeEvent(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("responses: marshal %s: %w", name, err)
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return fmt.Errorf("responses: write %s: %w", name, err)
	}
	e.seq++
	if e.flush != nil {
		e.flush.Flush()
	}
	return nil
}

// ---- block transitions ----

func (e *ClientStreamEncoder) newItem(kind string) *encItem {
	it := &encItem{kind: kind, idx: len(e.items), id: itemID(kind)}
	e.items = append(e.items, it)
	return it
}

func itemID(kind string) string {
	switch kind {
	case "reasoning":
		return randID("rs")
	case "message":
		return randID("msg")
	default:
		return randID("fc")
	}
}

func (e *ClientStreamEncoder) thinkingDelta(text string) error {
	if e.curThink == nil {
		if err := e.closeMessage(); err != nil {
			return err
		}
		it := e.newItem("reasoning")
		e.curThink = it
		if err := e.writeEvent("response.output_item.added", evOutputItem{
			OutputIndex: it.idx, Type: "response.output_item.added", SequenceNumber: e.seq,
			Item: &reasoningItem{ID: it.id, Type: "reasoning", Summary: []summaryText{}},
		}); err != nil {
			return err
		}
		if err := e.writeEvent("response.reasoning_summary_part.added", evSummaryPart{
			ItemID: it.id, OutputIndex: it.idx, SummaryIndex: 0, Type: "response.reasoning_summary_part.added",
			SequenceNumber: e.seq, Part: summaryText{Type: "summary_text"},
		}); err != nil {
			return err
		}
	}
	e.curThink.text.WriteString(text)
	return e.writeEvent("response.reasoning_summary_text.delta", evSummaryDelta{
		ItemID: e.curThink.id, OutputIndex: e.curThink.idx, SummaryIndex: 0,
		Delta: text, Type: "response.reasoning_summary_text.delta", SequenceNumber: e.seq,
	})
}

func (e *ClientStreamEncoder) textDelta(text string) error {
	if e.curMsg == nil {
		if err := e.closeThinking(); err != nil {
			return err
		}
		it := e.newItem("message")
		e.curMsg = it
		if err := e.writeEvent("response.output_item.added", evOutputItem{
			OutputIndex: it.idx, Type: "response.output_item.added", SequenceNumber: e.seq,
			Item: &messageItem{ID: it.id, Type: "message", Role: "assistant", Status: "in_progress", Content: []outputTextPart{}},
		}); err != nil {
			return err
		}
		if err := e.writeEvent("response.content_part.added", evContentPart{
			ItemID: it.id, OutputIndex: it.idx, ContentIndex: 0, Type: "response.content_part.added",
			SequenceNumber: e.seq, Part: textPart(""),
		}); err != nil {
			return err
		}
	}
	e.curMsg.text.WriteString(text)
	return e.writeEvent("response.output_text.delta", evTextDelta{
		ItemID: e.curMsg.id, OutputIndex: e.curMsg.idx, ContentIndex: 0,
		Delta: text, Logprobs: []any{}, Type: "response.output_text.delta", SequenceNumber: e.seq,
	})
}

func (e *ClientStreamEncoder) toolStart(id, name string) error {
	if err := e.closeThinking(); err != nil {
		return err
	}
	if err := e.closeMessage(); err != nil {
		return err
	}
	it := e.newItem("function_call")
	it.callID = id
	it.name = name
	return e.writeEvent("response.output_item.added", evOutputItem{
		OutputIndex: it.idx, Type: "response.output_item.added", SequenceNumber: e.seq,
		Item: &functionCallItem{
			ID: it.id, Type: "function_call", CallID: id, Name: name,
			Arguments: "", Status: "in_progress",
		},
	})
}

func (e *ClientStreamEncoder) toolDelta(id, fragment string) error {
	it := e.findOpenTool(id)
	if it == nil {
		return nil // orphan fragment (no EvToolStart): tolerate silently
	}
	it.args.WriteString(fragment)
	return e.writeEvent("response.function_call_arguments.delta", evArgsDelta{
		ItemID: it.id, OutputIndex: it.idx, Delta: fragment,
		Type: "response.function_call_arguments.delta", SequenceNumber: e.seq,
	})
}

func (e *ClientStreamEncoder) toolEnd(id string) error {
	it := e.findOpenTool(id)
	if it == nil {
		return nil
	}
	return e.closeItem(it)
}

// findOpenTool matches by call id; an empty id matches when exactly one tool
// call is open (single-tool upstreams that omit the id in delta events).
func (e *ClientStreamEncoder) findOpenTool(id string) *encItem {
	var sole *encItem
	n := 0
	for _, it := range e.items {
		if it.kind != "function_call" || it.closed {
			continue
		}
		if it.callID == id {
			return it
		}
		n++
		sole = it
	}
	if id == "" && n == 1 {
		return sole
	}
	return nil
}

// closeItem closes one item, writing its done events in canonical order.
func (e *ClientStreamEncoder) closeItem(it *encItem) error {
	it.closed = true
	switch it.kind {
	case "reasoning":
		e.curThink = nil
		full := it.text.String()
		if err := e.writeEvent("response.reasoning_summary_text.done", evSummaryDone{
			ItemID: it.id, OutputIndex: it.idx, SummaryIndex: 0, Text: full,
			Type: "response.reasoning_summary_text.done", SequenceNumber: e.seq,
		}); err != nil {
			return err
		}
		if err := e.writeEvent("response.reasoning_summary_part.done", evSummaryPart{
			ItemID: it.id, OutputIndex: it.idx, SummaryIndex: 0, Type: "response.reasoning_summary_part.done",
			SequenceNumber: e.seq, Part: summaryText{Type: "summary_text", Text: full},
		}); err != nil {
			return err
		}
		return e.writeEvent("response.output_item.done", evOutputItem{
			OutputIndex: it.idx, Type: "response.output_item.done", SequenceNumber: e.seq,
			Item: &reasoningItem{ID: it.id, Type: "reasoning",
				Summary: []summaryText{{Type: "summary_text", Text: full}}},
		})
	case "message":
		e.curMsg = nil
		full := it.text.String()
		if err := e.writeEvent("response.output_text.done", evTextDone{
			ItemID: it.id, OutputIndex: it.idx, ContentIndex: 0, Text: full,
			Logprobs: []any{}, Type: "response.output_text.done", SequenceNumber: e.seq,
		}); err != nil {
			return err
		}
		if err := e.writeEvent("response.content_part.done", evContentPart{
			ItemID: it.id, OutputIndex: it.idx, ContentIndex: 0, Type: "response.content_part.done",
			SequenceNumber: e.seq, Part: textPart(full),
		}); err != nil {
			return err
		}
		return e.writeEvent("response.output_item.done", evOutputItem{
			OutputIndex: it.idx, Type: "response.output_item.done", SequenceNumber: e.seq,
			Item: &messageItem{ID: it.id, Type: "message", Role: "assistant", Status: "completed",
				Content: []outputTextPart{textPart(full)}},
		})
	default: // "function_call"
		full := it.args.String()
		if err := e.writeEvent("response.function_call_arguments.done", evArgsDone{
			ItemID: it.id, OutputIndex: it.idx, Arguments: full,
			Type: "response.function_call_arguments.done", SequenceNumber: e.seq,
		}); err != nil {
			return err
		}
		return e.writeEvent("response.output_item.done", evOutputItem{
			OutputIndex: it.idx, Type: "response.output_item.done", SequenceNumber: e.seq,
			Item: &functionCallItem{ID: it.id, Type: "function_call", CallID: it.callID,
				Name: it.name, Arguments: full, Status: "completed"},
		})
	}
}

func (e *ClientStreamEncoder) closeThinking() error {
	if e.curThink == nil || e.curThink.closed {
		e.curThink = nil
		return nil
	}
	return e.closeItem(e.curThink)
}

func (e *ClientStreamEncoder) closeMessage() error {
	if e.curMsg == nil || e.curMsg.closed {
		e.curMsg = nil
		return nil
	}
	return e.closeItem(e.curMsg)
}

// finalEnv clones the opening skeleton with the accumulated output items.
func (e *ClientStreamEncoder) finalEnv(status string) *responseEnvelope {
	env := *e.env
	env.Status = status
	env.Output = make([]any, 0, len(e.items))
	for _, it := range e.items {
		switch it.kind {
		case "reasoning":
			env.Output = append(env.Output, &reasoningItem{ID: it.id, Type: "reasoning",
				Summary: []summaryText{{Type: "summary_text", Text: it.text.String()}}})
		case "message":
			env.Output = append(env.Output, &messageItem{ID: it.id, Type: "message",
				Role: "assistant", Status: "completed", Content: []outputTextPart{textPart(it.text.String())}})
		default:
			env.Output = append(env.Output, &functionCallItem{ID: it.id, Type: "function_call",
				CallID: it.callID, Name: it.name, Arguments: it.args.String(), Status: "completed"})
		}
	}
	return &env
}

// streamFailed terminates the stream with response.failed.
func (e *ClientStreamEncoder) streamFailed(ev ir.StreamEvent) error {
	e.failed = true
	env := e.finalEnv("failed")
	env.Error = struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: ev.ErrCode, Message: ev.ErrMsg}
	return e.writeEvent("response.failed", evResponse{Response: env, Type: "response.failed", SequenceNumber: e.seq})
}
