package store

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Request is one row of the requests table (design 11.2). Nullable columns
// are pointers; nil is NULL.
type Request struct {
	ID                 int64     `json:"id"` // assigned by the database; ignored by Record
	TS                 time.Time `json:"ts"` // request start, stored as unix milliseconds
	RequestID          string    `json:"request_id"`
	UpstreamRequestID  *string   `json:"upstream_request_id"`
	Protocol           string    `json:"protocol"`
	Operation          string    `json:"operation"`
	ClientModel        string    `json:"client_model"`
	Route              *string   `json:"route"`
	Provider           *string   `json:"provider"` // of the committed response, or of the last attempt
	UpstreamModel      *string   `json:"upstream_model"`
	Status             int       `json:"status"`
	Outcome            string    `json:"outcome"`
	Stream             bool      `json:"stream"`
	Attempts           int       `json:"attempts"`
	AttemptsMS         int64     `json:"attempts_ms"`
	TTFBMS             *int64    `json:"ttfb_ms"` // nil when no upstream headers were committed
	TotalMS            int64     `json:"total_ms"`
	RoutingJSON        string    `json:"routing_json"` // JSON object text
	AttemptLog         string    `json:"attempt_log"`  // JSON array text: [{"provider": ..., ...}, ...]
	AffinityKey        *string   `json:"affinity_key"`
	InputTokens        *int64    `json:"input_tokens"`
	CacheReadTokens    *int64    `json:"cache_read_tokens"`
	CacheWriteTokens   *int64    `json:"cache_write_tokens"`
	CacheWrite1hTokens *int64    `json:"cache_write_1h_tokens"`
	OutputTokens       *int64    `json:"output_tokens"`
	ReasoningTokens    *int64    `json:"reasoning_tokens"`
	InputImageTokens   *int64    `json:"input_image_tokens"`  // subset of input_tokens (images)
	OutputImageTokens  *int64    `json:"output_image_tokens"` // subset of output_tokens (images)
	UsageJSON          *string   `json:"usage_json"`
	CostUSD            *float64  `json:"cost_usd"`
	PriceKey           *string   `json:"price_key"`
	PriceJSON          *string   `json:"price_json"`
	Error              *string   `json:"error"`
}

const requestColumns = `id, ts, request_id, upstream_request_id, protocol, operation, client_model, route,
	provider, upstream_model, status, outcome, stream, attempts, attempts_ms, ttfb_ms,
	total_ms, routing_json, attempt_log, affinity_key, input_tokens, cache_read_tokens,
	cache_write_tokens, cache_write_1h_tokens, output_tokens, reasoning_tokens,
	input_image_tokens, output_image_tokens, usage_json, cost_usd, price_key, price_json, error`

func scanRequest(row interface{ Scan(...any) error }) (*Request, error) {
	var r Request
	var ts int64
	err := row.Scan(
		&r.ID, &ts, &r.RequestID, &r.UpstreamRequestID, &r.Protocol, &r.Operation, &r.ClientModel, &r.Route,
		&r.Provider, &r.UpstreamModel, &r.Status, &r.Outcome, &r.Stream, &r.Attempts, &r.AttemptsMS, &r.TTFBMS,
		&r.TotalMS, &r.RoutingJSON, &r.AttemptLog, &r.AffinityKey, &r.InputTokens, &r.CacheReadTokens,
		&r.CacheWriteTokens, &r.CacheWrite1hTokens, &r.OutputTokens, &r.ReasoningTokens,
		&r.InputImageTokens, &r.OutputImageTokens,
		&r.UsageJSON, &r.CostUSD, &r.PriceKey, &r.PriceJSON, &r.Error,
	)
	if err != nil {
		return nil, err
	}
	r.TS = time.UnixMilli(ts)
	return &r, nil
}

// where accumulates SQL conditions and their parameters.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *where) String() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// errorCond is what counts as an error row everywhere: a non-success status
// or any outcome other than completed.
const errorCond = `(status >= 400 OR outcome != 'completed')`

// RequestFilter selects rows for the Requests view and `verbatim log`.
// Zero values mean "no constraint".
type RequestFilter struct {
	Since, Until  time.Time // Since inclusive, Until exclusive
	Protocol      string
	Provider      string
	Route         string
	Outcome       string
	StatusClass   int    // 2, 3, 4, 5: status/100 must equal it
	ModelContains string // case-insensitive substring of client_model
	ErrorsOnly    bool   // status >= 400 or outcome != completed
	BeforeID      int64  // page backwards: rows with id < BeforeID
	AfterID       int64  // live tail: rows with id > AfterID, returned oldest first
	Limit         int    // default 100, capped at 1000
}

// Requests returns matching rows newest first, or oldest first when AfterID
// is set so that a live tail can append them in order.
func (s *Store) Requests(f RequestFilter) ([]Request, error) {
	var w where
	if !f.Since.IsZero() {
		w.add(`ts >= ?`, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		w.add(`ts < ?`, f.Until.UnixMilli())
	}
	if f.Protocol != "" {
		w.add(`protocol = ?`, f.Protocol)
	}
	if f.Provider != "" {
		w.add(`provider = ?`, f.Provider)
	}
	if f.Route != "" {
		w.add(`route = ?`, f.Route)
	}
	if f.Outcome != "" {
		w.add(`outcome = ?`, f.Outcome)
	}
	if f.StatusClass != 0 {
		w.add(`status / 100 = ?`, f.StatusClass)
	}
	if f.ModelContains != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.ModelContains)
		w.add(`client_model LIKE ? ESCAPE '\'`, "%"+esc+"%")
	}
	if f.ErrorsOnly {
		w.add(errorCond)
	}
	if f.BeforeID > 0 {
		w.add(`id < ?`, f.BeforeID)
	}
	order := ` ORDER BY id DESC`
	if f.AfterID > 0 {
		w.add(`id > ?`, f.AfterID)
		order = ` ORDER BY id ASC`
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)

	rows, err := s.r.Query(`SELECT `+requestColumns+` FROM requests`+w.String()+order+` LIMIT ?`, append(w.args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("query requests: %w", err)
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("query requests: %w", err)
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query requests: %w", err)
	}
	return out, nil
}

// Request returns the row with the given id, or ErrNotFound.
func (s *Store) Request(id int64) (*Request, error) {
	r, err := scanRequest(s.r.QueryRow(`SELECT `+requestColumns+` FROM requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("request %d: %w", id, err)
	}
	return r, nil
}

// RequestByRequestID returns the row with the given x-verbatim-request-id,
// or ErrNotFound.
func (s *Store) RequestByRequestID(rid string) (*Request, error) {
	r, err := scanRequest(s.r.QueryRow(`SELECT `+requestColumns+` FROM requests WHERE request_id = ?`, rid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("request %q: %w", rid, err)
	}
	return r, nil
}

// UsageFilter selects rows for usage aggregates. Rows of the auxiliary
// operations, count_tokens and search, are never aggregated. Zero values mean
// "no constraint".
type UsageFilter struct {
	Since, Until time.Time // Since inclusive, Until exclusive
	Protocol     string
	Provider     string
	Route        string
	Model        string // exact client_model
}

func (f UsageFilter) where() *where {
	w := &where{}
	w.add(`operation NOT IN ('count_tokens', 'search')`)
	if !f.Since.IsZero() {
		w.add(`ts >= ?`, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		w.add(`ts < ?`, f.Until.UnixMilli())
	}
	if f.Protocol != "" {
		w.add(`protocol = ?`, f.Protocol)
	}
	if f.Provider != "" {
		w.add(`provider = ?`, f.Provider)
	}
	if f.Route != "" {
		w.add(`route = ?`, f.Route)
	}
	if f.Model != "" {
		w.add(`client_model = ?`, f.Model)
	}
	return w
}

// Aggregate is the sum over a group of requests. NULL token counts and costs
// contribute 0; Unpriced and NoUsage say how many rows that hides.
type Aggregate struct {
	Key          string  `json:"key"` // model, provider, route, or day (YYYY-MM-DD, UTC); empty for the total
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"` // status >= 400 or outcome != completed
	Input        int64   `json:"input_tokens"`
	CacheRead    int64   `json:"cache_read_tokens"`
	CacheWrite   int64   `json:"cache_write_tokens"`
	CacheWrite1h int64   `json:"cache_write_1h_tokens"`
	Output       int64   `json:"output_tokens"`
	Reasoning    int64   `json:"reasoning_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Unpriced     int64   `json:"unpriced"` // usage present but cost_usd NULL
	NoUsage      int64   `json:"no_usage"` // usage_json NULL
}

const aggregateColumns = `COUNT(*),
	COALESCE(SUM(` + errorCond + `), 0),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(cache_write_tokens), 0),
	COALESCE(SUM(cache_write_1h_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(reasoning_tokens), 0),
	COALESCE(SUM(cost_usd), 0),
	COALESCE(SUM(cost_usd IS NULL AND usage_json IS NOT NULL), 0),
	COALESCE(SUM(usage_json IS NULL), 0)`

func (a *Aggregate) fields() []any {
	return []any{&a.Requests, &a.Errors, &a.Input, &a.CacheRead, &a.CacheWrite, &a.CacheWrite1h,
		&a.Output, &a.Reasoning, &a.CostUSD, &a.Unpriced, &a.NoUsage}
}

// usageGroups maps a grouping name to its SQL key expression and its ORDER BY over
// the select list (1 is the key, 2 the request count, 10 the cost).
// Days are listed chronologically; the other groupings most expensive first.
var usageGroups = map[string]struct{ key, order string }{
	"model":    {`client_model`, `10 DESC, 2 DESC, 1`},
	"provider": {`COALESCE(provider, '')`, `10 DESC, 2 DESC, 1`},
	"route":    {`COALESCE(route, '')`, `10 DESC, 2 DESC, 1`},
	"day":      {`strftime('%Y-%m-%d', ts / 1000, 'unixepoch')`, `1`},
}

// Usage groups matching rows by "model", "provider", "route", or "day" and
// also returns the overall total. The total is summed from the groups, so
// both come from the same snapshot.
func (s *Store) Usage(f UsageFilter, by string) (rows []Aggregate, total Aggregate, err error) {
	g, ok := usageGroups[by]
	if !ok {
		return nil, Aggregate{}, fmt.Errorf("usage: unknown grouping %q (want model, provider, route, or day)", by)
	}
	w := f.where()
	q, err := s.r.Query(`SELECT `+g.key+`, `+aggregateColumns+` FROM requests`+w.String()+` GROUP BY 1 ORDER BY `+g.order, w.args...)
	if err != nil {
		return nil, Aggregate{}, fmt.Errorf("usage by %s: %w", by, err)
	}
	defer q.Close()
	for q.Next() {
		var a Aggregate
		if err := q.Scan(append([]any{&a.Key}, a.fields()...)...); err != nil {
			return nil, Aggregate{}, fmt.Errorf("usage by %s: %w", by, err)
		}
		rows = append(rows, a)
		total.Requests += a.Requests
		total.Errors += a.Errors
		total.Input += a.Input
		total.CacheRead += a.CacheRead
		total.CacheWrite += a.CacheWrite
		total.CacheWrite1h += a.CacheWrite1h
		total.Output += a.Output
		total.Reasoning += a.Reasoning
		total.CostUSD += a.CostUSD
		total.Unpriced += a.Unpriced
		total.NoUsage += a.NoUsage
	}
	if err := q.Err(); err != nil {
		return nil, Aggregate{}, fmt.Errorf("usage by %s: %w", by, err)
	}
	return rows, total, nil
}

// ModelCount is a client model name with a number of requests.
type ModelCount struct {
	Model string `json:"model"`
	Count int64  `json:"count"`
}

// UnpricedModels lists the client model names that have requests with usage
// but no cost in the range and that the price table still has no price for,
// most requests first. priced reports whether the table prices a request
// of the protocol logged with these client and upstream model names; rows
// logged before their price was added keep no cost, but their names need
// nothing more.
func (s *Store) UnpricedModels(f UsageFilter, priced func(protocol, clientModel, upstreamModel string) bool) ([]ModelCount, error) {
	w := f.where()
	w.add(`cost_usd IS NULL AND usage_json IS NOT NULL`)
	q, err := s.r.Query(`SELECT protocol, client_model, COALESCE(upstream_model, ''), COUNT(*) FROM requests`+w.String()+
		` GROUP BY 1, 2, 3`, w.args...)
	if err != nil {
		return nil, fmt.Errorf("unpriced models: %w", err)
	}
	defer q.Close()
	counts := map[string]int64{}
	for q.Next() {
		var proto, client, upstream string
		var n int64
		if err := q.Scan(&proto, &client, &upstream, &n); err != nil {
			return nil, fmt.Errorf("unpriced models: %w", err)
		}
		if !priced(proto, client, upstream) {
			counts[client] += n
		}
	}
	if err := q.Err(); err != nil {
		return nil, fmt.Errorf("unpriced models: %w", err)
	}
	out := make([]ModelCount, 0, len(counts))
	for m, n := range counts {
		out = append(out, ModelCount{Model: m, Count: n})
	}
	slices.SortFunc(out, func(a, b ModelCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), strings.Compare(a.Model, b.Model))
	})
	return out, nil
}

// Endpoint is a provider serving one protocol.
type Endpoint struct{ Provider, Protocol string }

// EndpointStats are the per-endpoint counters of the Providers view.
type EndpointStats struct {
	Committed   int64 // rows whose response headers came from this endpoint (ttfb_ms set)
	Failover    int64 // attempts at this endpoint that were followed by another attempt
	ErrorEvents int64 // committed rows with outcome upstream_error_event
}

// EndpointStats counts, per endpoint, the requests since the given time. A
// row's endpoints are its providers under its protocol.
func (s *Store) EndpointStats(since time.Time) (map[Endpoint]EndpointStats, error) {
	out := map[Endpoint]EndpointStats{}
	ms := since.UnixMilli()

	q, err := s.r.Query(`SELECT provider, protocol,
			SUM(ttfb_ms IS NOT NULL),
			SUM(outcome = 'upstream_error_event')
		FROM requests WHERE ts >= ? AND provider IS NOT NULL GROUP BY provider, protocol`, ms)
	if err != nil {
		return nil, fmt.Errorf("endpoint stats: %w", err)
	}
	defer q.Close()
	for q.Next() {
		var e Endpoint
		var st EndpointStats
		if err := q.Scan(&e.Provider, &e.Protocol, &st.Committed, &st.ErrorEvents); err != nil {
			return nil, fmt.Errorf("endpoint stats: %w", err)
		}
		out[e] = st
	}
	if err := q.Err(); err != nil {
		return nil, fmt.Errorf("endpoint stats: %w", err)
	}

	// Every attempt but the last one in attempt_log failed over to the next.
	f, err := s.r.Query(`SELECT json_extract(a.value, '$.provider'), r.protocol, COUNT(*)
		FROM requests r, json_each(r.attempt_log) a
		WHERE r.ts >= ? AND json_valid(r.attempt_log) AND a.key < json_array_length(r.attempt_log) - 1
		GROUP BY 1, 2`, ms)
	if err != nil {
		return nil, fmt.Errorf("endpoint failovers: %w", err)
	}
	defer f.Close()
	for f.Next() {
		var p sql.NullString
		var proto string
		var n int64
		if err := f.Scan(&p, &proto, &n); err != nil {
			return nil, fmt.Errorf("endpoint failovers: %w", err)
		}
		if !p.Valid {
			continue
		}
		e := Endpoint{p.String, proto}
		st := out[e]
		st.Failover = n
		out[e] = st
	}
	if err := f.Err(); err != nil {
		return nil, fmt.Errorf("endpoint failovers: %w", err)
	}
	return out, nil
}
