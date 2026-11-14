package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"kleinia.io/llmgw/internal/ir"
	"kleinia.io/llmgw/internal/router"
	"kleinia.io/llmgw/internal/store"
)

const maxBodyBytes = 32 << 20 // 32 MiB request body cap

type serveParams struct {
	cc      ClientCodec
	format  string
	key     store.Key
	req     ir.Request
	session string
	reqID   string
	start   time.Time
}

// serve runs the full pipeline: route → upstream (with bounded fallback
// retries before any payload byte) → translate → account.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, p serveParams) {
	exclude := map[int64]bool{}
	for attempt := 0; attempt <= s.cfg.RetryFallbacks; attempt++ {
		src, err := s.rt.PickExcluding(p.req.Model, p.session, exclude)
		if err != nil {
			if errors.Is(err, router.ErrUnknownModel) {
				p.cc.WriteErr(w, http.StatusNotFound, "model_not_found",
					"no source provides model '"+p.req.Model+"'")
			} else {
				p.cc.WriteErr(w, http.StatusBadGateway, "upstream_error", err.Error())
			}
			return
		}
		exclude[src.ID] = true
		uc, ok := s.up[src.Type]
		if !ok {
			log.Printf("server: source %q has unsupported type %q", src.Name, src.Type)
			s.rt.ReportFailure(src.ID)
			continue
		}

		// From here on every exit path (success, terminal error, retryable
		// failure that exhausts the loop) writes at least one usage row, so
		// the response may carry the req id. Requests rejected earlier
		// (parse/auth/ACL/quota/routing) write no row and must NOT carry
		// it: a visible id without a usage row reads as a bypass call to
		// the agenttrace reconciliation.
		writeReqID(w, p.reqID)

		if p.req.Stream {
			done := s.serveStream(w, r, uc, src, p, attempt > 0)
			if done {
				return
			}
			// stream failed before first byte → fall through to retry
			continue
		}
		if s.serveOnce(w, r, uc, src, p, attempt > 0) {
			return
		}
		// non-stream failed retryably before responding → retry next source
	}
	// exhausted retries; last error already written by serveOnce/serveStream
	// unless we got here via unsupported-type continues.
	p.cc.WriteErr(w, http.StatusBadGateway, "upstream_error", "all candidate sources failed")
}

// serveOnce performs one non-streaming upstream attempt. It returns true if
// the response is finished (success or terminal error written to w).
func (s *Server) serveOnce(w http.ResponseWriter, r *http.Request, uc UpstreamCodec, src *router.Source, p serveParams, degraded bool) bool {
	res, uerr, latency := s.execUpstream(r.Context(), uc, src, p.req)
	if uerr != nil {
		s.rt.ReportFailure(src.ID)
		s.recordUsage(p, src, res, latency, uerr.Status, degraded, uerr.Kind)
		if uerr.Retryable {
			return false // caller retries next candidate
		}
		status := uerr.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		p.cc.WriteErr(w, status, uerr.Kind, uerr.Message)
		return true
	}
	s.rt.ReportSuccess(src.ID, p.session)
	if res.Model == "" {
		res.Model = src.Models[p.req.Model]
	}
	if err := p.cc.WriteResp(w, res); err != nil {
		log.Printf("server: write response: %v", err)
	}
	s.recordUsage(p, src, res, latency, http.StatusOK, degraded, "")
	return true
}

// serveStream performs one streaming attempt. Returns true when the response
// is finished (stream written or terminal error sent); false means the
// attempt failed before any payload byte and the caller may retry.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, uc UpstreamCodec, src *router.Source, p serveParams, degraded bool) bool {
	enc := p.cc.NewEnc(w)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	acc := &streamAccountant{alias: p.req.Model, inText: p.req.FirstUserText()}
	ttft := time.Duration(0)
	first := true
	sink := func(ev ir.StreamEvent) {
		if first {
			ttft = time.Since(p.start)
			first = false
		}
		acc.feed(ev)
		if err := enc.Emit(ev); err != nil {
			log.Printf("server: emit: %v", err)
		}
		flush()
	}

	uerr := s.execUpstreamStream(r.Context(), uc, src, p.req, sink)
	if uerr != nil {
		// Not a single event made it out: safe to retry or answer with error.
		s.rt.ReportFailure(src.ID)
		s.recordUsage(p, src, acc.response(), time.Since(p.start), uerr.Status, degraded, uerr.Kind)
		if uerr.Retryable {
			return false
		}
		status := uerr.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		p.cc.WriteErr(w, status, uerr.Kind, uerr.Message)
		return true
	}
	s.rt.ReportSuccess(src.ID, p.session)
	if err := enc.Close(acc.finish); err != nil {
		log.Printf("server: close stream: %v", err)
	}
	flush()
	latency := time.Since(p.start)
	s.recordUsageStream(p, src, acc, latency, ttft, degraded)
	return true
}

// execUpstream performs one complete non-streaming upstream call.
func (s *Server) execUpstream(ctx context.Context, uc UpstreamCodec, src *router.Source, req ir.Request) (ir.Response, *ir.UpstreamError, time.Duration) {
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	hreq, err := uc.Build(cctx, src.BaseURL, src.APIKey, src.Models[req.Model], req, false)
	if err != nil {
		return ir.Response{}, &ir.UpstreamError{Kind: ir.ErrUpstream, Message: err.Error()}, 0
	}
	resp, err := s.httpc.Do(hreq)
	if err != nil {
		return ir.Response{}, &ir.UpstreamError{Kind: ir.ErrNetwork, Message: err.Error(), Retryable: true}, time.Since(start)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return ir.Response{}, &ir.UpstreamError{Kind: ir.ErrNetwork, Message: err.Error(), Retryable: true}, time.Since(start)
	}
	latency := time.Since(start)
	res, perr := uc.ParseBody(body)
	if perr != nil {
		var ue *ir.UpstreamError
		if errors.As(perr, &ue) {
			if resp.StatusCode >= 400 {
				ue.Status = resp.StatusCode
			}
			ue.Retryable = ue.Retryable || retryableStatus(resp.StatusCode)
			return ir.Response{}, ue, latency
		}
		return ir.Response{}, &ir.UpstreamError{Kind: ir.ErrUpstream, Status: resp.StatusCode, Message: perr.Error(), Retryable: retryableStatus(resp.StatusCode)}, latency
	}
	if resp.StatusCode >= 400 {
		// 2xx-ish body that still failed to yield a response
		return ir.Response{}, &ir.UpstreamError{Kind: kindForStatus(resp.StatusCode), Status: resp.StatusCode,
			Message: "upstream returned " + resp.Status, Retryable: retryableStatus(resp.StatusCode)}, latency
	}
	return res, nil, latency
}

// execUpstreamStream performs one streaming upstream call, forwarding events
// to sink. It returns a non-nil *ir.UpstreamError only when nothing was
// emitted (safe to retry); errors after the first event are delivered to the
// sink as EvError and nil is returned.
func (s *Server) execUpstreamStream(ctx context.Context, uc UpstreamCodec, src *router.Source, req ir.Request, sink func(ir.StreamEvent)) *ir.UpstreamError {
	hreq, err := uc.Build(ctx, src.BaseURL, src.APIKey, src.Models[req.Model], req, true)
	if err != nil {
		return &ir.UpstreamError{Kind: ir.ErrUpstream, Message: err.Error()}
	}
	resp, err := s.httpc.Do(hreq)
	if err != nil {
		return &ir.UpstreamError{Kind: ir.ErrNetwork, Message: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_, perr := uc.ParseBody(body)
		var ue *ir.UpstreamError
		if errors.As(perr, &ue) {
			ue.Status = resp.StatusCode
			ue.Retryable = ue.Retryable || retryableStatus(resp.StatusCode)
			return ue
		}
		return &ir.UpstreamError{Kind: kindForStatus(resp.StatusCode), Status: resp.StatusCode,
			Message: "upstream returned " + resp.Status, Retryable: retryableStatus(resp.StatusCode)}
	}

	parser := uc.NewParser()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		evs, lerr := parser.Line(sc.Text())
		if lerr != nil {
			return streamAbort(sink, &ir.UpstreamError{Kind: ir.ErrUpstream, Message: lerr.Error()})
		}
		for _, ev := range evs {
			if ev.Kind == ir.EvError {
				sink(ev)
				return nil
			}
			sink(ev)
		}
	}
	if err := sc.Err(); err != nil {
		return streamAbort(sink, &ir.UpstreamError{Kind: ir.ErrNetwork, Message: err.Error(), Retryable: true})
	}
	return nil
}

// streamAbort reports an upstream failure: before first output it is
// retryable (returned), after output it is surfaced to the client via EvError.
func streamAbort(sink func(ir.StreamEvent), ue *ir.UpstreamError) *ir.UpstreamError {
	sink(ir.StreamEvent{Kind: ir.EvError, ErrCode: ue.Kind, ErrMsg: ue.Message,
		Retryable: ue.Retryable, HTTPStatus: ue.Status})
	return nil
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func kindForStatus(code int) string {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ir.ErrInvalidKey
	case code == http.StatusTooManyRequests:
		return ir.ErrRateLimited
	case code == http.StatusNotFound:
		return ir.ErrNotFound
	default:
		return ir.ErrUpstream
	}
}
