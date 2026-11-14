// Package anthropic implements the Anthropic Messages wire format on both
// sides of the llmgw gateway: the client format the gateway serves at
// /v1/messages and the upstream format it speaks to Anthropic-compatible
// providers.
//
// new-api-compatible specifics baked into the conversions (sampled in
// testdata/):
//   - usage: anthropic input_tokens EXCLUDES cache-read tokens while
//     ir.Usage.InputTokens INCLUDES them (OpenAI semantics), so every
//     boundary converts by adding/subtracting CacheReadTokens.
//   - thinking blocks carry no signature, so they cannot be replayed to a
//     strict upstream and are dropped when building requests.
//   - the error body is the OpenAI-style envelope {"error":{...}} with type
//     "new_api_error", not the official anthropic envelope; both are parsed.
//   - max_tokens and the anthropic-version header are not enforced by such upstreams;
//     we always send them anyway because real Anthropic requires both.
package anthropic

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"kleinia.io/llmgw/internal/ir"
)

// defaultMaxTokens is substituted when a request carries no max_tokens. The
// field is required by the anthropic spec, but new-api upstreams tolerate its
// absence, so
// parsing stays lenient and the default is filled in here.
const defaultMaxTokens = 4096

// anthropicVersion is the protocol version header sent upstream.
const anthropicVersion = "2023-06-01"

// finishToIR maps an anthropic stop_reason to the normalized IR finish.
// "pause_turn" (server-tool pause) has no IR spelling and reads as stop.
func finishToIR(reason string) ir.Finish {
	switch reason {
	case "tool_use":
		return ir.FinishToolCalls
	case "max_tokens":
		return ir.FinishLength
	case "refusal":
		return ir.FinishContentFilter
	default: // "end_turn", "stop_sequence", "pause_turn", unknown
		return ir.FinishStop
	}
}

// finishFromIR maps a normalized finish back to an anthropic stop_reason.
func finishFromIR(f ir.Finish) string {
	switch f {
	case ir.FinishToolCalls:
		return "tool_use"
	case ir.FinishLength:
		return "max_tokens"
	case ir.FinishContentFilter:
		return "refusal"
	default: // FinishStop and anything unrecognized
		return "end_turn"
	}
}

// wireUsage is the anthropic usage object. IR input tokens include the
// cache-read carve-out; anthropic's do not, so the conversion below subtracts
// it on the way out and adds it back on the way in. The cache fields are
// emitted only when nonzero (clients read missing fields as zero).
type wireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens"`
}

// usageFromIR converts IR usage to anthropic semantics: input_tokens
// excludes cache-read tokens, output_tokens includes reasoning (same as IR).
func usageFromIR(u ir.Usage) wireUsage {
	in := u.InputTokens - u.CacheReadTokens
	if in < 0 {
		in = 0
	}
	return wireUsage{
		InputTokens:              in,
		CacheCreationInputTokens: u.CacheWriteTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		OutputTokens:             u.OutputTokens,
	}
}

// upstreamUsage is an upstream-reported usage object. The upstream additionally
// nests the original OpenAI usage under billing_usage; the reasoning token
// count only exists there (the top-level anthropic object has no such field).
type upstreamUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	BillingUsage             *struct {
		OpenAIUsage *struct {
			CompletionTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"openai_usage"`
	} `json:"billing_usage"`
}

// toIR converts an upstream usage object to IR semantics: the IR input count
// re-includes cache-read tokens, output already includes reasoning.
func (u upstreamUsage) toIR() ir.Usage {
	out := ir.Usage{
		InputTokens:      u.InputTokens + u.CacheReadInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	if u.BillingUsage != nil && u.BillingUsage.OpenAIUsage != nil &&
		u.BillingUsage.OpenAIUsage.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.BillingUsage.OpenAIUsage.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

// wire content blocks shared by requests, responses and streams.

type textBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type thinkingBlock struct {
	Type     string `json:"type"` // "thinking"
	Thinking string `json:"thinking"`
}

type toolUseBlock struct {
	Type  string          `json:"type"` // "tool_use"
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"` // parsed JSON object, never a string
}

type toolResultBlock struct {
	Type      string `json:"type"` // "tool_result"
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

type imageBlock struct {
	Type   string      `json:"type"` // "image"
	Source imageSource `json:"source"`
}

type imageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// wireTool is an anthropic tool definition ({name, description, input_schema}).
type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// wireBlock is the union of every content block shape the codec accepts.
// Unknown fields inside known block types (cache_control, signature, ...) are
// ignored: the IR cannot express them.
type wireBlock struct {
	Type     string       `json:"type"`
	Text     string       `json:"text"`
	Thinking string       `json:"thinking"`
	Source   *imageSource `json:"source"`
	// ImageURL is a non-standard OpenAI-ish {"type":"image_url"} variant.
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// compactArgs minifies a raw tool input object for ToolArgsJSON; absent or
// invalid input becomes "{}".
func compactArgs(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "{}"
	}
	return buf.String()
}

// rawToolInput renders tool arguments as the wire's parsed input object;
// missing or invalid JSON degrades to {} rather than failing the message.
func rawToolInput(args string) json.RawMessage {
	if args == "" || !json.Valid([]byte(args)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}

// toolResultOutput flattens a tool_result content field (plain string or
// block array) into the plain text the IR carries; non-text blocks (images)
// have no IR spelling and are dropped.
func toolResultOutput(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
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
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// newMessageID returns an anthropic-style id: "msg_" + 24 hex chars.
func newMessageID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Linux; keep the id unique anyway.
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (uint(i%8) * 8))
		}
	}
	return "msg_" + hex.EncodeToString(b[:])
}
