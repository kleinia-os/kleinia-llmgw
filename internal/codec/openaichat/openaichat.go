// Package openaichat implements the OpenAI Chat Completions wire format on
// both sides of the gateway: it serves clients
// that speak /v1/chat/completions and it drives OpenAI-compatible upstreams.
//
// The package is pure translation between the wire format and internal/ir:
// no logging, no DB, no alias resolution, no sockets. new-api-compatible quirks
// that shaped this codec:
//
//   - Assistant messages carry a non-standard "reasoning_content" field
//     (a convention of several reasoning-model providers) both streaming and non-streaming; it maps to
//     ir.PartThinking / EvThinkingDelta.
//   - Streaming usage rides on the final finish_reason chunk (no separate
//     empty-choices usage chunk, no stream_options required upstream-side).
//   - Every stream chunk carries content and reasoning_content, one of them
//     usually "" — empty deltas must be dropped, not forwarded.
//   - Error bodies use the {"error":{"code","message","type":"new_api_error"}}
//     envelope; HTTP 403 covers both unknown models and unauthorized tokens.
package openaichat

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"kleinia.io/llmgw/internal/ir"
)

// Compile-time guarantees that the encoder/parser satisfy the shapes
// internal/server wires up (server.StreamEncoder / server.StreamParser).
var _ interface {
	Emit(ev ir.StreamEvent) error
	Close(finish ir.Finish) error
} = (*ClientStreamEncoder)(nil)

var _ interface {
	Line(line string) ([]ir.StreamEvent, error)
} = (*UpstreamStreamParser)(nil)

// randID returns a fresh response id ("chatcmpl-<random hex>").
func randID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(b)
}

// finishFromWire maps an upstream finish_reason to IR. Unknown values (and
// the empty string, meaning "not set") map to stop.
func finishFromWire(s string) ir.Finish {
	switch s {
	case "tool_calls", "function_call":
		return ir.FinishToolCalls
	case "length":
		return ir.FinishLength
	case "content_filter":
		return ir.FinishContentFilter
	default:
		return ir.FinishStop
	}
}

// finishToWire maps an IR finish reason back to the wire value.
func finishToWire(f ir.Finish) string {
	switch f {
	case ir.FinishToolCalls:
		return "tool_calls"
	case ir.FinishLength:
		return "length"
	case ir.FinishContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}
