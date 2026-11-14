// Package ir defines llmgw's canonical intermediate representation (IR) for
// chat-family LLM requests and responses.
//
// Every wire format (OpenAI Chat Completions, OpenAI Responses, Anthropic
// Messages, Gemini) converts to/from this IR, so N formats need 2N codecs
// instead of N² pairwise converters.
//
// Design rules:
//   - The IR is lossy-minimal: it must express everything the three G0 client
//     formats and two G0 upstream formats can express, nothing more.
//   - Tool-call arguments stay as raw JSON strings (ToolArgsJSON) so we never
//     re-serialize user JSON and break byte-for-byte fidelity.
//   - Unknown client fields are preserved in Request.Raw for upstream
//     pass-through when the target format matches the source format.
package ir

import (
	"encoding/json"
	"fmt"
)

// Role is a normalized message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool carries tool results. OpenAI renders this as role "tool";
	// Anthropic renders it as a user message whose content blocks are
	// tool_result parts; Responses renders it as function_call_output items.
	RoleTool Role = "tool"
)

// PartType discriminates content parts.
type PartType string

const (
	PartText       PartType = "text"
	PartThinking   PartType = "thinking" // reasoning content (reasoning_content on openai-compatible upstreams, anthropic thinking, responses reasoning summary)
	PartImage      PartType = "image"
	PartToolUse    PartType = "tool_use"    // assistant-side tool call
	PartToolResult PartType = "tool_result" // tool-side result, carried by RoleTool messages
)

// Part is one content block of a message.
type Part struct {
	Type PartType `json:"type"`

	// Text carries PartText, PartThinking content, the partial JSON argument
	// fragment for EvToolDelta streaming, and plain-text tool output.
	Text string `json:"text,omitempty"`

	// ImageURL is an http(s) URL or a data: URL (base64 inline image).
	ImageURL string `json:"image_url,omitempty"`

	// ToolCallID identifies the call for both PartToolUse and PartToolResult.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName is the function name for PartToolUse.
	ToolName string `json:"tool_name,omitempty"`
	// ToolArgsJSON is the raw JSON object string for PartToolUse. May be ""
	// while streaming (arguments arrive incrementally).
	ToolArgsJSON string `json:"tool_args,omitempty"`
	// ToolOutput is the tool result payload for PartToolResult (text; JSON
	// results are stringified by the codec that parsed them).
	ToolOutput string `json:"tool_output,omitempty"`
	// IsError marks a failed tool result (Anthropic is_error).
	IsError bool `json:"is_error,omitempty"`
}

// Message is one conversation turn.
type Message struct {
	Role    Role   `json:"role"`
	Content []Part `json:"content"`
}

// Tool is a function tool definition.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// ParametersJSON is a JSON Schema object ({"type":"object",...}).
	ParametersJSON json.RawMessage `json:"parameters,omitempty"`
}

// ToolChoiceType enumerates tool selection modes.
type ToolChoiceType string

const (
	ToolChoiceAuto     ToolChoiceType = "auto"
	ToolChoiceNone     ToolChoiceType = "none"
	ToolChoiceRequired ToolChoiceType = "required"
	ToolChoiceNamed    ToolChoiceType = "tool"
)

// ToolChoice selects tool behavior for a request.
type ToolChoice struct {
	Type ToolChoiceType `json:"type"`
	Name string         `json:"name,omitempty"` // only for ToolChoiceNamed
}

// ResponseFormat requests structured output ("json_object" today).
type ResponseFormat struct {
	Type string `json:"type"`
}

// Sampling holds generation parameters. Nil pointers mean "unset" so codecs
// can distinguish "client said 0.7" from "client said nothing".
type Sampling struct {
	MaxTokens   int64    `json:"max_tokens,omitempty"` // 0 = unset
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	TopK        *int64   `json:"top_k,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
}

// Request is a normalized LLM call.
type Request struct {
	// Model is the client-supplied alias, exactly as received. The router —
	// never a codec — resolves it to a concrete upstream model.
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	Tools      []Tool      `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	Sampling       Sampling        `json:"sampling"`
	Stream         bool            `json:"stream,omitempty"`

	// Session is the stickiness hint for routing (x-kln-session header or a
	// synthesized hash). Empty means no affinity requested.
	Session string `json:"session,omitempty"`

	// Raw preserves unknown client fields as raw JSON, keyed by field name.
	// Codecs copy a field into Raw only when it is well-formed JSON they do
	// not understand; the server may pass matching fields through when client
	// and upstream formats are identical.
	Raw map[string]json.RawMessage `json:"raw,omitempty"`
}

// FirstUserText returns the first user text part (best-effort logging helper).
func (r *Request) FirstUserText() string {
	for _, m := range r.Messages {
		if m.Role != RoleUser {
			continue
		}
		for _, p := range m.Content {
			if p.Type == PartText && p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// Finish is a normalized stop reason.
type Finish string

const (
	FinishStop          Finish = "stop"
	FinishToolCalls     Finish = "tool_calls"
	FinishLength        Finish = "length"
	FinishContentFilter Finish = "content_filter"
)

// Usage is the normalized metering record. Upstream-reported numbers are
// authoritative; Estimated=true marks locally-derived fallback numbers.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	Estimated        bool  `json:"estimated,omitempty"`
}

// Total returns input+output tokens (cache and reasoning tokens are already
// contained in the provider's input/output counts; they are informational
// carve-outs, not additive).
func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

// Add merges two Usage records (streaming accumulation across chunks).
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// Response is a complete (non-streaming) normalized result.
type Response struct {
	// Model is the upstream model that actually served the call.
	Model        string `json:"model"`
	Content      []Part `json:"content"`
	FinishReason Finish `json:"finish_reason"`
	Usage        Usage  `json:"usage"`
}

// StreamKind discriminates IR stream events.
type StreamKind string

const (
	// EvStart opens the logical message. Model carries the serving model.
	EvStart StreamKind = "start"
	// EvTextDelta appends visible text (Text).
	EvTextDelta StreamKind = "text_delta"
	// EvThinkingDelta appends reasoning text (Text).
	EvThinkingDelta StreamKind = "thinking_delta"
	// EvToolStart opens a tool call (ToolID, ToolName).
	EvToolStart StreamKind = "tool_start"
	// EvToolDelta appends a partial JSON argument fragment (ToolID, Text).
	EvToolDelta StreamKind = "tool_delta"
	// EvToolEnd closes a tool call (ToolID). Arguments are final at this point.
	EvToolEnd StreamKind = "tool_end"
	// EvFinish reports the stop reason (Finish). Exactly one per stream.
	EvFinish StreamKind = "finish"
	// EvUsage carries the authoritative final usage. Emitted once, before or
	// at EvFinish; codecs must tolerate absence (estimate fallback upstream).
	EvUsage StreamKind = "usage"
	// EvError aborts the stream. Nothing valid follows.
	EvError StreamKind = "error"
)

// StreamEvent is a single normalized streaming event. The single-struct shape
// keeps codec plumbing trivial; unused fields are zero for the kind.
type StreamEvent struct {
	Kind     StreamKind `json:"kind"`
	Model    string     `json:"model,omitempty"`
	Text     string     `json:"text,omitempty"`
	ToolID   string     `json:"tool_id,omitempty"`
	ToolName string     `json:"tool_name,omitempty"`
	Finish   Finish     `json:"finish,omitempty"`
	Usage    Usage      `json:"usage,omitempty"`

	// Error fields (EvError).
	ErrCode    string `json:"err_code,omitempty"`
	ErrMsg     string `json:"err_msg,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// UpstreamError is the normalized cross-provider error. Every upstream codec
// converts provider-specific error bodies into this shape; the server converts
// it back into the client's native error envelope.
type UpstreamError struct {
	// Status is the upstream HTTP status; 0 for transport-level failures.
	Status int `json:"status"`
	// Kind is one of: invalid_key, rate_limited, quota, context_length,
	// not_found, upstream_error, network.
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %s: %s (status %d, retryable=%v)", e.Kind, e.Message, e.Status, e.Retryable)
}

// Common UpstreamError kinds.
const (
	ErrInvalidKey    = "invalid_key"
	ErrRateLimited   = "rate_limited"
	ErrQuota         = "quota"
	ErrContextLength = "context_length"
	ErrNotFound      = "not_found"
	ErrUpstream      = "upstream_error"
	ErrNetwork       = "network"
)
