package server

import (
	"context"
	"net/http"

	"kleinia.io/llmgw/internal/codec/anthropic"
	"kleinia.io/llmgw/internal/codec/openaichat"
	"kleinia.io/llmgw/internal/codec/openairesponses"
	"kleinia.io/llmgw/internal/ir"
)

// WireDefault returns the default codec wiring: client formats chat /
// responses / anthropic, upstream types openai (chat-completions transport)
// and anthropic (messages transport).
func WireDefault() (map[string]ClientCodec, map[string]UpstreamCodec) {
	clients := map[string]ClientCodec{
		"chat": {
			Parse:     openaichat.ParseClientRequest,
			WriteResp: openaichat.WriteClientResponse,
			NewEnc: func(w http.ResponseWriter) StreamEncoder {
				return openaichat.NewClientStreamEncoder(w)
			},
			WriteErr: openaichat.WriteClientError,
		},
		"responses": {
			Parse:     openairesponses.ParseClientRequest,
			WriteResp: openairesponses.WriteClientResponse,
			NewEnc: func(w http.ResponseWriter) StreamEncoder {
				return openairesponses.NewClientStreamEncoder(w)
			},
			WriteErr: openairesponses.WriteClientError,
		},
		"anthropic": {
			Parse:     anthropic.ParseClientRequest,
			WriteResp: anthropic.WriteClientResponse,
			NewEnc: func(w http.ResponseWriter) StreamEncoder {
				return anthropic.NewClientStreamEncoder(w)
			},
			WriteErr: anthropic.WriteClientError,
		},
	}
	upstreams := map[string]UpstreamCodec{
		"openai": {
			Build: func(ctx context.Context, baseURL, apiKey, model string, req ir.Request, stream bool) (*http.Request, error) {
				return openaichat.BuildUpstreamRequest(ctx, baseURL, apiKey, model, req, stream)
			},
			ParseBody: openaichat.ParseUpstreamBody,
			NewParser: func() StreamParser { return openaichat.NewUpstreamStreamParser() },
		},
		"openai-responses": {
			Build: func(ctx context.Context, baseURL, apiKey, model string, req ir.Request, stream bool) (*http.Request, error) {
				return openairesponses.BuildUpstreamRequest(ctx, baseURL, apiKey, model, req, stream)
			},
			ParseBody: openairesponses.ParseUpstreamBody,
			NewParser: func() StreamParser { return openairesponses.NewUpstreamStreamParser() },
		},
		"anthropic": {
			Build: func(ctx context.Context, baseURL, apiKey, model string, req ir.Request, stream bool) (*http.Request, error) {
				return anthropic.BuildUpstreamRequest(ctx, baseURL, apiKey, model, req, stream)
			},
			ParseBody: anthropic.ParseUpstreamBody,
			NewParser: func() StreamParser { return anthropic.NewUpstreamStreamParser() },
		},
	}
	return clients, upstreams
}
