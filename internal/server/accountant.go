package server

import (
	"log"
	"strconv"
	"time"

	"kleinia.io/llmgw/internal/ir"
	"kleinia.io/llmgw/internal/router"
	"kleinia.io/llmgw/internal/store"
)

// streamAccountant accumulates what a stream actually delivered so usage can
// be recorded even when the upstream never sends an EvUsage event.
type streamAccountant struct {
	alias  string
	inText string

	usage    ir.Usage
	finish   ir.Finish
	text     []byte // concatenated visible output, for fallback estimation
	sawUsage bool
}

func (a *streamAccountant) feed(ev ir.StreamEvent) {
	switch ev.Kind {
	case ir.EvTextDelta:
		a.text = append(a.text, ev.Text...)
	case ir.EvFinish:
		a.finish = ev.Finish
	case ir.EvUsage:
		// Anthropic message_delta carries the cumulative final usage —
		// last event wins; OpenAI emits usage exactly once.
		a.usage = ev.Usage
		a.sawUsage = true
	}
}

// response builds a best-effort ir.Response from the accountant state.
func (a *streamAccountant) response() ir.Response {
	return ir.Response{Model: a.alias, FinishReason: a.finish, Usage: a.usage}
}

// finalUsage returns upstream-reported usage when present, else a clearly
// marked local estimate (never silently zero).
func (a *streamAccountant) finalUsage() ir.Usage {
	if a.sawUsage && (a.usage.InputTokens > 0 || a.usage.OutputTokens > 0) {
		return a.usage
	}
	return ir.Usage{
		InputTokens:  int64(estimateTokens(a.inText)),
		OutputTokens: int64(estimateTokens(string(a.text))),
		Estimated:    true,
	}
}

// recordUsage writes one usage row for a non-streaming call.
func (s *Server) recordUsage(p serveParams, src *router.Source, res ir.Response, latency time.Duration, status int, degraded bool, errKind string) {
	u := res.Usage
	if !p.req.Stream && u.InputTokens == 0 && u.OutputTokens == 0 {
		u = ir.Usage{
			InputTokens:  int64(estimateTokens(p.req.FirstUserText())),
			OutputTokens: int64(estimateTokens(partsText(res.Content))),
			Estimated:    true,
		}
	}
	s.insertUsage(store.UsageRow{
		ReqID: p.reqID, KeyID: p.key.ID, KeyName: p.key.Name, AID: p.key.AID,
		SourceID: src.ID, SourceName: src.Name,
		ClientFormat: p.format, ModelAlias: p.req.Model, UpstreamModel: res.Model,
		In: u.InputTokens, Out: u.OutputTokens,
		CacheRead: u.CacheReadTokens, CacheWrite: u.CacheWriteTokens, Reasoning: u.ReasoningTokens,
		Cost:      s.price.Cost(p.req.Model, u),
		LatencyMS: millis(latency), Stream: false, Status: status,
		Degraded: degraded, Session: p.session, ErrorKind: errKind,
	})
}

func (s *Server) recordUsageStream(p serveParams, src *router.Source, acc *streamAccountant, latency, ttft time.Duration, degraded bool) {
	u := acc.finalUsage()
	s.insertUsage(store.UsageRow{
		ReqID: p.reqID, KeyID: p.key.ID, KeyName: p.key.Name, AID: p.key.AID,
		SourceID: src.ID, SourceName: src.Name,
		ClientFormat: p.format, ModelAlias: p.req.Model, UpstreamModel: src.Models[p.req.Model],
		In: u.InputTokens, Out: u.OutputTokens,
		CacheRead: u.CacheReadTokens, CacheWrite: u.CacheWriteTokens, Reasoning: u.ReasoningTokens,
		Cost:      s.price.Cost(p.req.Model, u),
		LatencyMS: millis(latency), TTFTMS: millis(ttft), Stream: true, Status: 200,
		Degraded: degraded, Session: p.session,
	})
}

func (s *Server) insertUsage(row store.UsageRow) {
	row.TS = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.st.InsertUsage(row); err != nil {
		log.Printf("server: insert usage: %v", err)
	}
	auditLog(row)
}

// auditLog emits one journald-friendly structured line per request. Under
// systemd this lands in the journal with KLEINIA_LLM_REQ greppable; in dev
// it is plain stdout. ReqID is the correlation key shared with tracepoints
// and (G1+) cnode audit segments.
func auditLog(r store.UsageRow) {
	cost := "-"
	if r.Cost != nil {
		cost = strconv.FormatFloat(*r.Cost, 'f', -1, 64)
	}
	log.Printf("KLEINIA_LLM_REQ=1 req_id=%s ident=%s aid=%s source=%s model=%s via=%s "+
		"in=%d out=%d cache_read=%d cache_write=%d reasoning=%d cost=%s "+
		"latency_ms=%d ttft_ms=%d stream=%v status=%d degraded=%v session=%s err=%s",
		r.ReqID, r.KeyName, r.AID, r.SourceName, r.ModelAlias, r.ClientFormat,
		r.In, r.Out, r.CacheRead, r.CacheWrite, r.Reasoning, cost,
		r.LatencyMS, r.TTFTMS, r.Stream, r.Status, r.Degraded, r.Session, r.ErrorKind)
}

func millis(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// partsText concatenates text/thinking parts (estimation helper).
func partsText(parts []ir.Part) string {
	var b []byte
	for _, p := range parts {
		if p.Type == ir.PartText || p.Type == ir.PartThinking {
			b = append(b, p.Text...)
		}
	}
	return string(b)
}
