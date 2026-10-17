package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func openTest(t *testing.T, queue int) *Store {
	t.Helper()
	s, err := open(filepath.Join(t.TempDir(), "sub", "v.db"), 0, queue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// flush waits until the writer has stored n rows.
func flush(t *testing.T, s *Store, n int) {
	t.Helper()
	for range 500 {
		var got int
		if err := s.r.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("writer did not store %d rows", n)
}

var base = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type rowSpec struct {
	at         time.Duration
	proto, op  string
	model      string
	route      string
	provider   string
	status     int
	outcome    string
	input, out int64
	cost       *float64
	noUsage    bool
	noTTFB     bool // upstream headers were never committed
	attempts   string
}

func (r rowSpec) request(i int) *Request {
	q := &Request{
		TS:          base.Add(r.at),
		RequestID:   fmt.Sprintf("req-%d", i),
		Protocol:    r.proto,
		Operation:   r.op,
		ClientModel: r.model,
		Route:       ptr(r.route),
		Provider:    ptr(r.provider),
		Status:      r.status,
		Outcome:     r.outcome,
		Attempts:    1,
		RoutingJSON: `{}`,
		AttemptLog:  r.attempts,
		CostUSD:     r.cost,
	}
	if q.AttemptLog == "" {
		q.AttemptLog = fmt.Sprintf(`[{"provider":%q,"status":%d}]`, r.provider, r.status)
	}
	if !r.noTTFB {
		q.TTFBMS = ptr(int64(50))
	}
	if !r.noUsage {
		q.InputTokens = ptr(r.input)
		q.OutputTokens = ptr(r.out)
		q.UsageJSON = ptr(`{}`)
	}
	return q
}

var fixture = []rowSpec{
	{at: 0, proto: "anthropic", op: "create", model: "kimi-k3", route: "kimi-k3", provider: "moonshot", status: 200, outcome: "completed", input: 100, out: 10, cost: ptr(0.5)},
	{at: time.Hour, proto: "anthropic", op: "count_tokens", model: "kimi-k3", route: "kimi-k3", provider: "moonshot", status: 200, outcome: "completed", input: 999},
	{at: 2 * time.Hour, proto: "chat", op: "create", model: "gpt-5.5", route: "gpt-*", provider: "openai", status: 200, outcome: "upstream_error_event", input: 50, out: 5},
	{at: 25 * time.Hour, proto: "chat", op: "create", model: "gpt-5.5", route: "gpt-*", provider: "openai", status: 429, outcome: "completed", noUsage: true,
		attempts: `[{"provider":"relay","status":503},{"provider":"openai","status":429}]`},
	{at: 26 * time.Hour, proto: "responses", op: "create", model: "my_model%", route: "*", provider: "relay", status: 502, outcome: "gateway_error", noUsage: true, noTTFB: true,
		attempts: `[{"provider":"relay","error":"dial"},{"provider":"openai","error":"dial"}]`},
	{at: 27 * time.Hour, proto: "anthropic", op: "create", model: "kimi-k3", route: "kimi-k3", provider: "moonshot", status: 200, outcome: "completed", input: 10, out: 1, cost: ptr(0.25)},
}

func seed(t *testing.T, s *Store) {
	t.Helper()
	for i, r := range fixture {
		s.Record(r.request(i))
	}
	flush(t, s, len(fixture))
}

func TestRoundTrip(t *testing.T) {
	s := openTest(t, 16)
	in := &Request{
		TS: base, RequestID: "abc", UpstreamRequestID: ptr("up-1"),
		Protocol: "anthropic", Operation: "create", ClientModel: "m",
		Route: ptr("m"), Provider: ptr("p"), UpstreamModel: ptr("um"),
		Status: 200, Outcome: "completed", Stream: true, Attempts: 2,
		AttemptsMS: 30, TTFBMS: ptr(int64(40)), TotalMS: 90,
		RoutingJSON: `{"route":"m"}`, AttemptLog: `[]`, AffinityKey: ptr("k"),
		InputTokens: ptr(int64(1)), CacheReadTokens: ptr(int64(2)), CacheWriteTokens: ptr(int64(3)),
		CacheWrite1hTokens: ptr(int64(1)), OutputTokens: ptr(int64(4)), ReasoningTokens: ptr(int64(2)),
		UsageJSON: ptr(`{"x":1}`), CostUSD: ptr(1.5), PriceKey: ptr("m"), PriceJSON: ptr(`{}`), Error: nil,
	}
	s.Record(in)
	flush(t, s, 1)

	got, err := s.RequestByRequestID("abc")
	if err != nil {
		t.Fatal(err)
	}
	in.ID = got.ID
	if fmt.Sprintf("%+v", deref(*got)) != fmt.Sprintf("%+v", deref(*in)) || !got.TS.Equal(in.TS) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", deref(*got), deref(*in))
	}
	if got.Error != nil || got.TTFBMS == nil {
		t.Fatal("nullable columns not preserved")
	}
	byID, err := s.Request(got.ID)
	if err != nil || byID.RequestID != "abc" {
		t.Fatalf("Request(id) = %v, %v", byID, err)
	}
	if _, err := s.Request(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id: err = %v", err)
	}
	if _, err := s.RequestByRequestID("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing request id: err = %v", err)
	}
}

// deref renders pointer fields as values so rows can be compared as text.
func deref(r Request) map[string]any {
	str := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	num := func(p *int64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	var cost any
	if r.CostUSD != nil {
		cost = *r.CostUSD
	}
	return map[string]any{
		"id": r.ID, "ts": r.TS.UnixMilli(), "rid": r.RequestID, "urid": str(r.UpstreamRequestID),
		"proto": r.Protocol, "op": r.Operation, "model": r.ClientModel, "route": str(r.Route),
		"provider": str(r.Provider), "umodel": str(r.UpstreamModel), "status": r.Status,
		"outcome": r.Outcome, "stream": r.Stream, "attempts": r.Attempts, "ams": r.AttemptsMS,
		"ttfb": num(r.TTFBMS), "total": r.TotalMS, "routing": r.RoutingJSON, "alog": r.AttemptLog,
		"akey": str(r.AffinityKey), "in": num(r.InputTokens), "cr": num(r.CacheReadTokens),
		"cw": num(r.CacheWriteTokens), "cw1h": num(r.CacheWrite1hTokens), "out": num(r.OutputTokens),
		"reason": num(r.ReasoningTokens), "usage": str(r.UsageJSON), "cost": cost,
		"pkey": str(r.PriceKey), "pjson": str(r.PriceJSON), "err": str(r.Error),
	}
}

func TestRequestsFilters(t *testing.T) {
	s := openTest(t, 16)
	seed(t, s)
	ids := func(rs []Request) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.RequestID)
		}
		return out
	}
	all, err := s.Requests(RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	idOf := map[string]int64{}
	for _, r := range all {
		idOf[r.RequestID] = r.ID
	}

	tests := []struct {
		name string
		f    RequestFilter
		want string
	}{
		{"all newest first", RequestFilter{}, "[req-5 req-4 req-3 req-2 req-1 req-0]"},
		{"since", RequestFilter{Since: base.Add(25 * time.Hour)}, "[req-5 req-4 req-3]"},
		{"until exclusive", RequestFilter{Until: base.Add(2 * time.Hour)}, "[req-1 req-0]"},
		{"protocol", RequestFilter{Protocol: "chat"}, "[req-3 req-2]"},
		{"provider", RequestFilter{Provider: "moonshot"}, "[req-5 req-1 req-0]"},
		{"route", RequestFilter{Route: "*"}, "[req-4]"},
		{"outcome", RequestFilter{Outcome: "upstream_error_event"}, "[req-2]"},
		{"status 4xx", RequestFilter{StatusClass: 4}, "[req-3]"},
		{"status 5xx", RequestFilter{StatusClass: 5}, "[req-4]"},
		{"model substring", RequestFilter{ModelContains: "GPT-5"}, "[req-3 req-2]"},
		{"model wildcard literal", RequestFilter{ModelContains: "_model%"}, "[req-4]"},
		{"model underscore literal", RequestFilter{ModelContains: "_"}, "[req-4]"},
		{"errors only", RequestFilter{ErrorsOnly: true}, "[req-4 req-3 req-2]"},
		{"limit", RequestFilter{Limit: 2}, "[req-5 req-4]"},
		{"before cursor", RequestFilter{BeforeID: idOf["req-3"], Limit: 2}, "[req-2 req-1]"},
		{"after cursor ascending", RequestFilter{AfterID: idOf["req-3"]}, "[req-4 req-5]"},
		{"combined", RequestFilter{Protocol: "anthropic", Since: base.Add(time.Minute)}, "[req-5 req-1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Requests(tt.f)
			if err != nil {
				t.Fatal(err)
			}
			if g := fmt.Sprint(ids(got)); g != tt.want {
				t.Fatalf("got %s, want %s", g, tt.want)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	s := openTest(t, 16)
	seed(t, s)
	search := rowSpec{at: 3 * time.Hour, proto: "responses", op: "search", model: "kimi-k3", route: "kimi-k3", provider: "moonshot", status: 200, outcome: "completed", noUsage: true}
	s.Record(search.request(len(fixture)))
	flush(t, s, len(fixture)+1)

	rows, total, err := s.Usage(UsageFilter{}, "model")
	if err != nil {
		t.Fatal(err)
	}
	// count_tokens (req-1, input 999) and search (req-6) are excluded everywhere.
	want := Aggregate{Requests: 5, Errors: 3, Input: 160, Output: 16, CostUSD: 0.75, Unpriced: 1, NoUsage: 2}
	if total != want {
		t.Fatalf("total = %+v, want %+v", total, want)
	}
	if fmt.Sprint(keys(rows)) != "[kimi-k3 gpt-5.5 my_model%]" {
		t.Fatalf("model order = %v", keys(rows))
	}
	if rows[0].Requests != 2 || rows[0].Input != 110 || rows[0].CostUSD != 0.75 {
		t.Fatalf("kimi-k3 = %+v", rows[0])
	}
	if rows[1].Unpriced != 1 || rows[1].NoUsage != 1 || rows[1].Errors != 2 {
		t.Fatalf("gpt-5.5 = %+v", rows[1])
	}

	tests := []struct {
		by   string
		f    UsageFilter
		want string
	}{
		{"provider", UsageFilter{}, "[moonshot openai relay]"},
		{"route", UsageFilter{}, "[kimi-k3 gpt-* *]"},
		{"day", UsageFilter{}, "[2026-10-01 2026-10-02]"},
		{"day", UsageFilter{Model: "kimi-k3"}, "[2026-10-01 2026-10-02]"},
		{"model", UsageFilter{Protocol: "chat"}, "[gpt-5.5]"},
		{"model", UsageFilter{Provider: "relay"}, "[my_model%]"},
		{"model", UsageFilter{Route: "kimi-k3", Since: base.Add(time.Hour)}, "[kimi-k3]"},
		{"model", UsageFilter{Until: base.Add(time.Hour)}, "[kimi-k3]"},
	}
	for _, tt := range tests {
		rows, _, err := s.Usage(tt.f, tt.by)
		if err != nil {
			t.Fatal(err)
		}
		if g := fmt.Sprint(keys(rows)); g != tt.want {
			t.Errorf("by %s %+v: got %s, want %s", tt.by, tt.f, g, tt.want)
		}
	}
	day, _, _ := s.Usage(UsageFilter{}, "day")
	if day[0].Requests != 2 || day[1].Requests != 3 {
		t.Fatalf("day counts = %+v", day)
	}
	if _, _, err := s.Usage(UsageFilter{}, "week"); err == nil {
		t.Fatal("unknown grouping accepted")
	}

	unpriced, err := s.UnpricedModels(UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(unpriced) != "[{gpt-5.5 1}]" {
		t.Fatalf("unpriced = %v", unpriced)
	}
}

func keys(rows []Aggregate) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Key)
	}
	return out
}

func TestProviderStats(t *testing.T) {
	s := openTest(t, 16)
	seed(t, s)
	// A client that went away before openai sent headers: openai was chosen
	// but nothing was committed.
	s.Record(rowSpec{at: 28 * time.Hour, proto: "chat", op: "create", model: "gpt-5.5", route: "gpt-*", provider: "openai",
		status: 499, outcome: "client_cancelled", noUsage: true, noTTFB: true}.request(len(fixture)))
	flush(t, s, len(fixture)+1)
	got, err := s.ProviderStats(base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ProviderStats{
		"moonshot": {Committed: 2},
		"openai":   {Committed: 2, ErrorEvents: 1},
		"relay":    {Failover: 2},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOwnership(t *testing.T) {
	s := openTest(t, 16)
	if _, _, ok, err := s.Owner("resp_1"); ok || err != nil {
		t.Fatalf("unknown id: ok=%v err=%v", ok, err)
	}
	if err := s.RecordOwner("resp_1", "openai", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	p, m, ok, err := s.Owner("resp_1")
	if err != nil || !ok || p != "openai" || m != "gpt-5.5" {
		t.Fatalf("Owner = %q %q %v %v", p, m, ok, err)
	}
	if err := s.RecordOwner("resp_1", "relay", "x"); err != nil {
		t.Fatal(err)
	}
	if p, _, _, _ := s.Owner("resp_1"); p != "relay" {
		t.Fatalf("re-record kept %q", p)
	}
}

func TestSweep(t *testing.T) {
	s := openTest(t, 16)
	seed(t, s)
	if err := s.RecordOwner("resp_new", "p", "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`INSERT INTO owned_responses VALUES ('resp_old', 'p', 'm', ?)`, base.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	s.sweep(base.Add(2 * time.Hour)) // rows at 0 and 1h go
	rows, err := s.Requests(RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("after sweep %d rows, want 4", len(rows))
	}
	if _, _, ok, _ := s.Owner("resp_old"); ok {
		t.Fatal("old ownership row survived")
	}
	if _, _, ok, _ := s.Owner("resp_new"); !ok {
		t.Fatal("new ownership row deleted")
	}
}

func TestQueueOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.db")
	s, err := open(path, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the writer's only connection so it stalls and the queue fills.
	conn, err := s.w.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	for i := range n {
		s.Record(fixture[0].request(i))
	}
	if s.Dropped() == 0 {
		t.Fatal("nothing dropped with a stalled writer")
	}
	conn.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := open(path, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var stored int64
	s2.r.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&stored)
	if stored+s.Dropped() != n {
		t.Fatalf("stored %d + dropped %d != %d", stored, s.Dropped(), n)
	}
	s.Record(fixture[0].request(n))
	if s.Dropped() == 0 || stored+s.Dropped() != n+1 {
		t.Fatal("record after close not counted as dropped")
	}
}

func TestCloseDrains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.db")
	s, err := Open(path, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 1000 {
		s.Record(&Request{TS: now, RequestID: fmt.Sprint(i), Protocol: "chat", Operation: "create",
			ClientModel: "m", Status: 200, Outcome: "completed", RoutingJSON: "{}", AttemptLog: "[]"})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Dropped() != 0 {
		t.Fatalf("dropped %d", s.Dropped())
	}
	s, err = Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.r.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1000 {
		t.Fatalf("stored %d rows, want 1000", n)
	}
	var mode string
	s.r.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode = %q", mode)
	}
}
