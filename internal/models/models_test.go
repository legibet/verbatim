package models

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"verbatim/internal/config"
)

// provider serves GET /models with the pages given, keyed by the after_id
// query parameter, and records the headers of the last request.
type provider struct {
	*httptest.Server
	header atomic.Pointer[http.Header]
	fail   atomic.Bool
}

func newProvider(t *testing.T, pages map[string]string) *provider {
	t.Helper()
	p := &provider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		p.header.Store(&h)
		body, ok := pages[r.URL.Query().Get("after_id")]
		if p.fail.Load() || !strings.HasSuffix(r.URL.Path, "/models") || !ok {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(p.Close)
	return p
}

func openai(ids ...string) string {
	s := `{"object":"list","data":[`
	for i, id := range ids {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf(`{"id":%q,"object":"model"}`, id)
	}
	return s + "]}"
}

func TestNamesAndFetch(t *testing.T) {
	chat := newProvider(t, map[string]string{"": openai("gpt-4o", "gpt-5", "o3", "gpt-5", "kimi-k3")})
	anth := newProvider(t, map[string]string{
		"":         `{"data":[{"id":"claude-a"},{"id":"claude-b"}],"has_more":true,"last_id":"claude-b"}`,
		"claude-b": `{"data":[{"id":"claude-c"},{"id":"gpt-6"}],"has_more":false,"last_id":"gpt-6"}`,
	})
	gem := newProvider(t, map[string]string{"": `{"models":[{"name":"models/gemini-x"},{"name":"models/other"}]}`})
	rewrite := newProvider(t, nil) // must never be asked
	unused := newProvider(t, nil)

	cfg, _, err := config.Parse([]byte(fmt.Sprintf(`
providers:
  chat: {type: chat, base_url: %s, key: kc}
  anth: {type: anthropic, base_url: %s, key: ka}
  gem: {type: gemini, base_url: %s, key: kg}
  rewrite: {type: chat, base_url: %s, key: kr, headers: {x-extra: "1", x-drop: null}}
  unused: {type: chat, base_url: %s, key: ku}
routes:
  - model: kimi-k3
    targets: [{provider: unused}]
  - model: "gpt-4*"
    discover: false
    targets: [{provider: unused}]
  - model: "gpt-*"
    targets: [{provider: chat}, {provider: rewrite, model: fixed}, {provider: anth}]
  - model: "claude-*"
    targets: [{provider: anth}, {provider: rewrite, model: fixed}]
  - model: "gemini-*"
    targets: [{provider: gem}]
  - model: "*"
    discover: false
    targets: [{provider: chat}]
`, chat.URL, anth.URL, gem.URL, rewrite.URL, unused.URL)), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, nil, time.Minute, time.Second)
	if want := []string{"anth", "chat", "gem"}; !slices.Equal(c.providers, want) {
		t.Fatalf("providers to fetch = %v, want %v", c.providers, want)
	}
	if got := c.Names(context.Background()); !slices.Equal(got, []string{"kimi-k3"}) {
		t.Errorf("before any fetch: %v", got)
	}
	for _, name := range c.providers {
		if err := c.refresh(context.Background(), name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// gpt-4o belongs to the earlier gpt-4* route; o3 matches no pattern;
	// anth's gpt-6 is listed under gpt-* because anth is a target there too;
	// gemini ids lose their models/ prefix.
	want := []string{"kimi-k3", "gpt-5", "gpt-6", "claude-a", "claude-b", "claude-c", "gemini-x"}
	if got := c.Names(context.Background()); !slices.Equal(got, want) {
		t.Errorf("Names() = %v\nwant %v", got, want)
	}
	if rewrite.header.Load() != nil || unused.header.Load() != nil {
		t.Error("a provider discovery does not read was fetched")
	}

	h := *chat.header.Load()
	if h.Get("Authorization") != "Bearer kc" {
		t.Errorf("chat auth: %v", h)
	}
	h = *anth.header.Load()
	if h.Get("X-Api-Key") != "ka" || h.Get("Anthropic-Version") != "2023-06-01" || h.Get("Authorization") != "" {
		t.Errorf("anthropic headers: %v", h)
	}
	if h := *gem.header.Load(); h.Get("X-Goog-Api-Key") != "kg" {
		t.Errorf("gemini headers: %v", h)
	}
	if ages := c.Ages(); len(ages) != 3 || ages["chat"] < 0 || ages["chat"] > time.Minute {
		t.Errorf("Ages() = %v", ages)
	}

	// A failed refresh keeps the previous list.
	chat.fail.Store(true)
	if err := c.refresh(context.Background(), "chat"); err == nil {
		t.Error("refresh of a failing provider succeeded")
	}
	if got := c.Names(context.Background()); !slices.Equal(got, want) {
		t.Errorf("after a failed refresh: %v", got)
	}
}

func TestStartWarms(t *testing.T) {
	p := newProvider(t, map[string]string{"": openai("m-1")})
	cfg, _, err := config.Parse([]byte(fmt.Sprintf(`
providers:
  p: {type: chat, base_url: %s, key: k}
routes:
  - model: "m-*"
    targets: [{provider: p}]
`, p.URL)), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, nil, time.Minute, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	// Names called right after Start waits for the first fetch.
	if got := c.Names(context.Background()); !slices.Equal(got, []string{"m-1"}) {
		t.Errorf("Names() = %v", got)
	}
}

// A provider's configured Accept-Encoding must not leave discovery with
// compressed bytes: the transport negotiates and decodes the list itself.
func TestFetchDecompresses(t *testing.T) {
	var asked atomic.Value
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Store(r.Header.Get("Accept-Encoding"))
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			io.WriteString(w, openai("m-1", "m-2"))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		io.WriteString(zw, openai("m-1", "m-2"))
		zw.Close()
	}))
	t.Cleanup(p.Close)
	cfg, _, err := config.Parse([]byte(fmt.Sprintf(`
providers:
  p: {type: chat, base_url: %s, key: k, headers: {Accept-Encoding: gzip}}
routes:
  - model: "m-*"
    targets: [{provider: p}]
`, p.URL)), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, nil, time.Minute, time.Second)
	if err := c.refresh(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if got := c.Names(context.Background()); !slices.Equal(got, []string{"m-1", "m-2"}) {
		t.Errorf("Names() = %v", got)
	}
	if got := asked.Load(); got != "gzip" {
		t.Errorf("provider was asked for Accept-Encoding %q, want the transport's gzip", got)
	}
}

func TestAuthorize(t *testing.T) {
	v := "set"
	h := http.Header{"X-Drop": {"1"}, "Authorization": {"Bearer client"}}
	Authorize(h, config.Provider{Auth: "x-api-key", Key: "k", Headers: map[string]*string{"x-drop": nil, "x-api-key": &v, "x-new": &v}})
	if h.Get("X-Drop") != "" || h.Get("X-New") != "set" || h.Get("X-Api-Key") != "k" {
		t.Errorf("headers = %v", h)
	}
	h = http.Header{}
	Authorize(h, config.Provider{Auth: "none", Key: "k"})
	if len(h) != 0 {
		t.Errorf("auth none set %v", h)
	}
}
