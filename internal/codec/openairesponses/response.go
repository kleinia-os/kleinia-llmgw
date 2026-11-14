package openairesponses

import (
	"encoding/json"
	"net/http"
	"time"

	"kleinia.io/llmgw/internal/ir"
)

// ---- Responses wire shapes (field order mirrors the golden samples) ----

type summaryText struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

type outputTextPart struct {
	Type        string `json:"type"` // "output_text"
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
	Logprobs    []any  `json:"logprobs"`
}

func textPart(text string) outputTextPart {
	return outputTextPart{Type: "output_text", Text: text, Annotations: []any{}, Logprobs: []any{}}
}

type reasoningItem struct {
	ID      string        `json:"id"`
	Type    string        `json:"type"` // "reasoning"
	Summary []summaryText `json:"summary"`
}

type messageItem struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"` // "message"
	Role    string           `json:"role"` // "assistant"
	Status  string           `json:"status"`
	Content []outputTextPart `json:"content"`
}

type functionCallItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

type usageObj struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

func usageOf(u ir.Usage) *usageObj {
	return &usageObj{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.Total()}
}

// responseEnvelope is the response object embedded in every non-streaming
// body and in the created / completed / incomplete / failed stream events.
// Echo fields the codec has no request context for (instructions, tools,
// temperature, ...) carry neutral defaults; clients key off id / status /
// output / usage.
type responseEnvelope struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Error             any                `json:"error"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
	Instructions      any                `json:"instructions"`
	Metadata          map[string]any     `json:"metadata"`
	Model             string             `json:"model"`
	Output            []any              `json:"output"`
	ParallelToolCalls bool               `json:"parallel_tool_calls"`
	Temperature       any                `json:"temperature"`
	ToolChoice        string             `json:"tool_choice"`
	Tools             []any              `json:"tools"`
	TopP              any                `json:"top_p"`
	MaxOutputTokens   any                `json:"max_output_tokens"`
	Usage             *usageObj          `json:"usage"`
}

func newResponseEnvelope(id, model, status string) *responseEnvelope {
	return &responseEnvelope{
		ID: id, Object: "response", CreatedAt: time.Now().Unix(), Status: status,
		Metadata: map[string]any{}, Model: model,
		Output: []any{}, ParallelToolCalls: true, ToolChoice: "auto", Tools: []any{},
	}
}

// buildOutputItems converts assistant-side IR parts into output items in the
// canonical order: reasoning first, then function_calls, then messages
// (tool calls must precede the text message; Codex relies on this).
func buildOutputItems(parts []ir.Part) []any {
	var think, calls, texts []any
	for _, p := range parts {
		switch p.Type {
		case ir.PartThinking:
			think = append(think, &reasoningItem{
				ID: randID("rs"), Type: "reasoning",
				Summary: []summaryText{{Type: "summary_text", Text: p.Text}},
			})
		case ir.PartToolUse:
			calls = append(calls, &functionCallItem{
				ID: randID("fc"), Type: "function_call", CallID: p.ToolCallID,
				Name: p.ToolName, Arguments: p.ToolArgsJSON, Status: "completed",
			})
		case ir.PartText:
			texts = append(texts, &messageItem{
				ID: randID("msg"), Type: "message", Role: "assistant", Status: "completed",
				Content: []outputTextPart{textPart(p.Text)},
			})
		default:
			// PartImage / PartToolResult never appear on the assistant side.
		}
	}
	out := make([]any, 0, len(think)+len(calls)+len(texts))
	out = append(out, think...)
	out = append(out, calls...)
	out = append(out, texts...)
	return out
}

// WriteClientResponse writes a complete non-streaming Responses body.
func WriteClientResponse(w http.ResponseWriter, res ir.Response) error {
	env := newResponseEnvelope(randID("resp"), res.Model, "completed")
	switch res.FinishReason {
	case ir.FinishLength:
		env.Status = "incomplete"
		env.IncompleteDetails = &incompleteDetails{Reason: "max_output_tokens"}
	case ir.FinishContentFilter:
		env.Status = "incomplete"
		env.IncompleteDetails = &incompleteDetails{Reason: "content_filter"}
	}
	env.Output = buildOutputItems(res.Content)
	env.Usage = usageOf(res.Usage)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(env)
}
