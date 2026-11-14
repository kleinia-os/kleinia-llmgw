package openaichat

import (
	"strings"

	"kleinia.io/llmgw/internal/ir"
)

// wireErr is the new_api_error / OpenAI error body:
// {"message":..., "type":"new_api_error", "code":"", "param":""}.
type wireErr struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param"`
}

const maxErrBodySnippet = 200 // bytes of raw body kept when no envelope parses

// errorFromEnvelope classifies a parsed error envelope into *ir.UpstreamError.
// The wire gives us no HTTP status, so classification is content-based; the
// server stamps the real status and retryability afterwards.
func errorFromEnvelope(env *wireErr, body []byte) error {
	msg := strings.TrimSpace(env.Message)
	if msg == "" {
		return unparseableUpstreamError(body, nil)
	}
	kind, retryable := classifyError(msg, env.Type, env.Code)
	return &ir.UpstreamError{Kind: kind, Message: msg, Retryable: retryable}
}

// unparseableUpstreamError is the fallback: ErrUpstream plus a truncated copy
// of the raw body (HTML error pages, proxies garbage, ...).
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
	return &ir.UpstreamError{Kind: ir.ErrUpstream, Message: msg, Retryable: true}
}

// classifyError maps envelope text onto UpstreamError kinds. Order matters:
// each list is checked in the sequence below, most specific signal first.
// new-api specifics: 401 "无效的令牌" and 403 "该令牌无权访问模型 ..." are both
// key/permission failures → invalid_key (the upstream merges unknown-model and
// no-access into one 403, so they cannot be told apart on the wire anyway).
func classifyError(msg, typ, code string) (kind string, retryable bool) {
	m := strings.ToLower(msg)
	t := strings.ToLower(typ + " " + code)
	all := m + " " + t

	switch {
	case containsAny(all, invalidKeySignals):
		return ir.ErrInvalidKey, false
	case containsAny(all, rateLimitSignals):
		return ir.ErrRateLimited, true
	case containsAny(all, quotaSignals):
		return ir.ErrQuota, false
	case containsAny(all, contextSignals):
		return ir.ErrContextLength, false
	case containsAny(all, notFoundSignals):
		return ir.ErrNotFound, false
	case strings.Contains(t, "invalid_request"):
		// Our request is malformed for this upstream; retrying identical
		// requests will not help.
		return ir.ErrUpstream, false
	default:
		return ir.ErrUpstream, true
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

var invalidKeySignals = []string{
	"无效的令牌", "令牌无权", "无权访问", "鉴权", "身份认证",
	"invalid api key", "invalid_api_key", "incorrect api key", "api key",
	"authentication", "unauthorized", "permission denied", "forbidden",
	"access denied", "无权",
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
