package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"

	"verbatim/internal/affinity"
	"verbatim/internal/config"
	"verbatim/internal/models"
	"verbatim/internal/pricing"
	"verbatim/internal/protocol"
	"verbatim/internal/route"
	"verbatim/internal/store"
)

// upstream is a fake provider that records every request it receives.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seen
}

type seen struct {
	path, query string
	header      http.Header
	body        string
}

func newUpstream(t *testing.T, fn http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, seen{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
		u.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		fn(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *upstream) last() seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqs[len(u.reqs)-1]
}

// closedURL is the address of a server that is no longer listening.
func closedURL(t *testing.T) string {
	s := httptest.NewServer(http.NotFoundHandler())
	s.Close()
	return s.URL
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// switchable replies with the status currently stored in it.
func switchable(status *atomic.Int32, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { reply(int(status.Load()), body)(w, r) }
}

// prov renders one provider entry of the YAML configuration, with a single
// endpoint for protocol proto at url; extra lines go under the provider.
func prov(name, proto, url string, extra ...string) string {
	s := fmt.Sprintf("  %s:\n    key: key-%s\n    endpoints:\n      %s: {base_url: %q}\n", name, name, proto, url)
	for _, e := range extra {
		s += "    " + e + "\n"
	}
	return s
}

type gateway struct {
	*httptest.Server
	h      *Handler
	cfg    *config.Config
	store  *store.Store
	dbPath string
}

func newGateway(t *testing.T, yaml string, opts ...func(*Options)) *gateway {
	t.Helper()
	cfg, _, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "verbatim.db")
	st, err := store.Open(dbPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rules, err := affinity.Compile(cfg.Affinity)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		Config:   cfg,
		Routes:   route.New(cfg),
		Breakers: route.NewBreakers(cfg.Breaker.FailureThreshold, cfg.Breaker.Cooldown.Std()),
		Pins:     affinity.NewPins(cfg.Affinity.TTL.Std(), 1000),
		Rules:    rules,
		Store:    st,
		Models:   models.New(cfg, nil, time.Minute, time.Second),
	}
	for _, f := range opts {
		f(&o)
	}
	h := New(o)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &gateway{Server: srv, h: h, cfg: cfg, store: st, dbPath: dbPath}
}

type result struct {
	status int
	header http.Header
	body   string
	rid    string
	err    error // body read error
}

func (g *gateway) do(t *testing.T, method, path, body string, hdr ...string) result {
	t.Helper()
	req, err := http.NewRequest(method, g.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b), resp.Header.Get("X-Verbatim-Request-Id"), err}
}

func (g *gateway) post(t *testing.T, path, body string, hdr ...string) result {
	t.Helper()
	return g.do(t, http.MethodPost, path, body, hdr...)
}

// row waits for the log row of a request; rows are written asynchronously.
func (g *gateway) row(t *testing.T, rid string) *store.Request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := g.store.RequestByRequestID(rid)
		if err == nil {
			return r
		}
		if !errors.Is(err, store.ErrNotFound) || time.Now().After(deadline) {
			t.Fatalf("log row %q: %v", rid, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestStreamPassthroughUsageAndLogRow(t *testing.T) {
	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":2,\"output_tokens\":1}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	firstSeen := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_upstream_1")
		w.Header().Set("X-Upstream-Thing", "kept")
		w.WriteHeader(200)
		io.WriteString(w, events[0])
		w.(http.Flusher).Flush()
		// The rest is only sent once the client has the first event, which
		// proves the gateway streams instead of buffering.
		select {
		case <-firstSeen:
		case <-time.After(5 * time.Second):
		}
		for _, e := range events[1:] {
			io.WriteString(w, e)
			w.(http.Flusher).Flush()
		}
	})

	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	os.WriteFile(overrides, []byte(`{"models":{"claude-x":{"cost":{"input":1,"output":2,"cache_read":0.1,"cache_write":1.25}}}}`), 0o644)
	prices, _, err := pricing.Load(dir, nil, overrides, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway(t, "providers:\n"+prov("anth", "anthropic", up.URL)+`
routes:
  - model: claude-x
    targets:
      - provider: anth
        model: claude-up
`, func(o *Options) { o.Pricing = prices })

	clientBody := `{ "model" : "claude-x",  "stream":true, "x_unknown": {"a":[1, 2.50, "\u00e9"]},
  "messages":[{"role":"user","content":"hi"}] }`
	req, _ := http.NewRequest("POST", g.URL+"/v1/messages", strings.NewReader(clientBody))
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, len(events[0]))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	close(firstSeen)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(first)+string(rest), strings.Join(events, ""); got != want {
		t.Errorf("client body:\n%q\nwant\n%q", got, want)
	}
	if resp.Header.Get("X-Upstream-Thing") != "kept" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("upstream headers not forwarded: %v", resp.Header)
	}
	if resp.Header.Get("X-Verbatim-Attempts") != "" || resp.Header.Get("X-Verbatim-Error") != "" {
		t.Errorf("unexpected verbatim headers: %v", resp.Header)
	}
	wantUp := strings.Replace(clientBody, `"claude-x"`, `"claude-up"`, 1)
	if got := up.last().body; got != wantUp {
		t.Errorf("upstream body:\n%s\nwant\n%s", got, wantUp)
	}

	row := g.row(t, resp.Header.Get("X-Verbatim-Request-Id"))
	check := map[string][2]any{
		"protocol":            {row.Protocol, "anthropic"},
		"operation":           {row.Operation, "create"},
		"client_model":        {row.ClientModel, "claude-x"},
		"route":               {deref(row.Route), "claude-x"},
		"provider":            {deref(row.Provider), "anth"},
		"upstream_model":      {deref(row.UpstreamModel), "claude-up"},
		"upstream_request_id": {deref(row.UpstreamRequestID), "req_upstream_1"},
		"status":              {row.Status, 200},
		"outcome":             {row.Outcome, "completed"},
		"stream":              {row.Stream, true},
		"attempts":            {row.Attempts, 1},
		"attempts_ms":         {row.AttemptsMS, int64(0)},
		"input":               {deref(row.InputTokens), int64(10)},
		"cache_read":          {deref(row.CacheReadTokens), int64(5)},
		"cache_write":         {deref(row.CacheWriteTokens), int64(2)},
		"cache_write_1h":      {deref(row.CacheWrite1hTokens), nil},
		"output":              {deref(row.OutputTokens), int64(7)},
		"usage_json":          {deref(row.UsageJSON), `{"output_tokens":7}`},
		"price_key":           {deref(row.PriceKey), "claude-x"},
		"error":               {deref(row.Error), nil},
	}
	for name, c := range check {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
	if row.CostUSD == nil || fmt.Sprintf("%.9f", *row.CostUSD) != "0.000027000" {
		t.Errorf("cost = %v, want 27e-6", deref(row.CostUSD))
	}
	if row.TTFBMS == nil || row.TotalMS < *row.TTFBMS {
		t.Errorf("ttfb = %v, total = %d", deref(row.TTFBMS), row.TotalMS)
	}
	if got := gjson.Get(row.AttemptLog, "#").Int(); got != 1 || gjson.Get(row.AttemptLog, "0.status").Int() != 200 {
		t.Errorf("attempt_log = %s", row.AttemptLog)
	}
	if gjson.Get(row.RoutingJSON, "route").Str != "claude-x" || gjson.Get(row.RoutingJSON, "order.0.upstream_model").Str != "claude-up" {
		t.Errorf("routing_json = %s", row.RoutingJSON)
	}
}

// testPrices is a price table with one model at $1/$2 per million input and
// output tokens.
func testPrices(t *testing.T, model string) *pricing.Table {
	t.Helper()
	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	os.WriteFile(overrides, []byte(`{"models":{"`+model+`":{"cost":{"input":1,"output":2}}}}`), 0o644)
	prices, _, err := pricing.Load(dir, nil, overrides, nil)
	if err != nil {
		t.Fatal(err)
	}
	return prices
}

// compressor returns a writer in the given content coding.
func compressor(t *testing.T, encoding string, w io.Writer) interface {
	io.WriteCloser
	Flush() error
} {
	t.Helper()
	switch encoding {
	case "gzip":
		return gzip.NewWriter(w)
	case "deflate":
		return zlib.NewWriter(w)
	case "br":
		return brotli.NewWriter(w)
	case "zstd":
		zw, err := zstd.NewWriter(w)
		if err != nil {
			t.Fatal(err)
		}
		return zw
	}
	t.Fatalf("no compressor for %q", encoding)
	return nil
}

// The client gets an encoded body unchanged; the tee decodes its own copy of
// gzip, deflate, br and zstd, and inspects no other encoding (design 4.3).
func TestCompressedBodyUsage(t *testing.T) {
	const body = `{"id":"c1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	compressed := func(encoding string) []byte {
		var b bytes.Buffer
		w := compressor(t, encoding, &b)
		io.WriteString(w, body)
		w.Close()
		return b.Bytes()
	}
	gz := bytes.NewBuffer(compressed("gzip"))
	// A reserved block type right after the gzip header.
	corrupt := slices.Concat(gz.Bytes()[:10], []byte{0xff, 0xff}, gz.Bytes()[12:])

	for _, tc := range []struct {
		name, encoding string
		body           []byte
		usage          bool
	}{
		{"gzip", "gzip", gz.Bytes(), true},
		{"x-gzip", "X-GZip", gz.Bytes(), true},
		{"deflate", "deflate", compressed("deflate"), true},
		{"br", "br", compressed("br"), true},
		{"zstd", "zstd", compressed("zstd"), true},
		{"unknown", "compress", []byte(body), false}, // readable, but labelled with an encoding the tee does not decode
		{"stacked", "gzip, br", []byte(body), false},
		{"corrupt gzip", "gzip", corrupt, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", tc.encoding)
				w.Write(tc.body)
			})
			g := newGateway(t, "providers:\n"+prov("a", "chat", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`, func(o *Options) { o.Pricing = testPrices(t, "m") })
			res := g.post(t, "/v1/chat/completions", chatBody, "Accept-Encoding", "gzip, deflate, br, zstd")
			if res.status != 200 || res.body != string(tc.body) || res.header.Get("Content-Encoding") != tc.encoding {
				t.Fatalf("client got %d, Content-Encoding %q, body %q", res.status, res.header.Get("Content-Encoding"), res.body)
			}
			if got := up.last().header.Get("Accept-Encoding"); got != "gzip, deflate, br, zstd" {
				t.Errorf("upstream Accept-Encoding = %q", got)
			}
			row := g.row(t, res.rid)
			if row.Outcome != "completed" {
				t.Errorf("outcome = %s", row.Outcome)
			}
			if !tc.usage {
				if row.InputTokens != nil || row.OutputTokens != nil || row.UsageJSON != nil || row.CostUSD != nil {
					t.Errorf("usage recorded: input %v, output %v, cost %v", deref(row.InputTokens), deref(row.OutputTokens), deref(row.CostUSD))
				}
				return
			}
			if deref(row.InputTokens) != int64(10) || deref(row.OutputTokens) != int64(5) {
				t.Errorf("input %v, output %v", deref(row.InputTokens), deref(row.OutputTokens))
			}
			if row.CostUSD == nil || fmt.Sprintf("%.9f", *row.CostUSD) != "0.000020000" {
				t.Errorf("cost = %v, want 20e-6", deref(row.CostUSD))
			}
		})
	}
}

// A compressed Responses stream, flushed per event: usage is extracted and
// the owner is recorded before the client has the bytes that reveal the id.
func TestCompressedStreamUsageAndOwnership(t *testing.T) {
	for _, enc := range []string{"gzip", "br", "zstd"} {
		t.Run(enc, func(t *testing.T) { testCompressedStream(t, enc) })
	}
}

func testCompressedStream(t *testing.T, encoding string) {
	events := []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_gz\",\"status\":\"in_progress\"}}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_gz\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n",
	}
	firstLen := make(chan int, 1)
	firstRead := make(chan struct{})
	all := make(chan []byte, 1)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", encoding)
		var sent bytes.Buffer
		zw := compressor(t, encoding, io.MultiWriter(w, &sent))
		for i, e := range events {
			io.WriteString(zw, e)
			zw.Flush()
			w.(http.Flusher).Flush()
			if i == 0 {
				firstLen <- sent.Len()
				select {
				case <-firstRead:
				case <-time.After(5 * time.Second):
				}
			}
		}
		zw.Close()
		all <- sent.Bytes()
	})
	g := newGateway(t, "providers:\n"+prov("a", "responses", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`, func(o *Options) { o.Pricing = testPrices(t, "m") })

	req, _ := http.NewRequest("POST", g.URL+"/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	req.Header.Set("Accept-Encoding", encoding)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, <-firstLen)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	if p, _, ok, err := g.store.Owner("resp_gz"); err != nil || !ok || p != "a" {
		t.Errorf("owner of resp_gz = %q %v %v once the client has the first event", p, ok, err)
	}
	close(firstRead)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(first)+string(rest), string(<-all); got != want || resp.Header.Get("Content-Encoding") != encoding {
		t.Errorf("client body or Content-Encoding (%q) changed", resp.Header.Get("Content-Encoding"))
	}

	row := g.row(t, resp.Header.Get("X-Verbatim-Request-Id"))
	if row.Outcome != "completed" || !row.Stream || deref(row.InputTokens) != int64(10) || deref(row.OutputTokens) != int64(5) {
		t.Errorf("outcome %s, stream %v, input %v, output %v", row.Outcome, row.Stream, deref(row.InputTokens), deref(row.OutputTokens))
	}
	if row.CostUSD == nil || fmt.Sprintf("%.9f", *row.CostUSD) != "0.000020000" {
		t.Errorf("cost = %v, want 20e-6", deref(row.CostUSD))
	}
}

// A non-streamed Responses body compressed in one piece: the gzip decoder
// releases the end of the body, and with it the id, only once its input has
// ended, and the owner is still recorded.
func TestGzipBodyOwnership(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, `{"id":"resp_gzbody","object":"response","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}`)
	zw.Close()
	a := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz.Bytes())
		// Flushed, so the response is chunked and the client's body ends
		// only when the gateway's handler has returned.
		w.(http.Flusher).Flush()
	})
	b := newUpstream(t, reply(200, `{"id":"resp_b","object":"response"}`))
	g := newGateway(t, "providers:\n"+prov("a", "responses", a.URL)+prov("b", "responses", b.URL)+`
routes:
  - model: m
    targets: [{provider: b}, {provider: a, priority: 2}]
affinity:
  fallback: none
`)
	b.Close() // the create fails over to a
	res := g.post(t, "/v1/responses", `{"model":"m","input":"hi"}`, "Accept-Encoding", "gzip")
	if res.status != 200 || res.body != gz.String() {
		t.Fatalf("create: %d %q", res.status, res.body)
	}
	if row := g.row(t, res.rid); deref(row.Provider) != "a" || deref(row.InputTokens) != int64(3) {
		t.Fatalf("create row: provider %v, input %v", deref(row.Provider), deref(row.InputTokens))
	}
	res = g.post(t, "/v1/responses", `{"model":"m","previous_response_id":"resp_gzbody","input":"more"}`)
	if res.status != 200 || a.count() != 2 {
		t.Fatalf("continuation: %d %s, a got %d", res.status, res.body, a.count())
	}
	if o := gjson.Get(g.row(t, res.rid).RoutingJSON, "owner.provider").Str; o != "a" {
		t.Errorf("routing owner = %q", o)
	}
}

// twoTargets is a chat route over providers a (tried first) and b.
func twoTargets(a, b string, extra string) string {
	return "providers:\n" + prov("a", "chat", a) + prov("b", "chat", b) + `
routes:
  - model: m
    targets:
      - provider: a
      - provider: b
        priority: 2
` + extra
}

// noAffinity turns off the default prefix_hash fallback, which would pin a
// test's repeated identical requests to one target.
const noAffinity = "affinity:\n  fallback: none\n"

const chatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

func TestFailoverStatuses(t *testing.T) {
	for _, status := range []int{401, 403, 404, 408, 409, 425, 429, 500, 502, 503, 504, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a := newUpstream(t, reply(status, `{"error":"a failed"}`))
			b := newUpstream(t, reply(200, `{"ok":"b"}`))
			g := newGateway(t, twoTargets(a.URL, b.URL, ""))
			res := g.post(t, "/v1/chat/completions", chatBody)
			if res.status != 200 || res.body != `{"ok":"b"}` {
				t.Fatalf("got %d %s", res.status, res.body)
			}
			if res.header.Get("X-Verbatim-Attempts") != "" {
				t.Error("x-verbatim-attempts set on a committed response")
			}
			row := g.row(t, res.rid)
			if row.Attempts != 2 || deref(row.Provider) != "b" {
				t.Errorf("attempts = %d, provider = %v", row.Attempts, deref(row.Provider))
			}
			if s := gjson.Get(row.AttemptLog, "0.status").Int(); s != int64(status) {
				t.Errorf("attempt_log[0].status = %d", s)
			}
			if ex := gjson.Get(row.AttemptLog, "0.excerpt").Str; ex != `{"error":"a failed"}` {
				t.Errorf("attempt_log[0].excerpt = %q", ex)
			}
			if row.AttemptsMS != gjson.Get(row.AttemptLog, "0.ms").Int() {
				t.Errorf("attempts_ms = %d, attempt_log = %s", row.AttemptsMS, row.AttemptLog)
			}
		})
	}

	t.Run("connect error", func(t *testing.T) {
		b := newUpstream(t, reply(200, `{"ok":"b"}`))
		g := newGateway(t, twoTargets(closedURL(t), b.URL, ""))
		res := g.post(t, "/v1/chat/completions", chatBody)
		if res.status != 200 {
			t.Fatalf("got %d %s", res.status, res.body)
		}
		if row := g.row(t, res.rid); gjson.Get(row.AttemptLog, "0.error").Str == "" {
			t.Errorf("attempt_log = %s", row.AttemptLog)
		}
	})

	t.Run("header timeout", func(t *testing.T) {
		a := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		})
		b := newUpstream(t, reply(200, `{"ok":"b"}`))
		g := newGateway(t, twoTargets(a.URL, b.URL, "server:\n  header_timeout: 100ms\n"))
		res := g.post(t, "/v1/chat/completions", chatBody)
		if res.status != 200 {
			t.Fatalf("got %d %s", res.status, res.body)
		}
		if row := g.row(t, res.rid); !strings.Contains(gjson.Get(row.AttemptLog, "0.error").Str, "timeout") {
			t.Errorf("attempt_log = %s", row.AttemptLog)
		}
	})
}

func TestNonFailoverStatusReturnedUnchanged(t *testing.T) {
	for _, status := range []int{400, 402, 413, 422, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := `{"error":{"message":"bad thinking signature"}}`
			a := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Upstream", "yes")
				reply(status, body)(w, nil)
			})
			b := newUpstream(t, reply(200, `{}`))
			g := newGateway(t, twoTargets(a.URL, b.URL, ""))
			res := g.post(t, "/v1/chat/completions", chatBody)
			if res.status != status || res.body != body || res.header.Get("X-Upstream") != "yes" {
				t.Fatalf("got %d %q %v", res.status, res.body, res.header)
			}
			if res.header.Get("X-Verbatim-Error") != "" || res.header.Get("X-Verbatim-Attempts") != "" {
				t.Errorf("verbatim headers on a forwarded response: %v", res.header)
			}
			if b.count() != 0 {
				t.Error("failed over on a non-eligible status")
			}
			row := g.row(t, res.rid)
			if row.Outcome != "completed" || deref(row.Error) != body {
				t.Errorf("outcome = %s, error = %v", row.Outcome, deref(row.Error))
			}
		})
	}
}

func TestExhaustion(t *testing.T) {
	t.Run("last response returned unchanged", func(t *testing.T) {
		a := newUpstream(t, reply(503, `A`))
		b := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			reply(429, `{"error":"B"}`)(w, nil)
		})
		g := newGateway(t, twoTargets(a.URL, b.URL, ""))
		res := g.post(t, "/v1/chat/completions", chatBody)
		if res.status != 429 || res.body != `{"error":"B"}` || res.header.Get("Retry-After") != "7" {
			t.Fatalf("got %d %q %v", res.status, res.body, res.header)
		}
		if res.header.Get("X-Verbatim-Attempts") != "2" || res.header.Get("X-Verbatim-Error") != "" {
			t.Errorf("headers = %v", res.header)
		}
		row := g.row(t, res.rid)
		if row.Status != 429 || row.Outcome != "completed" || row.Attempts != 2 || deref(row.Provider) != "b" || deref(row.Error) != `{"error":"B"}` {
			t.Errorf("row = %+v", row)
		}
		if row.AttemptsMS != gjson.Get(row.AttemptLog, "0.ms").Int() {
			t.Errorf("attempts_ms = %d, attempt_log = %s", row.AttemptsMS, row.AttemptLog)
		}
	})

	t.Run("last attempt without response", func(t *testing.T) {
		a := newUpstream(t, reply(503, `A`))
		g := newGateway(t, twoTargets(a.URL, closedURL(t), ""))
		res := g.post(t, "/v1/chat/completions", chatBody)
		if res.status != 502 || res.header.Get("X-Verbatim-Error") != "all_upstreams_failed" {
			t.Fatalf("got %d %v %s", res.status, res.header, res.body)
		}
		msg := gjson.Get(res.body, "error.message").Str
		if !strings.Contains(msg, "a: HTTP 503") || !strings.Contains(msg, "b: ") {
			t.Errorf("message = %q", msg)
		}
	})
}

func TestAllConnectFailuresNative502(t *testing.T) {
	for _, tc := range []struct{ typ, path, body, codePath, msgPath string }{
		{"chat", "/v1/chat/completions", chatBody, "error.code", "error.message"},
		{"responses", "/v1/responses", `{"model":"m","input":"hi"}`, "error.code", "error.message"},
		{"anthropic", "/v1/messages", `{"model":"m","messages":[]}`, "", "error.message"},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			g := newGateway(t, "providers:\n"+prov("a", tc.typ, closedURL(t))+prov("b", tc.typ, closedURL(t))+`
routes:
  - model: m
    targets: [{provider: a}, {provider: b}]
`)
			res := g.post(t, tc.path, tc.body)
			if res.status != 502 || res.header.Get("X-Verbatim-Error") != "all_upstreams_failed" || res.header.Get("X-Verbatim-Attempts") != "2" {
				t.Fatalf("got %d %v", res.status, res.header)
			}
			if tc.codePath != "" && gjson.Get(res.body, tc.codePath).Str != "all_upstreams_failed" {
				t.Errorf("body = %s", res.body)
			}
			if tc.typ == "anthropic" && (gjson.Get(res.body, "type").Str != "error" || gjson.Get(res.body, "error.type").Str != "api_error") {
				t.Errorf("body = %s", res.body)
			}
			msg := gjson.Get(res.body, tc.msgPath).Str
			if !strings.Contains(msg, "a: ") || !strings.Contains(msg, "b: ") {
				t.Errorf("message = %q", msg)
			}
			row := g.row(t, res.rid)
			if row.Outcome != "gateway_error" || row.Status != 502 || row.Attempts != 2 || row.TTFBMS != nil {
				t.Errorf("row = %+v", row)
			}
		})
	}
}

func TestBreaker(t *testing.T) {
	var aStatus atomic.Int32
	aStatus.Store(500)
	release := make(chan struct{})
	var block atomic.Bool
	a := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if block.Load() {
			<-release
		}
		switchable(&aStatus, `{"from":"a"}`)(w, r)
	})
	b := newUpstream(t, reply(200, `{"from":"b"}`))
	g := newGateway(t, twoTargets(a.URL, b.URL, "breaker:\n  failure_threshold: 2\n  cooldown: 200ms\n"+noAffinity))
	breakers := g.h.breakers

	for range 2 {
		if res := g.post(t, "/v1/chat/completions", chatBody); res.status != 200 {
			t.Fatalf("got %d", res.status)
		}
	}
	if st := breakers.State("a.chat"); st.State != "open" {
		t.Fatalf("breaker after 2 failures: %+v", st)
	}
	res := g.post(t, "/v1/chat/completions", chatBody)
	if a.count() != 2 || res.body != `{"from":"b"}` {
		t.Fatalf("open breaker not skipped: a got %d requests", a.count())
	}
	if row := g.row(t, res.rid); gjson.Get(row.RoutingJSON, "skipped_open").Raw != `["a"]` || row.Attempts != 1 {
		t.Errorf("routing_json = %s, attempts = %d", row.RoutingJSON, row.Attempts)
	}

	// After the cooldown exactly one request probes a.
	time.Sleep(250 * time.Millisecond)
	aStatus.Store(200)
	block.Store(true)
	probe := make(chan result)
	go func() { probe <- g.post(t, "/v1/chat/completions", chatBody) }()
	deadline := time.Now().Add(5 * time.Second)
	for a.count() != 3 {
		if time.Now().After(deadline) {
			t.Fatal("probe never reached a")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if res := g.post(t, "/v1/chat/completions", chatBody); res.body != `{"from":"b"}` || a.count() != 3 {
		t.Fatalf("second request during the probe went to a")
	}
	close(release)
	if res := <-probe; res.body != `{"from":"a"}` {
		t.Fatalf("probe got %s", res.body)
	}
	if st := breakers.State("a.chat"); st.State != "closed" || st.Failures != 0 {
		t.Fatalf("breaker after a successful probe: %+v", st)
	}
	block.Store(false)
	if res := g.post(t, "/v1/chat/completions", chatBody); res.body != `{"from":"a"}` {
		t.Fatalf("closed breaker: got %s", res.body)
	}
}

func TestBreakerIgnoresAuthAndNotFound(t *testing.T) {
	a := newUpstream(t, reply(401, `{}`))
	b := newUpstream(t, reply(200, `{}`))
	g := newGateway(t, twoTargets(a.URL, b.URL, "breaker:\n  failure_threshold: 1\n"+noAffinity))
	for range 3 {
		g.post(t, "/v1/chat/completions", chatBody)
	}
	if a.count() != 3 || g.h.breakers.State("a.chat").State != "closed" {
		t.Errorf("401 fed the breaker: a got %d, state %+v", a.count(), g.h.breakers.State("a.chat"))
	}
}

// sessionTargets is a chat route over a and b with header affinity.
func sessionTargets(a, b, mode string) string {
	return twoTargets(a, b, fmt.Sprintf(`
affinity:
  mode: %s
  rules:
    - match: {protocol: chat}
      key: [{header: x-session-affinity}]
  fallback: none
`, mode))
}

func pinState(t *testing.T, g *gateway, rid string) (state, provider string) {
	row := g.row(t, rid)
	return gjson.Get(row.RoutingJSON, "pin.state").Str, deref(row.Provider).(string)
}

func TestPinLifecycle(t *testing.T) {
	var aStatus atomic.Int32
	aStatus.Store(200)
	a := newUpstream(t, switchable(&aStatus, `{"from":"a"}`))
	b := newUpstream(t, reply(200, `{"from":"b"}`))
	g := newGateway(t, sessionTargets(a.URL, b.URL, "prefer"))
	send := func(session string) result {
		return g.post(t, "/v1/chat/completions", chatBody, "X-Session-Affinity", session)
	}

	steps := []struct {
		aStatus             int32
		wantState, wantProv string
	}{
		{200, "reserved", "a"}, // first request reserves the first target
		{200, "hit", "a"},
		{503, "moved", "b"}, // the pinned target fails: the pin moves to the committed one
		{200, "hit", "b"},   // and stays there although a is healthy and tried first otherwise
	}
	for i, s := range steps {
		aStatus.Store(s.aStatus)
		res := send("S")
		state, provider := pinState(t, g, res.rid)
		if state != s.wantState || provider != s.wantProv {
			t.Errorf("step %d: pin %s at %s, want %s at %s", i, state, provider, s.wantState, s.wantProv)
		}
	}
	row := g.row(t, send("S").rid)
	if row.AffinityKey == nil || len(*row.AffinityKey) != 64 || gjson.Get(row.RoutingJSON, "pin.source").Str != "header:x-session-affinity" {
		t.Errorf("affinity_key = %v, routing_json = %s", deref(row.AffinityKey), row.RoutingJSON)
	}

	// A request that fails entirely releases its own reservation.
	aStatus.Store(503)
	b.Close()
	if res := send("T"); res.status != 502 {
		t.Fatalf("got %d", res.status)
	}
	key, _, _ := g.h.rules.Key(protocol.Lookup(protocol.Chat), "m", http.Header{"X-Session-Affinity": {"T"}}, nil)
	if _, ok := g.h.pins.Get(affinity.PinKey{Protocol: "chat", Key: key, Model: "m"}); ok {
		t.Error("reservation of a failed request was kept")
	}
	if g.h.pins.Len() != 1 {
		t.Errorf("pins = %d, want only session S", g.h.pins.Len())
	}
}

// A reserves a session's pin, B hits it and commits on the same target, then
// A fails entirely: the pin has served a committed response and stays.
func TestPinCommittedByHitSurvivesRelease(t *testing.T) {
	release := make(chan struct{})
	var n atomic.Int32
	a := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			<-release
			reply(503, `{}`)(w, r)
			return
		}
		reply(200, `{"from":"a"}`)(w, r)
	})
	g := newGateway(t, sessionTargets(a.URL, closedURL(t), "prefer"))
	send := func() result { return g.post(t, "/v1/chat/completions", chatBody, "X-Session-Affinity", "S") }

	first := make(chan result)
	go func() { first <- send() }()
	for a.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	res := send()
	close(release)
	if state, provider := pinState(t, g, res.rid); res.status != 200 || state != "hit" || provider != "a" {
		t.Fatalf("B: %d, pin %s at %s", res.status, state, provider)
	}
	if res := <-first; res.status != 502 {
		t.Fatalf("A: got %d %s", res.status, res.body)
	}
	key, _, _ := g.h.rules.Key(protocol.Lookup(protocol.Chat), "m", http.Header{"X-Session-Affinity": {"S"}}, nil)
	if got, ok := g.h.pins.Get(affinity.PinKey{Protocol: "chat", Key: key, Model: "m"}); !ok || got.Provider != "a" {
		t.Errorf("pin after A's failure = %v %v, want kept at a", got, ok)
	}
}

func TestPinStrict(t *testing.T) {
	var aStatus atomic.Int32
	aStatus.Store(200)
	a := newUpstream(t, switchable(&aStatus, `{"from":"a"}`))
	b := newUpstream(t, reply(200, `{"from":"b"}`))
	g := newGateway(t, sessionTargets(a.URL, b.URL, "strict")+"breaker:\n  failure_threshold: 2\n  cooldown: 1h\n")
	send := func() result { return g.post(t, "/v1/chat/completions", chatBody, "X-Session-Affinity", "S") }

	send() // reserves a
	aStatus.Store(503)
	res := send()
	if res.status != 503 || res.body != `{"from":"a"}` || res.header.Get("X-Verbatim-Attempts") != "1" || b.count() != 0 {
		t.Fatalf("strict pin failure: got %d %s, b got %d", res.status, res.body, b.count())
	}
	send() // second failure opens a's breaker
	res = send()
	if res.status != 502 || res.header.Get("X-Verbatim-Error") != "all_upstreams_open" || b.count() != 0 {
		t.Fatalf("strict pin skipped: got %d %v, b got %d", res.status, res.header, b.count())
	}
	if gjson.Get(res.body, "error.code").Str != "all_upstreams_open" {
		t.Errorf("body = %s", res.body)
	}
	// A new session behaves like a first request and fails over.
	if res := g.post(t, "/v1/chat/completions", chatBody, "X-Session-Affinity", "new"); res.body != `{"from":"b"}` {
		t.Errorf("new session: %d %s", res.status, res.body)
	}
}

func TestConcurrentFirstRequestsShareTarget(t *testing.T) {
	var ups []*upstream
	var cfg strings.Builder
	cfg.WriteString("providers:\n")
	for i := range 3 {
		u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(30 * time.Millisecond)
			reply(200, `{}`)(w, r)
		})
		ups = append(ups, u)
		cfg.WriteString(prov(fmt.Sprintf("p%d", i), "chat", u.URL))
	}
	cfg.WriteString(`
routes:
  - model: m
    targets: [{provider: p0}, {provider: p1}, {provider: p2}]
affinity:
  rules:
    - key: [{header: x-session-affinity}]
`)
	g := newGateway(t, cfg.String())
	for s := range 5 {
		before := make([]int, len(ups))
		for i, u := range ups {
			before[i] = u.count()
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { g.post(t, "/v1/chat/completions", chatBody, "X-Session-Affinity", fmt.Sprint("s", s)) })
		}
		wg.Wait()
		used := 0
		for i, u := range ups {
			if u.count() != before[i] {
				used++
			}
		}
		if used != 1 {
			t.Errorf("session %d spread over %d targets", s, used)
		}
	}
}

func TestCountTokensFollowsPinOnly(t *testing.T) {
	var aCreate, aCount, bCount atomic.Int32
	aCreate.Store(200)
	aCount.Store(200)
	bCount.Store(200)
	handler := func(create, count *atomic.Int32, name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			status := create
			if strings.HasSuffix(r.URL.Path, "/count_tokens") {
				status = count
			}
			reply(int(status.Load()), `{"input_tokens":3,"from":"`+name+`"}`)(w, r)
		}
	}
	a := newUpstream(t, handler(&aCreate, &aCount, "a"))
	var bCreate atomic.Int32
	bCreate.Store(200)
	b := newUpstream(t, handler(&bCreate, &bCount, "b"))
	g := newGateway(t, "providers:\n"+prov("a", "anthropic", a.URL)+prov("b", "anthropic", b.URL)+`
routes:
  - model: m
    targets: [{provider: a}, {provider: b, priority: 2}]
affinity:
  rules:
    - match: {protocol: anthropic}
      key: [{body: metadata.user_id}]
breaker:
  failure_threshold: 1
`)
	count := func() result {
		return g.post(t, "/v1/messages/count_tokens", `{"model":"m","metadata":{"user_id":"S"},"messages":[]}`)
	}
	create := func() result {
		return g.post(t, "/v1/messages", `{"model":"m","metadata":{"user_id":"S"},"messages":[]}`)
	}

	res := count()
	if gjson.Get(res.body, "from").Str != "a" || g.h.pins.Len() != 0 {
		t.Fatalf("count_tokens without a pin: %s, pins %d", res.body, g.h.pins.Len())
	}
	row := g.row(t, res.rid)
	if gjson.Get(row.RoutingJSON, "pin.state").Str != "none" || deref(row.InputTokens) != int64(3) || row.CostUSD != nil {
		t.Errorf("count_tokens row: routing %s, input %v", row.RoutingJSON, deref(row.InputTokens))
	}

	// count_tokens failures fail over but never feed the breaker.
	aCount.Store(500)
	for range 3 {
		if res := count(); gjson.Get(res.body, "from").Str != "b" {
			t.Fatalf("count_tokens did not fail over: %s", res.body)
		}
	}
	if st := g.h.breakers.State("a.anthropic"); st.State != "closed" || st.Failures != 0 {
		t.Fatalf("count_tokens fed the breaker: %+v", st)
	}

	// A create that fails over pins the session to b.
	aCreate.Store(529)
	create()
	aCreate.Store(200)
	g.h.breakers.Success("a.anthropic")

	aCount.Store(200)
	res = count()
	if gjson.Get(res.body, "from").Str != "b" {
		t.Fatalf("count_tokens did not follow the pin: %s", res.body)
	}
	if st := gjson.Get(g.row(t, res.rid).RoutingJSON, "pin.state").Str; st != "hit" {
		t.Errorf("pin state = %s", st)
	}

	// A failing count_tokens on the pinned target does not move the pin.
	bCount.Store(503)
	if res := count(); gjson.Get(res.body, "from").Str != "a" {
		t.Fatalf("count_tokens did not fail over: %s", res.body)
	}
	if res := create(); gjson.Get(res.body, "from").Str != "b" {
		t.Errorf("count_tokens moved the pin: create went to %s", gjson.Get(res.body, "from").Str)
	}
}

// Codex's standalone web search reaches the session's pinned upstream through
// its body id, which carries the value Codex sends as session-id.
func TestSearchFollowsSessionPin(t *testing.T) {
	var aCreate, bSearch atomic.Int32
	aCreate.Store(529)
	bSearch.Store(200)
	handler := func(create, search *atomic.Int32, name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			status := create
			if r.URL.Path == "/v1/alpha/search" {
				status = search
			}
			reply(int(status.Load()), `{"output":"found","from":"`+name+`"}`)(w, r)
		}
	}
	var ok atomic.Int32
	ok.Store(200)
	a := newUpstream(t, handler(&aCreate, &ok, "a"))
	b := newUpstream(t, handler(&ok, &bSearch, "b"))
	g := newGateway(t, "providers:\n"+prov("a", "responses", a.URL+"/v1")+prov("b", "responses", b.URL+"/v1")+`
routes:
  - model: gpt-*
    targets: [{provider: a}, {provider: b, priority: 2}]
affinity:
  rules:
    - match: {protocol: responses}
      key: [{header: session-id}, {body: prompt_cache_key}, {body: id}]
breaker:
  failure_threshold: 1
`)
	create := func() result {
		return g.post(t, "/v1/responses", `{"model":"gpt-x","input":"hi"}`, "Session-Id", "S")
	}
	search := func(id string) result {
		return g.post(t, "/v1/alpha/search", `{"id":"`+id+`","model":"gpt-x","commands":{"search_query":[{"q":"go"}]}}`)
	}

	// A create that fails over pins the session to b.
	if res := create(); gjson.Get(res.body, "from").Str != "b" {
		t.Fatalf("create: %s", res.body)
	}
	g.h.breakers.Success("a.responses")

	res := search("S")
	if gjson.Get(res.body, "from").Str != "b" || b.last().path != "/v1/alpha/search" || b.last().header.Get("Authorization") != "Bearer key-b" {
		t.Fatalf("search did not follow the pin: %s, upstream saw %s", res.body, b.last().path)
	}
	row := g.row(t, res.rid)
	if row.Protocol != "responses" || row.Operation != "search" || row.InputTokens != nil || row.CostUSD != nil ||
		gjson.Get(row.RoutingJSON, "pin.state").Str != "hit" || gjson.Get(row.RoutingJSON, "pin.source").Str != "body:id" {
		t.Errorf("search row: %s %s, routing %s", row.Protocol, row.Operation, row.RoutingJSON)
	}

	// An upstream without the endpoint fails over, without feeding its
	// breaker or moving the session's pin.
	bSearch.Store(500)
	if res := search("S"); gjson.Get(res.body, "from").Str != "a" {
		t.Fatalf("search did not fail over: %s", res.body)
	}
	if st := g.h.breakers.State("b.responses"); st.State != "closed" || st.Failures != 0 {
		t.Fatalf("search fed the breaker: %+v", st)
	}
	aCreate.Store(200)
	if res := create(); gjson.Get(res.body, "from").Str != "b" {
		t.Errorf("search moved the pin: create went to %s", gjson.Get(res.body, "from").Str)
	}

	// A search outside any pinned session creates no pin.
	pins := g.h.pins.Len()
	search("T")
	if g.h.pins.Len() != pins {
		t.Errorf("search created a pin")
	}
}

func TestResponseOwnership(t *testing.T) {
	var aStatus atomic.Int32
	aStatus.Store(200)
	a := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if aStatus.Load() != 200 {
			reply(int(aStatus.Load()), `{}`)(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\",\"status\":\"in_progress\"}}\n\n")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_a\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\n")
	})
	b := newUpstream(t, reply(200, `{"id":"resp_b","object":"response","usage":{"input_tokens":1,"output_tokens":1}}`))
	g := newGateway(t, "providers:\n"+prov("a", "responses", a.URL)+prov("b", "responses", b.URL)+`
routes:
  - model: m
    targets: [{provider: a}, {provider: b, priority: 2}]
`)

	if res := g.post(t, "/v1/responses", `{"model":"m","stream":true,"input":"hi"}`); res.status != 200 {
		t.Fatalf("got %d", res.status)
	}
	// Recorded before the client could read the id, so no waiting here.
	if p, m, ok, err := g.store.Owner("resp_a"); err != nil || !ok || p != "a" || m != "m" {
		t.Fatalf("owner of resp_a = %q %q %v %v", p, m, ok, err)
	}

	aStatus.Store(503)
	g.post(t, "/v1/responses", `{"model":"m","input":"hi"}`)
	if p, _, ok, _ := g.store.Owner("resp_b"); !ok || p != "b" {
		t.Fatalf("owner of resp_b = %q %v", p, ok)
	}
	aStatus.Store(200)

	// The owner is the only candidate, although a is healthy and first.
	bBefore, aBefore := b.count(), a.count()
	res := g.post(t, "/v1/responses", `{"model":"m","previous_response_id":"resp_b","input":"more"}`)
	if res.status != 200 || b.count() != bBefore+1 || a.count() != aBefore {
		t.Fatalf("continuation not sent to its owner: %d", res.status)
	}
	if o := gjson.Get(g.row(t, res.rid).RoutingJSON, "owner"); o.Get("id").Str != "resp_b" || o.Get("provider").Str != "b" {
		t.Errorf("routing owner = %s", o.Raw)
	}
	res = g.post(t, "/v1/responses", `{"model":"m","conversation":{"id":"resp_a"},"input":"more"}`)
	if res.status != 200 || a.count() != aBefore+1 {
		t.Fatalf("conversation not sent to its owner: %d", res.status)
	}

	res = g.post(t, "/v1/responses", `{"model":"m","previous_response_id":"resp_unknown","input":"x"}`)
	if res.status != 404 || res.header.Get("X-Verbatim-Error") != "unknown_response_id" || gjson.Get(res.body, "error.code").Str != "unknown_response_id" {
		t.Fatalf("unknown id: %d %v %s", res.status, res.header, res.body)
	}
}

func TestOverridesPerAttempt(t *testing.T) {
	a := newUpstream(t, reply(503, `{}`))
	b := newUpstream(t, reply(200, `{}`))
	g := newGateway(t, "providers:\n"+
		prov("a", "chat", a.URL, "overrides:", "  - delete: metadata", "  - set: {path: temperature, value: 1}", "  - set: {path: extra.flags, value: {x: [1, \"y\"]}}")+
		prov("b", "chat", b.URL)+`
routes:
  - model: m
    targets: [{provider: a}, {provider: b, priority: 2}]
`)
	body := `{"model":"m","metadata":{"x":1},"temperature":0.2,"messages":[]}`
	g.post(t, "/v1/chat/completions", body)
	if got, want := a.last().body, `{"model":"m","temperature":1,"messages":[],"extra":{"flags":{"x":[1,"y"]}}}`; got != want {
		t.Errorf("a got %s\nwant %s", got, want)
	}
	if got := b.last().body; got != body {
		t.Errorf("b got %s, want the original body", got)
	}
}

func TestRequestHeadersAndURL(t *testing.T) {
	ups := map[string]*upstream{}
	for _, n := range []string{"chat", "anth", "anthbearer", "open"} {
		ups[n] = newUpstream(t, reply(200, `{}`))
	}
	g := newGateway(t, `server:
  api_key: client-key
providers:
`+prov("chat", "chat", ups["chat"].URL+"/v1", "headers:", "  x-title: verbatim", "  openai-project: null")+
		prov("anth", "anthropic", ups["anth"].URL+"/anthropic", "headers:", "  anthropic-beta: from-config")+
		fmt.Sprintf("  anthbearer:\n    key: key-anthbearer\n    endpoints:\n      anthropic: {base_url: %q, auth: bearer}\n", ups["anthbearer"].URL)+
		fmt.Sprintf("  open:\n    endpoints:\n      chat: {base_url: %q, auth: none}\n", ups["open"].URL)+`
routes:
  - model: c
    targets: [{provider: chat}]
  - model: a
    targets: [{provider: anth}]
  - model: ab
    targets: [{provider: anthbearer}]
  - model: o
    targets: [{provider: open}]
`)

	res := g.post(t, "/v1/chat/completions?beta=true&key=client-key&x=%20y", `{"model":"c"}`,
		"Authorization", "Bearer client-key", "X-Api-Key", "client-key", "X-Goog-Api-Key", "client-key",
		"Connection", "X-Hop", "X-Hop", "1", "Openai-Project", "p", "X-Keep", "1", "Accept-Encoding", "gzip, br")
	if res.status != 200 {
		t.Fatalf("got %d %s", res.status, res.body)
	}
	got := ups["chat"].last()
	if got.path != "/v1/chat/completions" || got.query != "beta=true&x=%20y" {
		t.Errorf("url = %s?%s", got.path, got.query)
	}
	want := map[string]string{
		"Authorization":   "Bearer key-chat",
		"X-Api-Key":       "",
		"X-Goog-Api-Key":  "",
		"X-Hop":           "",
		"Openai-Project":  "",
		"X-Title":         "verbatim",
		"X-Keep":          "1",
		"Accept-Encoding": "gzip, br",
	}
	for k, v := range want {
		if got.header.Get(k) != v {
			t.Errorf("chat header %s = %q, want %q", k, got.header.Get(k), v)
		}
	}

	g.post(t, "/v1/messages?beta=true", `{"model":"a"}`, "X-Api-Key", "client-key")
	got = ups["anth"].last()
	if got.path != "/anthropic/v1/messages" || got.query != "beta=true" || got.header.Get("X-Api-Key") != "key-anth" ||
		got.header.Get("Authorization") != "" || got.header.Get("Anthropic-Version") != "2023-06-01" || got.header.Get("Anthropic-Beta") != "from-config" {
		t.Errorf("anthropic request: %s?%s %v", got.path, got.query, got.header)
	}
	g.post(t, "/v1/messages", `{"model":"a"}`, "X-Api-Key", "client-key", "Anthropic-Version", "2024-01-01")
	if v := ups["anth"].last().header.Values("Anthropic-Version"); len(v) != 1 || v[0] != "2024-01-01" {
		t.Errorf("client anthropic-version replaced: %v", v)
	}
	g.post(t, "/v1/messages/count_tokens", `{"model":"ab"}`, "X-Goog-Api-Key", "client-key")
	got = ups["anthbearer"].last()
	if got.path != "/v1/messages/count_tokens" || got.header.Get("Authorization") != "Bearer key-anthbearer" || got.header.Get("X-Api-Key") != "" {
		t.Errorf("anthropic bearer request: %s %v", got.path, got.header)
	}
	g.post(t, "/v1/chat/completions?key=client-key", `{"model":"o"}`)
	got = ups["open"].last()
	if got.query != "" || got.header.Get("Authorization") != "" || got.header.Get("X-Api-Key") != "" {
		t.Errorf("auth none request: %s %v", got.query, got.header)
	}
}

// One provider serving several protocols: a request goes to the endpoint
// for its protocol with the provider's key and headers, each endpoint has its
// own circuit breaker, and a "provider.protocol" target serves that protocol
// alone.
func TestProviderEndpoints(t *testing.T) {
	chat := newUpstream(t, reply(500, `{"from":"chat"}`))
	anth := newUpstream(t, reply(200, `{"from":"anthropic"}`))
	g := newGateway(t, fmt.Sprintf(`providers:
  p:
    key: key-p
    headers: {x-title: verbatim}
    endpoints:
      chat: {base_url: %q}
      anthropic: {base_url: %q, auth: bearer}
routes:
  - model: only-anthropic
    targets: [{provider: p.anthropic}]
  - model: m
    targets: [{provider: p}]
breaker:
  failure_threshold: 1
`, chat.URL+"/v1", anth.URL)+noAffinity)

	if res := g.post(t, "/v1/chat/completions", chatBody); res.status != 500 {
		t.Fatalf("chat: got %d %s", res.status, res.body)
	}
	got := chat.last()
	if got.path != "/v1/chat/completions" || got.header.Get("Authorization") != "Bearer key-p" || got.header.Get("X-Title") != "verbatim" {
		t.Errorf("chat request: %s %v", got.path, got.header)
	}
	if st := g.h.breakers.State("p.chat"); st.State != "open" {
		t.Fatalf("chat breaker: %+v", st)
	}
	if res := g.post(t, "/v1/chat/completions", chatBody); res.status != 502 || res.header.Get("X-Verbatim-Error") != "all_upstreams_open" {
		t.Errorf("chat with its breaker open: got %d %s", res.status, res.body)
	}

	// The open chat breaker does not stop the anthropic endpoint.
	res := g.post(t, "/v1/messages", `{"model":"m"}`)
	if res.status != 200 || gjson.Get(res.body, "from").Str != "anthropic" {
		t.Fatalf("anthropic: got %d %s", res.status, res.body)
	}
	got = anth.last()
	if got.path != "/v1/messages" || got.header.Get("Authorization") != "Bearer key-p" || got.header.Get("X-Api-Key") != "" || got.header.Get("X-Title") != "verbatim" {
		t.Errorf("anthropic request: %s %v", got.path, got.header)
	}
	if row := g.row(t, res.rid); deref(row.Provider) != "p" || row.Protocol != "anthropic" {
		t.Errorf("row provider %v, protocol %s", deref(row.Provider), row.Protocol)
	}

	if res := g.post(t, "/v1/messages", `{"model":"only-anthropic"}`); res.status != 200 {
		t.Errorf("endpoint target: got %d %s", res.status, res.body)
	}
	if res := g.post(t, "/v1/chat/completions", `{"model":"only-anthropic"}`); res.status != 404 || res.header.Get("X-Verbatim-Error") != "no_route" {
		t.Errorf("endpoint target served another protocol: got %d %s", res.status, res.body)
	}
}

func TestClientAuth(t *testing.T) {
	up := newUpstream(t, reply(200, `{}`))
	g := newGateway(t, "server:\n  api_key: secret\nproviders:\n"+prov("a", "anthropic", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`)
	res := g.post(t, "/v1/messages", `{"model":"m"}`, "X-Api-Key", "wrong")
	if res.status != 401 || res.header.Get("X-Verbatim-Error") != "unauthenticated" || gjson.Get(res.body, "type").Str != "error" || res.rid == "" {
		t.Fatalf("got %d %v %s", res.status, res.header, res.body)
	}
	for _, hdr := range [][]string{
		{"Authorization", "Bearer secret"},
		{"Authorization", "bearer secret"},
		{"X-Api-Key", "secret"},
		{"X-Goog-Api-Key", "secret"},
	} {
		if res := g.post(t, "/v1/messages", `{"model":"m"}`, hdr...); res.status != 200 {
			t.Errorf("%v: got %d", hdr, res.status)
		}
	}
	if res := g.post(t, "/v1/messages?key=secret", `{"model":"m"}`); res.status != 200 {
		t.Errorf("query key: got %d", res.status)
	}
	if res := g.do(t, "GET", "/v1/models", ""); res.status != 401 {
		t.Errorf("listing without key: got %d", res.status)
	}
	if res := g.do(t, "GET", "/healthz", ""); res.status != 200 {
		t.Errorf("healthz: got %d", res.status)
	}
}

func TestGatewayErrors(t *testing.T) {
	up := newUpstream(t, reply(200, `{}`))
	g := newGateway(t, "server:\n  max_body_bytes: 64\nproviders:\n"+prov("a", "chat", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`)
	for _, tc := range []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"no route", "/v1/chat/completions", `{"model":"other"}`, 404, "no_route"},
		{"no route for protocol", "/v1/responses", `{"model":"m"}`, 404, "no_route"},
		{"no model", "/v1/chat/completions", `{"messages":[]}`, 400, "no_model"},
		{"model not a string", "/v1/chat/completions", `{"model":1}`, 400, "no_model"},
		{"too large", "/v1/chat/completions", `{"model":"m","pad":"` + strings.Repeat("x", 100) + `"}`, 413, "body_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := g.post(t, tc.path, tc.body)
			if res.status != tc.status || res.header.Get("X-Verbatim-Error") != tc.code || gjson.Get(res.body, "error.code").Str != tc.code {
				t.Fatalf("got %d %v %s", res.status, res.header, res.body)
			}
			row := g.row(t, res.rid)
			if row.Outcome != "gateway_error" || row.Status != tc.status || !strings.HasPrefix(deref(row.Error).(string), tc.code+": ") {
				t.Errorf("row = %+v", row)
			}
		})
	}
	if up.count() != 0 {
		t.Errorf("upstream called %d times", up.count())
	}
}

func TestTooManyInflight(t *testing.T) {
	release := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		reply(200, `{}`)(w, r)
	})
	g := newGateway(t, "server:\n  max_inflight: 1\nproviders:\n"+prov("a", "anthropic", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`)
	// Registered after the gateway so that it runs before the servers close.
	t.Cleanup(func() { close(release) })
	go g.post(t, "/v1/messages", `{"model":"m"}`)
	for up.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	res := g.post(t, "/v1/messages", `{"model":"m"}`)
	if res.status != 503 || res.header.Get("X-Verbatim-Error") != "too_many_inflight" || !strings.HasPrefix(gjson.Get(res.body, "error.message").Str, "too_many_inflight: ") {
		t.Fatalf("got %d %v %s", res.status, res.header, res.body)
	}
}

func TestIdleTimeoutTruncates(t *testing.T) {
	stall := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(stall) })
	g := newGateway(t, "server:\n  idle_timeout: 100ms\nproviders:\n"+prov("a", "chat", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`)
	res := g.post(t, "/v1/chat/completions", `{"model":"m","stream":true}`)
	if res.status != 200 || res.body != "data: {\"id\":\"c1\",\"choices\":[]}\n\n" {
		t.Fatalf("got %d %q", res.status, res.body)
	}
	if !errors.Is(res.err, io.ErrUnexpectedEOF) {
		t.Errorf("client read error = %v, want an aborted connection", res.err)
	}
	row := g.row(t, res.rid)
	if row.Outcome != "truncated" || row.Status != 200 || !strings.Contains(deref(row.Error).(string), "idle") {
		t.Errorf("row outcome %s, error %v", row.Outcome, deref(row.Error))
	}
}

// The client leaves while a failed attempt's body is being read: the request
// ends as cancelled, and the failed response is not returned to it.
func TestClientCancelDuringDrain(t *testing.T) {
	headersSent := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		io.WriteString(w, `{"error":`)
		w.(http.Flusher).Flush()
		close(headersSent)
		<-r.Context().Done()
	})
	g := newGateway(t, "providers:\n"+prov("a", "chat", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`)
	conn, err := net.Dial("tcp", g.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n%s", len(chatBody), chatBody)
	<-headersSent
	time.Sleep(50 * time.Millisecond) // the gateway is now reading the 503 body
	conn.(*net.TCPConn).CloseWrite()  // the server sees the client leave
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if got, err := io.ReadAll(conn); len(got) != 0 {
		t.Fatalf("client received %q (%v)", got, err)
	}

	var row *store.Request
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := g.store.Requests(store.RequestFilter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			row = &rows[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no log row")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if row.Status != 499 || row.Outcome != "client_cancelled" || gjson.Get(row.AttemptLog, "0.status").Int() != 503 {
		t.Errorf("row: status %d, outcome %s, attempt_log %s", row.Status, row.Outcome, row.AttemptLog)
	}
}

// Wait holds shutdown until a request whose client has gone has recorded its
// row, so closing the store afterwards loses nothing.
func TestWaitForExchanges(t *testing.T) {
	firstSent := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[]}\n\n")
		w.(http.Flusher).Flush()
		close(firstSent)
		<-r.Context().Done()
	})
	g := newGateway(t, "providers:\n"+prov("a", "chat", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", g.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	rid := resp.Header.Get("X-Verbatim-Request-Id")
	<-firstSent
	if g.h.Wait(50 * time.Millisecond) {
		t.Fatal("Wait returned true while a request was streaming")
	}
	cancel()
	resp.Body.Close()
	if !g.h.Wait(5 * time.Second) {
		t.Fatal("Wait timed out")
	}
	if err := g.store.Close(); err != nil {
		t.Fatal(err)
	}
	if n := g.store.Dropped(); n != 0 {
		t.Fatalf("%d rows dropped", n)
	}
	st, err := store.Open(g.dbPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row, err := st.RequestByRequestID(rid)
	if err != nil {
		t.Fatalf("row of the cancelled request: %v", err)
	}
	if row.Outcome != "client_cancelled" {
		t.Errorf("outcome = %s", row.Outcome)
	}
}

func TestUpstreamErrorEvent(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n")
		io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n")
	})
	g := newGateway(t, "providers:\n"+prov("a", "anthropic", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`)
	res := g.post(t, "/v1/messages", `{"model":"m","stream":true}`)
	row := g.row(t, res.rid)
	if res.status != 200 || row.Outcome != "upstream_error_event" || deref(row.InputTokens) != int64(3) {
		t.Errorf("status %d, outcome %s, input %v", res.status, row.Outcome, deref(row.InputTokens))
	}
	if st := g.h.breakers.State("a.anthropic"); st.Failures != 0 {
		t.Errorf("error event fed the breaker: %+v", st)
	}
}

func TestOtherPaths(t *testing.T) {
	up := newUpstream(t, reply(200, `{}`))
	g := newGateway(t, "providers:\n"+prov("a", "chat", up.URL)+prov("g", "gemini", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
  - model: gem
    targets: [{provider: g}]
  - model: "x-*"
    targets: [{provider: a}]
`)
	for _, tc := range []struct {
		method, path string
		status       int
		body, code   string
	}{
		{"GET", "/healthz", 200, `{"ok":true}`, ""},
		{"GET", "/nope", 404, `{"error":"not found"}`, ""},
		{"GET", "/v1/chat/completions", 404, `{"error":"not found"}`, ""},
		{"POST", "/v1beta/models/gem:generateContent", 404, `{"error":"not found"}`, ""},
		{"GET", "/v1/responses/resp_1", 404, "", "unsupported_operation"},
		{"DELETE", "/v1/responses/resp_1", 404, "", "unsupported_operation"},
		{"POST", "/v1/responses/resp_1/cancel", 404, "", "unsupported_operation"},
		{"GET", "/v1/responses/resp_1/input_items", 404, "", "unsupported_operation"},
		{"POST", "/v1/responses/compact", 404, "", "unsupported_operation"},
		{"GET", "/v1/responses", 404, "", "unsupported_operation"},
	} {
		res := g.do(t, tc.method, tc.path, "")
		if res.status != tc.status || (tc.body != "" && res.body != tc.body) || res.rid == "" {
			t.Errorf("%s %s: got %d %s", tc.method, tc.path, res.status, res.body)
		}
		if tc.code != "" && (gjson.Get(res.body, "error.code").Str != tc.code || res.header.Get("X-Verbatim-Error") != tc.code) {
			t.Errorf("%s %s: got %v %s", tc.method, tc.path, res.header, res.body)
		}
	}
	if up.count() != 0 {
		t.Error("an unsupported path reached the upstream")
	}

	res := g.do(t, "GET", "/v1/models", "")
	want := `{"object":"list","data":[` +
		`{"id":"m","object":"model","created":0,"owned_by":"verbatim","type":"model","display_name":"m","created_at":"1970-01-01T00:00:00Z"},` +
		`{"id":"gem","object":"model","created":0,"owned_by":"verbatim","type":"model","display_name":"gem","created_at":"1970-01-01T00:00:00Z"}],` +
		`"has_more":false,"first_id":"m","last_id":"gem"}`
	if res.status != 200 || res.body != want {
		t.Errorf("/v1/models = %d %s", res.status, res.body)
	}
	res = g.do(t, "GET", "/v1beta/models", "")
	if res.body != `{"models":[{"name":"models/m","displayName":"m"},{"name":"models/gem","displayName":"gem"}]}` {
		t.Errorf("/v1beta/models = %s", res.body)
	}
}

func TestStats(t *testing.T) {
	a := newUpstream(t, reply(500, `{}`))
	b := newUpstream(t, reply(200, `{}`))
	g := newGateway(t, sessionTargets(a.URL, b.URL, "prefer"))
	g.post(t, "/v1/chat/completions", chatBody, "X-Session-Affinity", "S")
	st := g.h.Stats()
	if st.Breakers["a.chat"].Failures != 1 || st.Pins["b.chat"] != 1 || st.PinsTotal != 1 || st.LogDropped != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestTrailersForwarded(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.WriteHeader(200)
		io.WriteString(w, `{}`)
		w.Header().Set("X-Checksum", "abc")
	})
	g := newGateway(t, "providers:\n"+prov("a", "chat", up.URL)+`
routes:
  - model: m
    targets: [{provider: a}]
`)
	req, _ := http.NewRequest("POST", g.URL+"/v1/chat/completions", strings.NewReader(chatBody))
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	if v := resp.Trailer.Get("X-Checksum"); v != "abc" {
		t.Errorf("trailer = %q, header = %v", v, resp.Header)
	}
}

func TestCompressedErrorExcerpt(t *testing.T) {
	const msg = `{"error":{"message":"rate limited","type":"rate_limit_error"}}`
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, msg)
	zw.Close()
	serve := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(status)
			w.Write(gz.Bytes())
		}
	}
	// a fails over with a compressed 429; b commits a compressed 400.
	a, b := newUpstream(t, serve(429)), newUpstream(t, serve(400))
	g := newGateway(t, "providers:\n"+prov("a", "chat", a.URL)+prov("b", "chat", b.URL)+`
routes:
  - model: m
    targets: [{provider: a}, {provider: b, priority: 2}]
`)
	res := g.post(t, "/v1/chat/completions", chatBody, "Accept-Encoding", "gzip")
	if res.status != 400 || res.body != gz.String() {
		t.Fatalf("client got %d %q", res.status, res.body)
	}
	row := g.row(t, res.rid)
	if deref(row.Error) != msg {
		t.Errorf("error column = %q, want the decompressed body", deref(row.Error))
	}
	if !strings.Contains(row.AttemptLog, `"excerpt":`+strconv.Quote(msg)) {
		t.Errorf("attempt_log excerpt not decompressed: %s", row.AttemptLog)
	}
}
