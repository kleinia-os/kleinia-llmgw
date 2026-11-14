package store

import (
	"database/sql"
	"fmt"
	"math"
	"time"
)

// UsageRow is one metered gateway request, exactly as the server reports
// it. TS is RFC3339Nano UTC; In/Out follow ir.Usage semantics (cache and
// reasoning tokens are informational carve-outs, not additive).
type UsageRow struct {
	TS            string
	ReqID         string // correlates audit log / tracepoints / cnode segments
	KeyID         int64  // 0 for AID-only (agentfs) identities
	KeyName       string
	AID           string // kernel agent identity, "" for plain key calls
	SourceID      int64
	SourceName    string
	ClientFormat  string // "chat" | "responses" | "anthropic"
	ModelAlias    string
	UpstreamModel string

	In         int64
	Out        int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64

	Cost      *float64 // nil = price unknown, stored as NULL
	LatencyMS int64
	TTFTMS    int64
	Stream    bool
	Status    int // HTTP status, or 0 for transport-level failures
	Degraded  bool

	Session   string
	ErrorKind string
}

// InsertUsage appends one usage row. A nil Cost stores NULL.
func (s *Store) InsertUsage(u UsageRow) error {
	_, err := s.db.Exec(`
		INSERT INTO usage (
			ts, req_id, key_id, key_name, aid, source_id, source_name, client_format,
			model_alias, upstream_model,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
			cost_est, latency_ms, ttft_ms, stream, status, degraded, session, error_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.TS, u.ReqID, u.KeyID, u.KeyName, u.AID, u.SourceID, u.SourceName, u.ClientFormat,
		u.ModelAlias, u.UpstreamModel,
		u.In, u.Out, u.CacheRead, u.CacheWrite, u.Reasoning,
		u.Cost, u.LatencyMS, u.TTFTMS, u.Stream, u.Status, u.Degraded, u.Session, u.ErrorKind)
	if err != nil {
		return fmt.Errorf("store: insert usage: %w", err)
	}
	return nil
}

// AidWindows is KeyWindows for an AID-only identity (agentfs caller with no
// bound key row): its live quota windows derived from usage rows.
func (s *Store) AidWindows(aid string) (rpm, tpm, dailyTokens int64, err error) {
	now := time.Now().UTC()
	minuteAgo := now.Add(-time.Minute).Format(time.RFC3339)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	row := s.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM usage
				WHERE aid = ? AND datetime(ts) >= datetime(?)),
			(SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM usage
				WHERE aid = ? AND datetime(ts) >= datetime(?)),
			(SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM usage
				WHERE aid = ? AND datetime(ts) >= datetime(?))`,
		aid, minuteAgo, aid, minuteAgo, aid, dayStart)
	if err := row.Scan(&rpm, &tpm, &dailyTokens); err != nil {
		return 0, 0, 0, fmt.Errorf("store: aid windows %s: %w", aid, err)
	}
	return rpm, tpm, dailyTokens, nil
}

// KeyWindows returns a key's live quota windows in one round trip:
// rpm is the number of its requests in the last 60 seconds, tpm the
// input+output tokens in the last 60 seconds, and dailyTokens the
// input+output tokens since the current UTC day began.
//
// Window bounds are computed with SQLite datetime() over the RFC3339Nano
// ts column (second granularity), so rows remain greppable text.
func (s *Store) KeyWindows(keyID int64) (rpm, tpm, dailyTokens int64, err error) {
	now := time.Now().UTC()
	minuteAgo := now.Add(-time.Minute).Format(time.RFC3339)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	row := s.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM usage
				WHERE key_id = ? AND datetime(ts) >= datetime(?)),
			(SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM usage
				WHERE key_id = ? AND datetime(ts) >= datetime(?)),
			(SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM usage
				WHERE key_id = ? AND datetime(ts) >= datetime(?))`,
		keyID, minuteAgo, keyID, minuteAgo, keyID, dayStart)
	if err := row.Scan(&rpm, &tpm, &dailyTokens); err != nil {
		return 0, 0, 0, fmt.Errorf("store: key windows %d: %w", keyID, err)
	}
	return rpm, tpm, dailyTokens, nil
}

// Agg is one usage aggregate (per key, model or source). CostEst is nil
// when no request in the group had a known price.
type Agg struct {
	Label     string
	Requests  int64
	InTokens  int64
	OutTokens int64
	CostEst   *float64
}

// SummarySince aggregates usage rows with datetime(ts) >= datetime(since)
// by key name, model alias, source name and agent identity (rows lacking a
// value group under ""), most requests first.
func (s *Store) SummarySince(since string) (byKey, byModel, bySource, byAid []Agg, err error) {
	byKey, err = s.aggregateSince(since, "key_name")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	byModel, err = s.aggregateSince(since, "model_alias")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	bySource, err = s.aggregateSince(since, "source_name")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	byAid, err = s.aggregateSince(since, "aid")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return byKey, byModel, bySource, byAid, nil
}

func (s *Store) aggregateSince(since, group string) ([]Agg, error) {
	// group is one of three fixed column names, not user input.
	rows, err := s.db.Query(`
		SELECT COALESCE(`+group+`, ''), COUNT(*),
			COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			SUM(cost_est)
		FROM usage WHERE datetime(ts) >= datetime(?)
		GROUP BY `+group+` ORDER BY COUNT(*) DESC, 1 ASC`, since)
	if err != nil {
		return nil, fmt.Errorf("store: summary by %s: %w", group, err)
	}
	defer rows.Close()
	var out []Agg
	for rows.Next() {
		var a Agg
		var cost sql.NullFloat64
		if err := rows.Scan(&a.Label, &a.Requests, &a.InTokens, &a.OutTokens, &cost); err != nil {
			return nil, fmt.Errorf("store: summary by %s: %w", group, err)
		}
		if cost.Valid {
			c := cost.Float64
			a.CostEst = &c
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// usageSelect is the shared row projection for RecentUsage and UsagePage.
const usageSelect = `
		SELECT ts, COALESCE(req_id, ''), COALESCE(key_id, 0), COALESCE(key_name, ''),
			COALESCE(aid, ''), COALESCE(source_id, 0),
			COALESCE(source_name, ''), COALESCE(client_format, ''),
			COALESCE(model_alias, ''), COALESCE(upstream_model, ''),
			input_tokens, output_tokens, COALESCE(cache_read_tokens, 0), COALESCE(cache_write_tokens, 0),
			COALESCE(reasoning_tokens, 0), cost_est, COALESCE(latency_ms, 0), COALESCE(ttft_ms, 0),
			COALESCE(stream, 0), COALESCE(status, 0), COALESCE(degraded, 0),
			COALESCE(session, ''), COALESCE(error_kind, '')`

func scanUsageRows(rows *sql.Rows) ([]UsageRow, error) {
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		var cost sql.NullFloat64
		if err := rows.Scan(&u.TS, &u.ReqID, &u.KeyID, &u.KeyName, &u.AID, &u.SourceID, &u.SourceName,
			&u.ClientFormat, &u.ModelAlias, &u.UpstreamModel,
			&u.In, &u.Out, &u.CacheRead, &u.CacheWrite, &u.Reasoning,
			&cost, &u.LatencyMS, &u.TTFTMS, &u.Stream, &u.Status, &u.Degraded,
			&u.Session, &u.ErrorKind); err != nil {
			return nil, fmt.Errorf("store: recent usage: %w", err)
		}
		if cost.Valid {
			c := cost.Float64
			u.Cost = &c
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecentUsage returns the most recent usage rows, newest first. limit <= 0
// returns everything.
func (s *Store) RecentUsage(limit int) ([]UsageRow, error) {
	query := usageSelect + ` FROM usage ORDER BY id DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: recent usage: %w", err)
	}
	return scanUsageRows(rows)
}

// UsageFilter narrows a UsagePage query. Empty/nil fields match everything.
// Search is a substring across ident/aid/model/source/req id/error kind.
type UsageFilter struct {
	Since    string // RFC3339 lower bound (required)
	Ident    string // exact key_name match
	Model    string // exact model_alias match
	Source   string // exact source_name match
	Search   string
	HasError *bool // nil = all; true = only failed rows; false = only OK
}

func (f UsageFilter) where() (string, []any) {
	where, args := "datetime(ts) >= datetime(?)", []any{f.Since}
	if f.Ident != "" {
		where += " AND key_name = ?"
		args = append(args, f.Ident)
	}
	if f.Model != "" {
		where += " AND model_alias = ?"
		args = append(args, f.Model)
	}
	if f.Source != "" {
		where += " AND source_name = ?"
		args = append(args, f.Source)
	}
	if f.Search != "" {
		like := "%" + f.Search + "%"
		where += ` AND (COALESCE(key_name,'') LIKE ? OR COALESCE(aid,'') LIKE ?
			OR COALESCE(model_alias,'') LIKE ? OR COALESCE(source_name,'') LIKE ?
			OR COALESCE(req_id,'') LIKE ? OR COALESCE(error_kind,'') LIKE ?)`
		args = append(args, like, like, like, like, like, like)
	}
	if f.HasError != nil {
		failed := "(status >= 400 OR COALESCE(error_kind,'') != '')"
		if *f.HasError {
			where += " AND " + failed
		} else {
			where += " AND NOT " + failed
		}
	}
	return where, args
}

// UsagePage returns one page of usage rows (newest first) plus the total
// number of rows matching the filter — the paginated call-log view.
func (s *Store) UsagePage(f UsageFilter, offset, limit int) ([]UsageRow, int, error) {
	where, args := f.where()
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage WHERE `+where,
		args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: usage page count: %w", err)
	}
	query := usageSelect + ` FROM usage WHERE ` + where +
		` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: usage page: %w", err)
	}
	out, err := scanUsageRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// HistBucket is one cell of a time-bucketed usage histogram: either a totals
// bucket (Group "") or a dimension slice within the bucket.
type HistBucket struct {
	Label    string   `json:"label"` // bucket start, RFC3339 ("2026-10-28T10:00:00Z" hourly / date daily)
	Group    string   `json:"group"` // dimension value ("" for dimension "")
	Requests int64    `json:"requests"`
	In       int64    `json:"in"`
	Out      int64    `json:"out"`
	Cost     *float64 `json:"cost"`
}

var histFormats = map[string]string{
	"hour": `%Y-%m-%dT%H:00:00Z`,
	"day":  `%Y-%m-%dT00:00:00Z`,
}

var histDims = map[string]string{
	"":            "",
	"key_name":    "key_name",
	"model_alias": "model_alias",
	"source_name": "source_name",
	"aid":         "aid",
}

// Histogram aggregates usage into time buckets since the given RFC3339
// instant, optionally split by dimension ("key_name", "model_alias",
// "source_name", "aid" or "" for totals). bucket is "hour" or "day".
// The caller derives any metric (requests / tokens / cost) from the fields.
func (s *Store) Histogram(since, bucket, dimension string) ([]HistBucket, error) {
	bfmt, ok := histFormats[bucket]
	if !ok {
		return nil, fmt.Errorf("store: histogram: bucket must be hour|day, got %q", bucket)
	}
	dim, ok := histDims[dimension]
	if !ok {
		return nil, fmt.Errorf("store: histogram: unknown dimension %q", dimension)
	}
	sel, group := `strftime('`+bfmt+`', ts)`, `1`
	if dim != "" {
		sel += `, COALESCE(` + dim + `, '')`
		group += `, 2`
	}
	query := `SELECT ` + sel + `, COUNT(*),
			COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), SUM(cost_est)
		FROM usage WHERE datetime(ts) >= datetime(?)
		GROUP BY ` + group + ` ORDER BY ` + group
	rows, err := s.db.Query(query, since)
	if err != nil {
		return nil, fmt.Errorf("store: histogram: %w", err)
	}
	defer rows.Close()
	var out []HistBucket
	for rows.Next() {
		var b HistBucket
		var cost sql.NullFloat64
		var vals []any
		if dim != "" {
			vals = []any{&b.Label, &b.Group, &b.Requests, &b.In, &b.Out, &cost}
		} else {
			vals = []any{&b.Label, &b.Requests, &b.In, &b.Out, &cost}
		}
		if err := rows.Scan(vals...); err != nil {
			return nil, fmt.Errorf("store: histogram: %w", err)
		}
		if cost.Valid {
			c := cost.Float64
			b.Cost = &c
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Stats is the headline metrics block over a window.
type Stats struct {
	Requests     int64    `json:"requests"`
	In           int64    `json:"in"`
	Out          int64    `json:"out"`
	CacheRead    int64    `json:"cache_read"`
	CacheWrite   int64    `json:"cache_write"`
	Reasoning    int64    `json:"reasoning"`
	Cost         *float64 `json:"cost"`
	LatencyP50MS int64    `json:"latency_p50_ms"` // 0 when no samples
	LatencyP95MS int64    `json:"latency_p95_ms"`
	TTFTP50MS    int64    `json:"ttft_p50_ms"`
	ErrorRate    float64  `json:"error_rate"` // 0..1
	ActiveIds    int64    `json:"active_ids"`
	Streams      int64    `json:"streams"`
}

// Stats computes window metrics: totals, cache/reading carve-outs, latency
// percentiles (nearest-rank over up to 50k samples), TTFT median, error rate
// and the number of distinct identities (AID or key) that called.
func (s *Store) Stats(since string) (Stats, error) {
	var st Stats
	var cost sql.NullFloat64
	err := s.db.QueryRow(`
		SELECT COUNT(*),
			COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0), SUM(cost_est),
			COALESCE(SUM(CASE WHEN status >= 400 OR COALESCE(error_kind, '') != '' THEN 1 ELSE 0 END), 0),
			COUNT(DISTINCT COALESCE(NULLIF(aid, ''), 'key:' || COALESCE(key_name, ''))),
			COALESCE(SUM(CASE WHEN stream != 0 THEN 1 ELSE 0 END), 0)
		FROM usage WHERE datetime(ts) >= datetime(?)`, since).
		Scan(&st.Requests, &st.In, &st.Out, &st.CacheRead, &st.CacheWrite,
			&st.Reasoning, &cost, &st.ErrorRate, &st.ActiveIds, &st.Streams)
	if err != nil {
		return Stats{}, fmt.Errorf("store: stats: %w", err)
	}
	if cost.Valid {
		c := cost.Float64
		st.Cost = &c
	}
	if st.Requests > 0 {
		st.ErrorRate = st.ErrorRate / float64(st.Requests)
	}
	st.LatencyP50MS, st.LatencyP95MS, err = s.percentilesSince(since, "latency_ms")
	if err != nil {
		return Stats{}, err
	}
	st.TTFTP50MS, _, err = s.percentilesSince(since, "ttft_ms")
	if err != nil {
		return Stats{}, err
	}
	return st, nil
}

func (s *Store) percentilesSince(since, column string) (p50, p95 int64, err error) {
	// column is one of two fixed names, not user input.
	rows, err := s.db.Query(`SELECT `+column+` FROM usage
		WHERE `+column+` > 0 AND datetime(ts) >= datetime(?)
		ORDER BY `+column+` LIMIT 50000`, since)
	if err != nil {
		return 0, 0, fmt.Errorf("store: percentiles(%s): %w", column, err)
	}
	defer rows.Close()
	var vals []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return 0, 0, fmt.Errorf("store: percentiles(%s): %w", column, err)
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return 0, 0, rows.Err()
	}
	// nearest-rank percentiles: rank = ceil(q·n), 1-based.
	pick := func(q float64) int64 {
		rank := int(math.Ceil(q * float64(len(vals))))
		if rank < 1 {
			rank = 1
		}
		if rank > len(vals) {
			rank = len(vals)
		}
		return vals[rank-1]
	}
	return pick(0.50), pick(0.95), rows.Err()
}
