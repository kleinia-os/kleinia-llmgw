// Package server assembles the llmgw HTTP surface: auth → quota → route →
// codec-translate → account. It owns no translation logic itself; wire formats
// live in internal/codec/* and are referenced through the small interfaces
// below so the server stays testable with fake codecs.
package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"kleinia.io/llmgw/internal/ir"
	"kleinia.io/llmgw/internal/quota"
	"kleinia.io/llmgw/internal/router"
	"kleinia.io/llmgw/internal/store"
)

// ClientCodec is the contract every client-side codec fulfills.
type ClientCodec struct {
	Parse     func(body []byte) (ir.Request, error)
	WriteResp func(w http.ResponseWriter, res ir.Response) error
	NewEnc    func(w http.ResponseWriter) StreamEncoder
	WriteErr  func(w http.ResponseWriter, status int, kind, msg string) error
}

// StreamEncoder writes client-side SSE.
type StreamEncoder interface {
	Emit(ev ir.StreamEvent) error
	Close(finish ir.Finish) error
}

// UpstreamCodec is the upstream-side contract.
type UpstreamCodec struct {
	Build     func(ctx context.Context, baseURL, apiKey, model string, req ir.Request, stream bool) (*http.Request, error)
	ParseBody func(body []byte) (ir.Response, error)
	NewParser func() StreamParser
}

// StreamParser consumes upstream SSE lines and yields IR events.
type StreamParser interface {
	Line(line string) ([]ir.StreamEvent, error)
}

// Config controls the server.
type Config struct {
	Addr           string
	RequestTimeout time.Duration // whole call, non-streaming
	StreamTimeout  time.Duration // whole call, streaming
	// RetryFallbacks is how many additional sources to try when an upstream
	// fails with a retryable error before the first byte of payload.
	RetryFallbacks int

	// InternalToken, when non-empty, authenticates the sys.llm skill
	// carrier's forwarded agentfs calls: a request bearing
	// "x-kln-internal: <token>" is attributed to the kernel agent identity
	// in "x-kln-aid" without a gateway key.
	InternalToken string
	// AIDDailyDefault bounds AID-only identities without a bound key row
	// (nil = unlimited).
	AIDDailyDefault *int64

	// OnQuotaWarn, when set, is invoked synchronously whenever an allowed
	// request has consumed >=80% of the daily-token window; ident is
	// "key:<name>" or "aid:<aid>".
	OnQuotaWarn func(ident, window string, pct float64)
}

// Server is the llmgw daemon core.
type Server struct {
	cfg   Config
	st    *store.Store
	rt    *router.Router
	codec map[string]ClientCodec   // by client format: chat|responses|anthropic
	up    map[string]UpstreamCodec // by source type: openai|anthropic
	price *Pricer
	httpc *http.Client
}

// New builds a Server over an opened store and router.
func New(cfg Config, st *store.Store, rt *router.Router, price *Pricer) *Server {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 300 * time.Second
	}
	if cfg.StreamTimeout == 0 {
		cfg.StreamTimeout = 600 * time.Second
	}
	if cfg.RetryFallbacks == 0 {
		cfg.RetryFallbacks = 1
	}
	return &Server{
		cfg:   cfg,
		st:    st,
		rt:    rt,
		price: price,
		httpc: &http.Client{Timeout: cfg.StreamTimeout},
	}
}

// SetCodecs installs the codec wiring (client formats and upstream types).
func (s *Server) SetCodecs(clients map[string]ClientCodec, upstreams map[string]UpstreamCodec) {
	s.codec = clients
	s.up = upstreams
}

// Handler returns the fully-wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleLLM("chat"))
	mux.HandleFunc("POST /v1/responses", s.handleLLM("responses"))
	mux.HandleFunc("POST /v1/messages", s.handleLLM("anthropic"))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("GET /internal/usage", s.handleInternalUsage)
	return mux
}

// handleInternalUsage serves the sys.llm carrier (internal token): usage
// aggregates and recent rows as JSON. Read-only, metadata only.
func (s *Server) handleInternalUsage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.InternalToken == "" || r.Header.Get("x-kln-internal") != s.cfg.InternalToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "internal token required"})
		return
	}
	since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	byKey, byModel, bySource, byAid, err := s.st.SummarySince(since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	recent, err := s.st.RecentUsage(20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"since": since, "by_key": byKey, "by_model": byModel,
		"by_source": bySource, "by_aid": byAid, "recent": recent,
	})
}

// RefreshSources reloads source configuration from the store into the router.
func (s *Server) RefreshSources() {
	srcs, err := s.st.ListSources()
	if err != nil {
		log.Printf("server: refresh sources: %v", err)
		return
	}
	var rs []router.Source
	for _, src := range srcs {
		maps, err := s.st.SourceModels(src.ID)
		if err != nil {
			log.Printf("server: source %s models: %v", src.Name, err)
			continue
		}
		m := make(map[string]string, len(maps))
		for _, mm := range maps {
			m[mm.Alias] = mm.UpstreamModel
		}
		rs = append(rs, router.Source{
			ID: src.ID, Name: src.Name, Type: src.Type,
			BaseURL: src.BaseURL, APIKey: src.APIKey,
			Priority: src.Priority, Weight: src.Weight, Enabled: src.Enabled,
			Models: m,
		})
	}
	s.rt.Update(rs)
	log.Printf("server: router refreshed: %d sources", len(rs))
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	set := map[string]bool{}
	for _, src := range s.rt.Sources() {
		for alias := range src.Models {
			set[alias] = true
		}
	}
	names := make([]string, 0, len(set))
	for a := range set {
		names = append(names, a)
	}
	sort.Strings(names)
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := make([]model, 0, len(names))
	for _, n := range names {
		out = append(out, model{ID: n, Object: "model", OwnedBy: "llmgw"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// handleCountTokens is a rough local estimator (Anthropic endpoint shape).
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
		System   json.RawMessage `json:"system"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	n := estimateTokens(string(body.Messages)) + estimateTokens(string(body.System))
	writeJSON(w, http.StatusOK, map[string]int64{"input_tokens": int64(n)})
}

// handleLLM is the shared pipeline for all three client formats.
func (s *Server) handleLLM(format string) http.HandlerFunc {
	cc, ok := s.codec[format]
	if !ok {
		panic("server: no codec registered for format " + format)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body := readAll(r)
		req, err := cc.Parse(body)
		if err != nil {
			cc.WriteErr(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		session := r.Header.Get("x-kln-session")

		key, ok := s.authenticate(r, cc, w)
		if !ok {
			return
		}
		if !s.checkACL(key, req.Model, cc, w) {
			return
		}
		if !s.checkQuota(key, cc, w) {
			return
		}

		s.serve(w, r, serveParams{
			cc: cc, format: format, key: key, req: req, session: session, start: start,
			reqID: newReqID(),
		})
	}
}

// authenticate resolves the caller's identity: either a gateway key (Bearer
// or x-api-key) or, when the request carries the sys.llm internal token, the
// kernel-attested agent identity in x-kln-aid (no gateway key needed — the
// kernel already attested the caller at the agentfs boundary).
func (s *Server) authenticate(r *http.Request, cc ClientCodec, w http.ResponseWriter) (store.Key, bool) {
	if s.cfg.InternalToken != "" && r.Header.Get("x-kln-internal") == s.cfg.InternalToken {
		aid := r.Header.Get("x-kln-aid")
		name := r.Header.Get("x-kln-agent")
		if aid == "" && name == "" {
			cc.WriteErr(w, http.StatusBadRequest, "invalid_request", "internal auth needs x-kln-aid or x-kln-agent")
			return store.Key{}, false
		}
		if aid != "" {
			if k, err := s.st.FindKeyByAID(aid); err == nil {
				return k, true // agentd minted a bound key: inherit limits/ACL
			}
		}
		if name == "" {
			name = "agent:" + aid
		}
		return store.Key{Name: name, Enabled: true, Models: "*",
			AID: aid, DailyTokens: s.cfg.AIDDailyDefault}, true
	}
	token := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimPrefix(h, "Bearer ")
	} else if h := r.Header.Get("x-api-key"); h != "" {
		token = h
	}
	if token == "" {
		cc.WriteErr(w, http.StatusUnauthorized, "invalid_key", "missing API key")
		return store.Key{}, false
	}
	key, err := s.st.FindKey(token)
	if err != nil {
		cc.WriteErr(w, http.StatusUnauthorized, "invalid_key", "unknown, suspended or revoked key")
		return store.Key{}, false
	}
	return key, true
}

func (s *Server) checkACL(key store.Key, alias string, cc ClientCodec, w http.ResponseWriter) bool {
	if key.Models == "*" {
		return true
	}
	for _, m := range strings.Split(key.Models, ",") {
		if strings.TrimSpace(m) == alias {
			return true
		}
	}
	cc.WriteErr(w, http.StatusForbidden, "model_not_allowed",
		"key '"+key.Name+"' is not allowed to use model '"+alias+"'")
	return false
}

func (s *Server) checkQuota(key store.Key, cc ClientCodec, w http.ResponseWriter) bool {
	var rpm, tpm, daily int64
	var err error
	if key.ID != 0 {
		rpm, tpm, daily, err = s.st.KeyWindows(key.ID)
	} else if key.AID != "" {
		rpm, tpm, daily, err = s.st.AidWindows(key.AID)
	} else {
		return true // synthetic identity without metering: allow
	}
	if err != nil {
		log.Printf("server: quota windows: %v", err)
		return true // fail open on metering errors, never block on our own bug
	}
	v := quota.Check(rpm, tpm, daily, quota.Limits{RPM: key.RPM, TPM: key.TPM, DailyTokens: key.DailyTokens})
	if v.Allowed {
		s.warnQuota(key, daily)
		return true
	}
	w.Header().Set("Retry-After", itoa(v.RetryAfterSec))
	cc.WriteErr(w, http.StatusTooManyRequests, "rate_limited", v.Reason)
	return false
}

// warnQuota fires the >=80% soft-budget hook for the daily window.
func (s *Server) warnQuota(key store.Key, daily int64) {
	if s.cfg.OnQuotaWarn == nil || key.DailyTokens == nil || *key.DailyTokens <= 0 || daily <= 0 {
		return
	}
	pct := float64(daily) / float64(*key.DailyTokens)
	if pct < 0.8 {
		return
	}
	ident := "key:" + key.Name
	if key.AID != "" {
		ident = "aid:" + key.AID
	}
	s.cfg.OnQuotaWarn(ident, "daily", pct)
}
