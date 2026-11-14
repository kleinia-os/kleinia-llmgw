package openaichat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"kleinia.io/llmgw/internal/ir"
)

// ---- non-streaming response ----

type clientResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []clientRespChoice `json:"choices"`
	Usage   clientUsage        `json:"usage"`
}

type clientRespChoice struct {
	Index        int              `json:"index"`
	Message      clientMessageOut `json:"message"`
	Logprobs     *struct{}        `json:"logprobs"` // always null, matches wire samples
	FinishReason string           `json:"finish_reason"`
}

type clientMessageOut struct {
	Role             string          `json:"role"` // "assistant"
	Content          *string         `json:"content"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []clientToolOut `json:"tool_calls,omitempty"`
}

type clientToolOut struct {
	ID       string                `json:"id"`
	Type     string                `json:"type"` // "function"
	Function clientToolOutFunction `json:"function"`
}

type clientToolOutFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string, verbatim
}

type clientUsage struct {
	PromptTokens        int64                `json:"prompt_tokens"`
	CompletionTokens    int64                `json:"completion_tokens"`
	TotalTokens         int64                `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
	CompletionDetails   *completionDetails   `json:"completion_tokens_details,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type completionDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

func usageToClient(u ir.Usage) clientUsage {
	c := clientUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.Total(),
	}
	if u.CacheReadTokens > 0 {
		c.PromptTokensDetails = &promptTokensDetails{CachedTokens: u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		c.CompletionDetails = &completionDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return c
}

// WriteClientResponse writes a complete non-streaming chat.completion.
func WriteClientResponse(w http.ResponseWriter, res ir.Response) error {
	msg := clientMessageOut{Role: "assistant"}
	var text string
	hasText := false
	for _, p := range res.Content {
		switch p.Type {
		case ir.PartText:
			text += p.Text
			hasText = true
		case ir.PartThinking:
			msg.ReasoningContent += p.Text
		case ir.PartToolUse:
			msg.ToolCalls = append(msg.ToolCalls, clientToolOut{
				ID:   p.ToolCallID,
				Type: "function",
				Function: clientToolOutFunction{
					Name:      p.ToolName,
					Arguments: p.ToolArgsJSON,
				},
			})
		}
	}
	if hasText {
		msg.Content = &text
	} // else content stays null (no text parts)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(clientResponse{
		ID:      randID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   res.Model,
		Choices: []clientRespChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishToWire(res.FinishReason),
		}},
		Usage: usageToClient(res.Usage),
	})
}

// WriteClientError writes an OpenAI-style error envelope:
// {"error":{"message":...,"type":...,"param":"","code":...}}.
func WriteClientError(w http.ResponseWriter, status int, kind, msg string) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(struct {
		Error clientErrBody `json:"error"`
	}{
		Error: clientErrBody{Message: msg, Type: errorTypeForKind(kind), Code: kind},
	})
}

type clientErrBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    string `json:"code"`
}

func errorTypeForKind(kind string) string {
	switch kind {
	case ir.ErrInvalidKey:
		return "authentication_error"
	case ir.ErrRateLimited, ir.ErrQuota:
		return "rate_limit_error"
	case ir.ErrContextLength, ir.ErrNotFound, "model_not_found", "invalid_request":
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// ---- streaming response (SSE encoder) ----

type clientChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"` // "chat.completion.chunk"
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []clientChoice `json:"choices"`
	Usage   *clientUsage   `json:"usage,omitempty"`
}

type clientChoice struct {
	Index        int         `json:"index"`
	Delta        clientDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"` // null until the final chunk
}

type clientDelta struct {
	Role             string               `json:"role,omitempty"`
	Content          string               `json:"content,omitempty"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	ToolCalls        []clientToolDeltaOut `json:"tool_calls,omitempty"`
}

type clientToolDeltaOut struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function *clientToolFnDelta `json:"function,omitempty"`
}

type clientToolFnDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"` // "" on tool start, fragment on deltas
}

// ClientStreamEncoder writes OpenAI-style SSE: plain "data: {...}\n\n" lines
// terminated by "data: [DONE]". The opening chunk (delta:{role:"assistant"})
// is written lazily on the first Emit.
type ClientStreamEncoder struct {
	w        http.ResponseWriter
	id       string
	created  int64
	model    string
	opened   bool
	finished bool
	toolIdx  map[string]int // IR ToolID -> wire index 0..n
	nTools   int
}

// NewClientStreamEncoder sets the SSE response headers and returns a lazy
// encoder. Flushing is the server's job.
func NewClientStreamEncoder(w http.ResponseWriter) *ClientStreamEncoder {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return &ClientStreamEncoder{
		w:       w,
		id:      randID(),
		created: time.Now().Unix(),
		toolIdx: make(map[string]int),
	}
}

// Emit converts one IR stream event into SSE chunks.
func (e *ClientStreamEncoder) Emit(ev ir.StreamEvent) error {
	if ev.Model != "" {
		e.model = ev.Model
	}
	if err := e.openIfNeeded(); err != nil {
		return err
	}
	switch ev.Kind {
	case ir.EvStart, ir.EvToolEnd:
		return nil // nothing extra on the wire
	case ir.EvTextDelta:
		if ev.Text == "" {
			return nil
		}
		return e.sendChoices(clientChoice{Index: 0, Delta: clientDelta{Content: ev.Text}})
	case ir.EvThinkingDelta:
		if ev.Text == "" {
			return nil
		}
		return e.sendChoices(clientChoice{Index: 0, Delta: clientDelta{ReasoningContent: ev.Text}})
	case ir.EvToolStart:
		idx := e.nTools
		e.nTools++
		e.toolIdx[ev.ToolID] = idx
		return e.sendChoices(clientChoice{Index: 0, Delta: clientDelta{ToolCalls: []clientToolDeltaOut{{
			Index:    idx,
			ID:       ev.ToolID,
			Type:     "function",
			Function: &clientToolFnDelta{Name: ev.ToolName},
		}}}})
	case ir.EvToolDelta:
		idx, ok := e.toolIdx[ev.ToolID]
		if !ok {
			return nil // delta for an unknown call: cannot address it on the wire
		}
		return e.sendChoices(clientChoice{Index: 0, Delta: clientDelta{ToolCalls: []clientToolDeltaOut{{
			Index:    idx,
			Function: &clientToolFnDelta{Arguments: ev.Text},
		}}}})
	case ir.EvUsage:
		u := usageToClient(ev.Usage)
		return e.writeChunk(clientChunk{
			ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
			Choices: []clientChoice{}, Usage: &u,
		})
	case ir.EvFinish:
		e.finished = true
		fr := finishToWire(ev.Finish)
		return e.sendChoices(clientChoice{Index: 0, Delta: clientDelta{}, FinishReason: &fr})
	case ir.EvError:
		return e.writeRaw(struct {
			Error clientErrBody `json:"error"`
		}{Error: clientErrBody{Message: ev.ErrMsg, Type: errorTypeForKind(ev.ErrCode), Code: ev.ErrCode}})
	}
	return nil
}

// Close terminates the stream: a finish chunk (if none was emitted yet) and
// then the [DONE] sentinel.
func (e *ClientStreamEncoder) Close(finish ir.Finish) error {
	if e.opened && !e.finished {
		fr := finishToWire(finish)
		if err := e.sendChoices(clientChoice{Index: 0, Delta: clientDelta{}, FinishReason: &fr}); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(e.w, "data: [DONE]\n\n")
	return err
}

func (e *ClientStreamEncoder) openIfNeeded() error {
	if e.opened {
		return nil
	}
	e.opened = true
	return e.sendChoices(clientChoice{Index: 0, Delta: clientDelta{Role: "assistant"}})
}

func (e *ClientStreamEncoder) sendChoices(choices ...clientChoice) error {
	return e.writeChunk(clientChunk{
		ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
		Choices: choices,
	})
}

func (e *ClientStreamEncoder) writeChunk(c clientChunk) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return e.writeRaw(json.RawMessage(b))
}

func (e *ClientStreamEncoder) writeRaw(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.w, "data: %s\n\n", b)
	return err
}
