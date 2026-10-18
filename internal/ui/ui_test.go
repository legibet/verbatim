package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"verbatim/internal/config"
	"verbatim/internal/pricing"
	"verbatim/internal/proxy"
	"verbatim/internal/route"
	"verbatim/internal/store"
)

const testConfig = `
server:
  api_key: secret-key
providers:
  alpha:
    key: alpha-key
    headers:
      x-title: hidden-value
      x-drop: null
    endpoints:
      anthropic: {base_url: "https://user:pass@alpha.example/anthropic"}
      chat: {base_url: "https://alpha.example/v1"}
  beta:
    key: beta-key
    endpoints:
      chat: {base_url: "https://beta.example/v1"}
  gamma:
    endpoints:
      responses: {base_url: "http://127.0.0.1:9000/v1", auth: none}
routes:
  - model: "*"
    targets:
      - provider: alpha
      - provider: beta
      - provider: gamma
`

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, _, err := config.Parse([]byte(testConfig), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type row struct {
	ago      time.Duration
	proto    string
	model    string
	provider string
	status   int
	outcome  string
	cost     *float64
	attempts string
}

// newStore opens a temporary store holding rows, oldest first, so ids
// ascend in the order given.
func newStore(t *testing.T, rows []row) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "v.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	for i, r := range rows {
		attempts := r.attempts
		if attempts == "" {
			attempts = fmt.Sprintf(`[{"provider":%q,"status":%d}]`, r.provider, r.status)
		}
		in, out := int64(100), int64(10)
		st.Record(&store.Request{
			TS:           now.Add(-r.ago),
			RequestID:    fmt.Sprintf("req-%d", i),
			Protocol:     r.proto,
			Operation:    "create",
			ClientModel:  r.model,
			Route:        new("*"),
			Provider:     new(r.provider),
			Status:       r.status,
			Outcome:      r.outcome,
			Attempts:     1,
			TTFBMS:       new(int64(50)), // every fixture row was committed upstream
			RoutingJSON:  `{"route":"*"}`,
			AttemptLog:   attempts,
			InputTokens:  &in,
			OutputTokens: &out,
			UsageJSON:    new(`{"input_tokens":100}`),
			CostUSD:      r.cost,
		})
	}
	for range 400 {
		got, err := st.Requests(store.RequestFilter{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == len(rows) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("rows were not written")
	return nil
}

var testRows = []row{
	{ago: 50 * time.Minute, proto: "anthropic", model: "claude-opus", provider: "alpha", status: 200, outcome: "completed", cost: new(0.5)},
	{ago: 40 * time.Minute, proto: "chat", model: "gpt-x", provider: "beta", status: 502, outcome: "completed",
		attempts: `[{"provider":"alpha","status":503},{"provider":"beta","status":502}]`},
	{ago: 30 * time.Minute, proto: "anthropic", model: "claude-opus", provider: "alpha", status: 200, outcome: "upstream_error_event", cost: new(0.25)},
	{ago: 20 * time.Minute, proto: "chat", model: "gpt-x", provider: "beta", status: 200, outcome: "completed"},
	{ago: 10 * time.Minute, proto: "anthropic", model: "claude-haiku", provider: "alpha", status: 429, outcome: "completed"},
}

func newServer(t *testing.T, apiKey string, stats proxy.Stats) http.Handler {
	t.Helper()
	return New(Options{
		Store:  newStore(t, testRows),
		Stats:  func() proxy.Stats { return stats },
		Config: loadConfig(t),
		APIKey: apiKey,
	})
}

func get(t *testing.T, h http.Handler, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return v
}

func TestAuth(t *testing.T) {
	h := newServer(t, "secret-key", proxy.Stats{})
	for _, tc := range []struct {
		target, token string
		want          int
	}{
		{"/ui/api/providers", "", http.StatusUnauthorized},
		{"/ui/api/providers", "wrong", http.StatusUnauthorized},
		{"/ui/api/providers", "secret-key", http.StatusOK},
		{"/ui/api/requests", "", http.StatusUnauthorized},
		{"/ui/api/config", "", http.StatusUnauthorized},
		{"/ui/api/nope", "", http.StatusUnauthorized},
		{"/ui/api/nope", "secret-key", http.StatusNotFound},
		{"/ui/", "", http.StatusOK}, // static assets are public
	} {
		rec := get(t, h, tc.target, tc.token)
		if rec.Code != tc.want {
			t.Errorf("%s token %q: status %d, want %d", tc.target, tc.token, rec.Code, tc.want)
		}
		if strings.HasPrefix(tc.target, "/ui/api/") && rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: content type %q", tc.target, rec.Header().Get("Content-Type"))
		}
	}

	open := newServer(t, "", proxy.Stats{})
	if rec := get(t, open, "/ui/api/providers", ""); rec.Code != http.StatusOK {
		t.Errorf("no api key configured: status %d", rec.Code)
	}
}

func TestStatic(t *testing.T) {
	h := newServer(t, "", proxy.Stats{})
	for _, target := range []string{"/ui/", "/ui/requests/42", "/ui/index.html", "/ui/assets/missing.js"} {
		rec := get(t, h, target, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<div id="root">`) {
			t.Errorf("%s: status %d body %q, want index", target, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control %q", target, got)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /ui/: status %d", rec.Code)
	}
}

func TestRequests(t *testing.T) {
	h := newServer(t, "", proxy.Stats{})
	type page struct {
		Rows       []store.Request `json:"rows"`
		NextBefore *int64          `json:"next_before"`
	}
	ids := func(p page) []string {
		var out []string
		for _, r := range p.Rows {
			out = append(out, r.RequestID)
		}
		return out
	}

	// Cursor pagination walks newest to oldest.
	var seen []string
	target := "/ui/api/requests?limit=2"
	for range 5 {
		p := decode[page](t, get(t, h, target, ""))
		seen = append(seen, ids(p)...)
		if p.NextBefore == nil {
			break
		}
		target = fmt.Sprintf("/ui/api/requests?limit=2&before=%d", *p.NextBefore)
	}
	if got := strings.Join(seen, ","); got != "req-4,req-3,req-2,req-1,req-0" {
		t.Errorf("paged rows %s", got)
	}

	since := time.Now().Add(-35 * time.Minute)
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"", "req-4,req-3,req-2,req-1,req-0"},
		{"status=5xx", "req-1"},
		{"status=2xx&provider=alpha", "req-2,req-0"},
		{"protocol=chat", "req-3,req-1"},
		{"errors=1", "req-4,req-2,req-1"},
		{"outcome=upstream_error_event", "req-2"},
		{"model=HAIKU", "req-4"},
		{"route=*&model=claude", "req-4,req-2,req-0"},
		{fmt.Sprintf("since=%d", since.UnixMilli()), "req-4,req-3,req-2"},
		{"since=" + since.UTC().Format(time.RFC3339), "req-4,req-3,req-2"},
		{"until=" + since.UTC().Format(time.RFC3339), "req-1,req-0"},
	} {
		p := decode[page](t, get(t, h, "/ui/api/requests?"+tc.query, ""))
		if got := strings.Join(ids(p), ","); got != tc.want {
			t.Errorf("%q: got %s, want %s", tc.query, got, tc.want)
		}
		if p.NextBefore != nil {
			t.Errorf("%q: next_before %d on a partial page", tc.query, *p.NextBefore)
		}
	}

	// A live tail reads forward, oldest first, and has no older page.
	all := decode[page](t, get(t, h, "/ui/api/requests", ""))
	p := decode[page](t, get(t, h, fmt.Sprintf("/ui/api/requests?after=%d&limit=2", all.Rows[2].ID), ""))
	if got := strings.Join(ids(p), ","); got != "req-3,req-4" || p.NextBefore != nil {
		t.Errorf("after: got %s next_before %v", got, p.NextBefore)
	}

	// An empty result is an empty array, not null.
	if rec := get(t, h, "/ui/api/requests?provider=nobody", ""); !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Errorf("empty page: %s", rec.Body)
	}

	for _, q := range []string{"status=6xx", "since=yesterday", "errors=maybe", "before=x", "limit=0"} {
		if rec := get(t, h, "/ui/api/requests?"+q, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", q, rec.Code)
		}
	}
}

func TestRequestDetail(t *testing.T) {
	h := newServer(t, "", proxy.Stats{})
	all := decode[struct {
		Rows []store.Request `json:"rows"`
	}](t, get(t, h, "/ui/api/requests", ""))
	id := all.Rows[0].ID

	rec := get(t, h, fmt.Sprintf("/ui/api/requests/%d", id), "")
	var raw map[string]any
	if err := json.Unmarshal(decode[json.RawMessage](t, rec), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "ts", "request_id", "client_model", "routing_json", "attempt_log", "input_tokens", "cache_read_tokens", "cost_usd", "price_json"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("row has no %q: %s", k, rec.Body)
		}
	}
	if raw["request_id"] != "req-4" || raw["cache_read_tokens"] != nil {
		t.Errorf("row %s", rec.Body)
	}

	for _, target := range []string{"/ui/api/requests/999999", "/ui/api/requests/abc"} {
		if rec := get(t, h, target, ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s: status %d body %s", target, rec.Code, rec.Body)
		}
	}
}

func TestUsage(t *testing.T) {
	h := newServer(t, "", proxy.Stats{})
	rec := get(t, h, "/ui/api/usage?by=provider", "")
	v := decode[usageView](t, rec)
	if len(v.Rows) != 2 || v.Rows[0].Key != "alpha" {
		t.Errorf("rows %+v", v.Rows)
	}
	if v.Total.Requests != 5 || v.Total.Input != 500 || v.Total.CostUSD != 0.75 || v.Total.Unpriced != 3 {
		t.Errorf("total %+v", v.Total)
	}
	if len(v.Daily) == 0 {
		t.Error("daily series is empty")
	}
	if len(v.UnpricedModels) != 2 || v.UnpricedModels[0] != (store.ModelCount{Model: "gpt-x", Count: 2}) {
		t.Errorf("unpriced %+v", v.UnpricedModels)
	}
	for _, k := range []string{`"rows":`, `"total":`, `"unpriced_models":`, `"daily":`, `"input_tokens":500`, `"no_usage":0`} {
		if !strings.Contains(rec.Body.String(), k) {
			t.Errorf("response lacks %s: %s", k, rec.Body)
		}
	}

	day := decode[usageView](t, get(t, h, "/ui/api/usage?by=day&model=claude-opus", ""))
	if day.Total.Requests != 2 || len(day.Daily) != len(day.Rows) || len(day.UnpricedModels) != 0 {
		t.Errorf("by day %+v", day)
	}
	if rec := get(t, h, "/ui/api/usage?by=hour", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("by=hour: status %d", rec.Code)
	}
}

func TestProviders(t *testing.T) {
	openUntil := time.Now().Add(20 * time.Second).Truncate(time.Second)
	h := newServer(t, "", proxy.Stats{
		Breakers: map[string]route.State{
			"alpha.anthropic": {State: "open", Failures: 3, OpenUntil: openUntil},
			"beta.chat":       {State: "closed", Failures: 1},
		},
		Pins:       map[string]int{"alpha.anthropic": 4},
		PinsTotal:  4,
		ModelsAge:  map[string]time.Duration{"beta.chat": 90 * time.Second},
		LogDropped: 7,
	})
	rec := get(t, h, "/ui/api/providers", "")
	v := decode[ProvidersView](t, rec)
	if v.LogDropped != 7 || v.PinsTotal != 4 || len(v.Endpoints) != 4 {
		t.Fatalf("%+v", v)
	}
	alphaAnth, alphaChat, beta, gamma := v.Endpoints[0], v.Endpoints[1], v.Endpoints[2], v.Endpoints[3]
	if alphaAnth.Provider != "alpha" || alphaAnth.Protocol != "anthropic" || alphaAnth.Breaker.State != "open" ||
		alphaAnth.Breaker.OpenUntil == nil || !alphaAnth.Breaker.OpenUntil.Equal(openUntil) ||
		alphaAnth.Pins != 4 || alphaAnth.Committed != 3 || alphaAnth.Failover != 0 || alphaAnth.ErrorEvents != 1 || alphaAnth.ModelsAgeS != nil {
		t.Errorf("alpha anthropic %+v", alphaAnth)
	}
	if alphaAnth.BaseURL != "https://alpha.example/anthropic" {
		t.Errorf("alpha base_url %q not redacted", alphaAnth.BaseURL)
	}
	// alpha's chat endpoint failed over once and is counted apart from its
	// anthropic endpoint.
	if alphaChat.Provider != "alpha" || alphaChat.Protocol != "chat" || alphaChat.Breaker.State != "closed" ||
		alphaChat.Failover != 1 || alphaChat.Committed != 0 || alphaChat.Pins != 0 {
		t.Errorf("alpha chat %+v", alphaChat)
	}
	if beta.Breaker.State != "closed" || beta.Breaker.Failures != 1 || beta.Breaker.OpenUntil != nil ||
		beta.ModelsAgeS == nil || *beta.ModelsAgeS != 90 || beta.Committed != 2 || beta.Failover != 0 {
		t.Errorf("beta %+v", beta)
	}
	// An endpoint with no live state at all still appears.
	if gamma.Provider != "gamma" || gamma.Protocol != "responses" || gamma.Breaker.State != "closed" || gamma.Pins != 0 || gamma.ModelsAgeS != nil || gamma.Committed != 0 {
		t.Errorf("gamma %+v", gamma)
	}
	if !strings.Contains(rec.Body.String(), `"models_age_s":null`) || !strings.Contains(rec.Body.String(), `"open_until":null`) {
		t.Errorf("missing values are not null: %s", rec.Body)
	}
}

func TestConfigRedacted(t *testing.T) {
	h := newServer(t, "secret-key", proxy.Stats{})
	rec := get(t, h, "/ui/api/config", "secret-key")
	body := rec.Body.String()
	for _, secret := range []string{"secret-key", "alpha-key", "beta-key", "hidden-value", "user:pass"} {
		if strings.Contains(body, secret) {
			t.Errorf("config leaks %q: %s", secret, body)
		}
	}
	v := decode[map[string]any](t, rec)
	server := v["server"].(map[string]any)
	if server["api_key"] != "***" || server["header_timeout"] != "1m0s" {
		t.Errorf("server %v", server)
	}
	alpha := v["providers"].(map[string]any)["alpha"].(map[string]any)
	headers := alpha["headers"].(map[string]any)
	if alpha["key"] != "***" || headers["x-title"] != "***" || headers["x-drop"] != nil {
		t.Errorf("alpha %v", alpha)
	}
	if _, ok := headers["x-drop"]; !ok {
		t.Errorf("a removing header must stay visible as null: %v", headers)
	}
	if v["log"].(map[string]any)["retention"] != "90d" {
		t.Errorf("log %v", v["log"])
	}
}

func TestPricingSync(t *testing.T) {
	file := `{"models": {"m1": {"cost": {"input": 1, "output": 2}}}}`
	started, release := make(chan struct{}), make(chan struct{})
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.json":
			fmt.Fprint(w, file)
		case "/slow.json":
			close(started)
			<-release
			fmt.Fprint(w, file)
		default:
			http.NotFound(w, r)
		}
	}))
	defer src.Close()

	newSync := func(sources ...string) (http.Handler, **pricing.Table) {
		cfg := loadConfig(t)
		cfg.Pricing = config.Pricing{Sources: sources, Cache: t.TempDir()}
		var installed *pricing.Table
		reload := func() (*pricing.Table, []string, error) {
			tbl, warnings, err := pricing.Load(cfg.Pricing.Cache, cfg.Pricing.Sources, "", nil)
			installed = tbl
			return tbl, warnings, err
		}
		h := New(Options{Store: newStore(t, nil), Stats: func() proxy.Stats { return proxy.Stats{} }, Config: cfg, ReloadPricing: reload})
		return h, &installed
	}
	post := func(h http.Handler, header ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/ui/api/pricing/sync", nil)
		for i := 0; i < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	h, installed := newSync(src.URL + "/ok.json")
	if rec := post(h, "Sec-Fetch-Site", "cross-site"); rec.Code != http.StatusForbidden || *installed != nil {
		t.Errorf("cross-site: status %d, installed %v", rec.Code, *installed != nil)
	}
	v := decode[pricingSyncView](t, post(h, "Sec-Fetch-Site", "same-origin"))
	if v.Models != 1 || v.Warnings == nil {
		t.Errorf("sync %+v", v)
	}
	if *installed == nil {
		t.Fatal("no table installed")
	}
	if _, _, ok := (*installed).Lookup("m1", ""); !ok {
		t.Error("installed table lacks m1")
	}

	h, installed = newSync(src.URL+"/ok.json", src.URL+"/missing.json")
	if rec := post(h); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "missing.json") || *installed != nil {
		t.Errorf("failed source: status %d body %s", rec.Code, rec.Body)
	}
	h, _ = newSync()
	if rec := post(h); rec.Code != http.StatusBadRequest {
		t.Errorf("no sources: status %d", rec.Code)
	}

	h, _ = newSync(src.URL + "/slow.json")
	first := make(chan int)
	go func() { first <- post(h).Code }()
	<-started
	if rec := post(h); rec.Code != http.StatusConflict {
		t.Errorf("sync while one runs: status %d", rec.Code)
	}
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Errorf("first sync: status %d", code)
	}
}
