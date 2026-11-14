package openairesponses

import (
	"encoding/json"
	"strings"

	"kleinia.io/llmgw/internal/ir"
)

// wireError is the new-api / OpenAI error envelope the upstream emits on every
// endpoint:
// {"message":..., "type":"new_api_error", "code":"", "param":""}.
type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param"`
}

const maxErrBodySnippet = 200 // bytes of raw body kept when no envelope parses

// errorFromEnvelope classifies a parsed error envelope into *ir.UpstreamError.
// raw may be nil (status:"failed" without an error object) or unparseable;
// both fall back to the raw-body snippet.
func errorFromEnvelope(raw json.RawMessage, body []byte) error {
	var env wireError
	if len(raw) == 0 || json.Unmarshal(raw, &env) != nil || strings.TrimSpace(env.Message) == "" {
		return unparseableUpstreamError(body, nil)
	}
	return classifyUpstreamError(strings.TrimSpace(env.Message), env.Type, env.Code)
}

// unparseableUpstreamError is the fallback: ErrUpstream plus a truncated copy
// of the raw body (HTML error pages, proxy garbage, ...).
func unparseableUpstreamError(body []byte, cause error) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = "empty upstream body"
	}
	if cause != nil {
		msg = "malformed JSON: " + msg
	}
	if len(msg) > maxErrBodySnippet {
		msg = msg[:maxErrBodySnippet]
	}
	return &ir.UpstreamError{Status: 502, Kind: ir.ErrUpstream, Message: msg, Retryable: true}
}

// classifyUpstreamError maps envelope text onto UpstreamError kinds with a
// best-guess HTTP status. Order matters: each list is checked most specific
// signal first. new-api specifics: 401 "无效的令牌" and 403 "该令牌无权访问模型"
// are both key/permission failures → invalid_key (the upstream merges unknown-model
// and no-access into one 403, so they cannot be told apart on the wire).
func classifyUpstreamError(msg, typ, code string) *ir.UpstreamError {
	m := strings.ToLower(msg)
	t := strings.ToLower(typ + " " + code)
	all := m + " " + t

	switch {
	case containsAny(all, authSignals):
		return &ir.UpstreamError{Status: 401, Kind: ir.ErrInvalidKey, Message: msg}
	case containsAny(all, forbiddenSignals):
		return &ir.UpstreamError{Status: 403, Kind: ir.ErrInvalidKey, Message: msg}
	case containsAny(all, rateLimitSignals):
		return &ir.UpstreamError{Status: 429, Kind: ir.ErrRateLimited, Message: msg, Retryable: true}
	case containsAny(all, quotaSignals):
		return &ir.UpstreamError{Status: 429, Kind: ir.ErrQuota, Message: msg}
	case containsAny(all, contextSignals):
		return &ir.UpstreamError{Status: 400, Kind: ir.ErrContextLength, Message: msg}
	case containsAny(all, notFoundSignals):
		return &ir.UpstreamError{Status: 404, Kind: ir.ErrNotFound, Message: msg}
	case strings.Contains(t, "invalid_request"), containsAny(all, badRequestSignals):
		// Our request is malformed for this upstream; retrying an identical
		// request will not help.
		return &ir.UpstreamError{Status: 400, Kind: ir.ErrUpstream, Message: msg}
	default:
		// Unknown envelope: assume a transient gateway problem; the server
		// overrides status/retryable from the real HTTP response anyway.
		return &ir.UpstreamError{Status: 502, Kind: ir.ErrUpstream, Message: msg, Retryable: true}
	}
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

var authSignals = []string{
	"无效的令牌", "鉴权", "身份认证",
	"invalid api key", "invalid_api_key", "incorrect api key",
	"authentication", "unauthorized",
}

var forbiddenSignals = []string{
	"无权访问模型", "令牌无权", "无权",
	"forbidden", "permission denied", "access denied",
}

var rateLimitSignals = []string{
	"rate limit", "rate_limit", "ratelimit", "too many requests",
	"请求过于频繁", "频率过高", "负载已饱和", "requests per minute",
	"slow down",
}

var quotaSignals = []string{
	"insufficient_quota", "quota exceeded", "quota", "余额不足", "欠费", "额度",
}

var contextSignals = []string{
	"context_length", "context length", "context window", "maximum context",
	"max context", "too many tokens", "token limit", "tokens exceed",
	"exceeds the model", "exceeds the maximum", "reduce the length",
	"reduce your prompt", "上下文", "输入长度", "长度超", "输入过长",
}

var notFoundSignals = []string{
	"model not found", "model_not_found", "no such model", "unknown model",
	"unknown_model", "does not exist", "not found", "模型不存在", "未知模型",
}

// badRequestSignals catch the upstream's no-code 400 bodies ("无效的请求，invalid JSON
// request body", "field ... is required") that carry no invalid_request code.
var badRequestSignals = []string{
	"无效的请求", "invalid json", "is required",
}
