package openaichat

import (
	"encoding/json"
	"strings"

	"kleinia.io/llmgw/internal/ir"
)

// UpstreamStreamParser converts OpenAI-style chat.completion.chunk SSE into IR
// stream events. Feed it one raw line at a time (as read from the socket,
// with or without trailing newline): "data: {...}" payloads, blank
// separators, ": keep-alive" comments and the "data: [DONE]" sentinel.
//
// new-api quirks handled here:
//   - Every chunk carries content AND reasoning_content, one of them "" —
//     empty deltas are dropped.
//   - Usage arrives on the final finish_reason chunk (no separate empty-
//     choices usage chunk); emitted once as EvUsage.
//   - Tool call fragments are addressed by wire index: the first fragment of
//     an index carries {id, function.name}; later ones only
//     {index, function.arguments}. The parser tracks index→ToolID and closes
//     all still-open calls (EvToolEnd) when finish_reason arrives, when a new
//     index appears, or at [DONE].
type UpstreamStreamParser struct {
	started   bool
	done      bool
	finished  bool
	usageSent bool
	model     string
	byIndex   map[int]string // wire tool index -> ToolID ("" = seen but unidentifiable)
	open      []string       // open ToolIDs in start order
}

// NewUpstreamStreamParser returns a parser in its initial state.
func NewUpstreamStreamParser() *UpstreamStreamParser {
	return &UpstreamStreamParser{byIndex: make(map[int]string)}
}

type streamChunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *wireUsage     `json:"usage"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"` // absent and null both mean "not yet"
}

type streamDelta struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []streamToolCall `json:"tool_calls"`
}

type streamToolCall struct {
	Index    *int     `json:"index"` // pointer: some impls omit it for single calls
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function streamFn `json:"function"`
}

type streamFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Line consumes one raw stream line and returns 0..n IR events. A malformed
// data payload yields a single EvError event and poisons the parser (later
// lines are ignored); lines after [DONE] are ignored as well.
func (p *UpstreamStreamParser) Line(line string) ([]ir.StreamEvent, error) {
	if p.done {
		return nil, nil
	}
	l := strings.TrimRight(line, "\r")
	l = strings.TrimSpace(l)
	if l == "" || strings.HasPrefix(l, ":") { // separator or ": keep-alive" comment
		return nil, nil
	}
	if !strings.HasPrefix(l, "data:") { // "event:" lines etc. — chat SSE has none, skip
		return nil, nil
	}
	payload := strings.TrimSpace(l[len("data:"):])
	if payload == "" {
		return nil, nil
	}
	if payload == "[DONE]" {
		p.done = true
		return p.closeOpenTools(), nil
	}
	var chunk streamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		p.done = true
		return []ir.StreamEvent{{
			Kind:    ir.EvError,
			ErrCode: ir.ErrUpstream,
			ErrMsg:  "openaichat: malformed stream chunk: " + err.Error(),
		}}, nil
	}

	var evs []ir.StreamEvent
	if !p.started {
		p.started = true
		if chunk.Model != "" {
			p.model = chunk.Model
		}
		evs = append(evs, ir.StreamEvent{Kind: ir.EvStart, Model: p.model})
	}
	for _, ch := range chunk.Choices {
		evs = append(evs, p.choiceEvents(ch)...)
	}
	// IR contract: EvUsage comes before or at EvFinish; tool calls still open
	// are closed first (their arguments are final once finish_reason arrives).
	finish := p.pendingFinish(chunk)
	if finish != nil {
		evs = append(evs, p.closeOpenTools()...)
	}
	if chunk.Usage != nil && !p.usageSent {
		p.usageSent = true
		evs = append(evs, ir.StreamEvent{Kind: ir.EvUsage, Usage: usageFromWire(chunk.Usage)})
	}
	if finish != nil {
		evs = append(evs, ir.StreamEvent{Kind: ir.EvFinish, Finish: *finish})
	}
	return evs, nil
}

func (p *UpstreamStreamParser) choiceEvents(ch streamChoice) []ir.StreamEvent {
	var evs []ir.StreamEvent
	if ch.Delta.Content != "" {
		evs = append(evs, ir.StreamEvent{Kind: ir.EvTextDelta, Text: ch.Delta.Content})
	}
	if ch.Delta.ReasoningContent != "" {
		evs = append(evs, ir.StreamEvent{Kind: ir.EvThinkingDelta, Text: ch.Delta.ReasoningContent})
	}
	for _, tc := range ch.Delta.ToolCalls {
		evs = append(evs, p.toolCallEvents(tc)...)
	}
	return evs
}

func (p *UpstreamStreamParser) toolCallEvents(tc streamToolCall) []ir.StreamEvent {
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	if id, known := p.byIndex[idx]; known {
		if id != "" && tc.Function.Arguments != "" {
			return []ir.StreamEvent{{Kind: ir.EvToolDelta, ToolID: id, Text: tc.Function.Arguments}}
		}
		return nil
	}
	// First fragment of a new index: close everything still open (wire tool
	// calls arrive sequentially), then open the new one.
	evs := p.closeOpenTools()
	if tc.ID == "" {
		// Fragment without id: nothing to anchor EvToolStart on; remember the
		// index so later fragments with arguments are still skipped cleanly.
		p.byIndex[idx] = ""
		return evs
	}
	p.byIndex[idx] = tc.ID
	p.open = append(p.open, tc.ID)
	evs = append(evs, ir.StreamEvent{Kind: ir.EvToolStart, ToolID: tc.ID, ToolName: tc.Function.Name})
	if tc.Function.Arguments != "" {
		evs = append(evs, ir.StreamEvent{Kind: ir.EvToolDelta, ToolID: tc.ID, Text: tc.Function.Arguments})
	}
	return evs
}

// pendingFinish reports the finish reason of this chunk when it is the first
// one carrying it.
func (p *UpstreamStreamParser) pendingFinish(chunk streamChunk) *ir.Finish {
	if p.finished {
		return nil
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			p.finished = true
			f := finishFromWire(*ch.FinishReason)
			return &f
		}
	}
	return nil
}

func (p *UpstreamStreamParser) closeOpenTools() []ir.StreamEvent {
	if len(p.open) == 0 {
		return nil
	}
	evs := make([]ir.StreamEvent, 0, len(p.open))
	for _, id := range p.open {
		evs = append(evs, ir.StreamEvent{Kind: ir.EvToolEnd, ToolID: id})
	}
	p.open = nil
	return evs
}
