// Package openairesponses implements the OpenAI Responses API (/v1/responses)
// on both sides: parsing client request bodies into ir.Request and writing
// complete responses, SSE streams, and errors back in the Responses wire
// format; and building/parsing requests for Responses-transport upstreams.
//
// Wire shapes follow the real samples captured from a live upstream
// (copied to testdata/): flat function tools, item-based input
// (message / function_call / function_call_output / reasoning), output items
// ordered reasoning → function_call → message, and a named-event SSE stream
// with a monotonically increasing sequence_number on every event.
package openairesponses

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// randID returns "<prefix>_<32 hex chars>", matching the id shapes observed
// in the goldens (resp_, rs_, msg_, fc_ + 32 hex).
func randID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return prefix + "_" + hex.EncodeToString(b[:])
}

// WriteClientError writes a non-streaming error in the Responses error
// envelope. status is passed through unchanged.
func WriteClientError(w http.ResponseWriter, status int, kind, msg string) error {
	body := struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   any    `json:"param"`
			Code    any    `json:"code"`
		} `json:"error"`
	}{}
	body.Error.Message = msg
	body.Error.Type = kind
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(body)
}
