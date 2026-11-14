package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"
)

// newReqID mints a per-request correlation id (audit log ↔ usage row ↔
// response header ↔ future cnode segments). Empty string means minting
// failed: no correlation is possible and writeReqID must stay silent.
func newReqID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return "req_" + hex.EncodeToString(b[:])
}

// ReqIDHeader carries the gateway's internal request id back to the caller
// so agenttrace spans can be reconciled 1:1 against usage rows (the value
// is exactly the usage.req_id / audit-log req_id, never an upstream id).
// It is set only where at least one usage row will exist — see serve.go.
const ReqIDHeader = "X-Kln-Req-Id"

// writeReqID exposes the correlation id on the response. An empty id (mint
// failure) writes nothing: clients fall back to session+time-window
// matching instead of correlating against a non-unique sentinel.
func writeReqID(w http.ResponseWriter, id string) {
	if id != "" {
		w.Header().Set(ReqIDHeader, id)
	}
}

func readAll(r *http.Request) []byte {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return nil
	}
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string { return strconv.Itoa(n) }

// estimateTokens is a crude local fallback used only when an upstream reports
// no usage: roughly 1 token per 2 runes for CJK-heavy text, 1 per 4 for ASCII.
// Always marked Estimated=true in usage rows.
func estimateTokens(s string) int {
	runes := utf8.RuneCountInString(s)
	if runes == 0 {
		return 0
	}
	ascii := 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		}
	}
	nonASCII := runes - ascii
	return (ascii+3)/4 + (nonASCII+1)/2
}
