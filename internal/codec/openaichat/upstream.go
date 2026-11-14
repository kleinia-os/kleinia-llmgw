package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"kleinia.io/llmgw/internal/ir"
)

// ---- outbound request building ----

type upBody struct {
	Model          string            `json:"model"`
	Messages       []upMessage       `json:"messages"`
	MaxTokens      int64             `json:"max_tokens,omitempty"` // 0 = unset, omitted
	Temperature    *float64          `json:"temperature,omitempty"`
	TopP           *float64          `json:"top_p,omitempty"`
	TopK           *int64            `json:"top_k,omitempty"`
	Stop           []string          `json:"stop,omitempty"`
	Seed           *int64            `json:"seed,omitempty"`
	ResponseFormat *upResponseFormat `json:"response_format,omitempty"`
	Stream         bool              `json:"stream,omitempty"`
	StreamOptions  *upStreamOptions  `json:"stream_options,omitempty"`
	Tools          []upTool          `json:"tools,omitempty"`
	ToolChoice     any               `json:"tool_choice,omitempty"` // string or object
}

type upStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type upResponseFormat struct {
	Type string `json:"type"`
}

type upMessage struct {
	Role             string       `json:"role"`
	Content          any          `json:"content"` // string or []upContentPart
	ReasoningContent string       `json:"reasoning_content,omitempty"`
	ToolCallID       string       `json:"tool_call_id,omitempty"`
	ToolCalls        []upToolCall `json:"tool_calls,omitempty"`
}

type upToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // "function"
	Function upToolFn `json:"function"`
}

type upToolFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type upContentPart struct {
	Type     string      `json:"type"` // "text" | "image_url"
	Text     string      `json:"text,omitempty"`
	ImageURL *upImageURL `json:"image_url,omitempty"`
}

type upImageURL struct {
	URL string `json:"url"`
}

type upTool struct {
	Type     string      `json:"type"` // "function"
	Function upToolDefFn `json:"function"`
}

type upToolDefFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema, verbatim
}

type upToolChoice struct {
	Type     string          `json:"type"` // "tool"
	Function *upToolChoiceFn `json:"function,omitempty"`
}

type upToolChoiceFn struct {
	Name string `json:"name"`
}

// BuildUpstreamRequest builds a POST {baseURL}/chat/completions request with
// Bearer auth. model is the concrete upstream model (alias already resolved).
// Unknown fields preserved in req.Raw (same-format pass-through) are appended
// to the body without ever overriding fields derived from the IR.
func BuildUpstreamRequest(ctx context.Context, baseURL, apiKey, model string, req ir.Request, stream bool) (*http.Request, error) {
	body := upBody{
		Model:       model,
		Messages:    upstreamMessages(req.Messages),
		MaxTokens:   req.Sampling.MaxTokens,
		Temperature: req.Sampling.Temperature,
		TopP:        req.Sampling.TopP,
		TopK:        req.Sampling.TopK,
		Stop:        req.Sampling.Stop,
		Seed:        req.Sampling.Seed,
		Stream:      stream,
	}
	if req.ResponseFormat != nil {
		body.ResponseFormat = &upResponseFormat{Type: req.ResponseFormat.Type}
	}
	if stream {
		body.StreamOptions = &upStreamOptions{IncludeUsage: true}
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, upTool{
			Type:     "function",
			Function: upToolDefFn{Name: t.Name, Description: t.Description, Parameters: t.ParametersJSON},
		})
	}
	switch {
	case req.ToolChoice == nil:
	case req.ToolChoice.Type == ir.ToolChoiceNamed:
		// OpenAI chat-completions wire format is {"type":"function",
		// "function":{"name":...}}; "tool" is the Anthropic spelling and the
		// upstream rejects it with "only function tool_choice is supported".
		body.ToolChoice = upToolChoice{Type: "function", Function: &upToolChoiceFn{Name: req.ToolChoice.Name}}
	default:
		body.ToolChoice = string(req.ToolChoice.Type) // auto / none / required
	}

	b, err := marshalWithRaw(body, req.Raw)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+apiKey)
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	} else {
		hreq.Header.Set("Accept", "application/json")
	}
	return hreq, nil
}

// marshalWithRaw serializes body, then adds req.Raw fields that body itself
// did not produce (known fields always win).
func marshalWithRaw(body upBody, raw map[string]json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return b, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	changed := false
	for k, v := range raw {
		if _, dup := m[k]; dup {
			continue
		}
		m[k] = v
		changed = true
	}
	if !changed {
		return b, nil
	}
	return json.Marshal(m)
}

// upstreamMessages maps IR messages to wire messages. A RoleTool message with
// several PartToolResult parts becomes several role:"tool" wire messages (the
// wire format allows exactly one tool_call_id per message).
func upstreamMessages(msgs []ir.Message) []upMessage {
	out := make([]upMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case ir.RoleTool:
			for _, p := range m.Content {
				if p.Type != ir.PartToolResult {
					continue
				}
				out = append(out, upMessage{Role: "tool", Content: p.ToolOutput, ToolCallID: p.ToolCallID})
			}
		case ir.RoleAssistant:
			um := upMessage{Role: "assistant"}
			var think, text []string
			var images []upContentPart
			for _, p := range m.Content {
				switch p.Type {
				case ir.PartThinking:
					think = append(think, p.Text)
				case ir.PartText:
					text = append(text, p.Text)
				case ir.PartImage:
					images = append(images, upContentPart{Type: "image_url", ImageURL: &upImageURL{URL: p.ImageURL}})
				case ir.PartToolUse:
					um.ToolCalls = append(um.ToolCalls, upToolCall{
						ID: p.ToolCallID, Type: "function",
						Function: upToolFn{Name: p.ToolName, Arguments: p.ToolArgsJSON},
					})
				}
			}
			um.ReasoningContent = strings.Join(think, "")
			if len(images) > 0 {
				parts := make([]upContentPart, 0, len(text)+len(images))
				for _, t := range text {
					parts = append(parts, upContentPart{Type: "text", Text: t})
				}
				parts = append(parts, images...)
				um.Content = parts
			} else {
				um.Content = strings.Join(text, "")
			}
			out = append(out, um)
		default: // system, user, pass-through roles
			um := upMessage{Role: string(m.Role)}
			var text []string
			var images []upContentPart
			for _, p := range m.Content {
				switch p.Type {
				case ir.PartText:
					text = append(text, p.Text)
				case ir.PartImage:
					images = append(images, upContentPart{Type: "image_url", ImageURL: &upImageURL{URL: p.ImageURL}})
				}
			}
			if len(images) > 0 {
				parts := make([]upContentPart, 0, len(text)+len(images))
				for _, t := range text {
					parts = append(parts, upContentPart{Type: "text", Text: t})
				}
				parts = append(parts, images...)
				um.Content = parts
			} else {
				um.Content = strings.Join(text, "")
			}
			out = append(out, um)
		}
	}
	return out
}

// ---- inbound response body parsing ----

type wireUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type upResponse struct {
	ID          string     `json:"id"`
	Model       string     `json:"model"`
	Choices     []upChoice `json:"choices"`
	Usage       *wireUsage `json:"usage"`
	Error       *wireErr   `json:"error"`
	NewAPIError *wireErr   `json:"new_api_error"`
}

type upChoice struct {
	Index        int          `json:"index"`
	Message      upMsgContent `json:"message"`
	FinishReason string       `json:"finish_reason"`
}

type upMsgContent struct {
	Role             string       `json:"role"`
	Content          string       `json:"content"`
	ReasoningContent string       `json:"reasoning_content"`
	ToolCalls        []upToolCall `json:"tool_calls"`
}

// usageFromWire normalizes wire usage into IR:
// prompt_tokens (cache-inclusive) → InputTokens, cached_tokens →
// CacheReadTokens, completion_tokens (reasoning-inclusive) → OutputTokens,
// reasoning_tokens → ReasoningTokens.
func usageFromWire(u *wireUsage) ir.Usage {
	if u == nil {
		return ir.Usage{}
	}
	out := ir.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

// ParseUpstreamBody parses a complete non-streaming upstream body. Bodies that
// carry an error envelope ({"error":{...}} or {"new_api_error":{...}}) become
// *ir.UpstreamError with the best Kind guess from the envelope text; the HTTP
// status itself is patched in by the server, which alone sees it.
func ParseUpstreamBody(body []byte) (ir.Response, error) {
	var r upResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return ir.Response{}, unparseableUpstreamError(body, err)
	}
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		res := ir.Response{Model: r.Model, FinishReason: finishFromWire(c.FinishReason), Usage: usageFromWire(r.Usage)}
		if c.Message.ReasoningContent != "" {
			res.Content = append(res.Content, ir.Part{Type: ir.PartThinking, Text: c.Message.ReasoningContent})
		}
		if c.Message.Content != "" {
			res.Content = append(res.Content, ir.Part{Type: ir.PartText, Text: c.Message.Content})
		}
		for _, tc := range c.Message.ToolCalls {
			res.Content = append(res.Content, ir.Part{
				Type: ir.PartToolUse, ToolCallID: tc.ID,
				ToolName: tc.Function.Name, ToolArgsJSON: tc.Function.Arguments,
			})
		}
		return res, nil
	}
	if r.Error != nil {
		return ir.Response{}, errorFromEnvelope(r.Error, body)
	}
	if r.NewAPIError != nil {
		return ir.Response{}, errorFromEnvelope(r.NewAPIError, body)
	}
	// Valid chat.completion-ish JSON without choices: keep whatever usage it
	// carried rather than failing the call.
	if r.Model != "" || r.ID != "" || r.Usage != nil {
		return ir.Response{Model: r.Model, FinishReason: ir.FinishStop, Usage: usageFromWire(r.Usage)}, nil
	}
	return ir.Response{}, unparseableUpstreamError(body, nil)
}
