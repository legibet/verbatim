package models

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"verbatim/internal/config"
	"verbatim/internal/protocol"
)

// provider serves GET /models with the pages given, keyed by the after_id
// query parameter, and records the headers of the last request.
type provider struct {
	*httptest.Server
	header atomic.Pointer[http.Header]
	fail   atomic.Bool
	hits   atomic.Int64 // requests served
}

func newProvider(t *testing.T, pages map[string]string) *provider {
	t.Helper()
	p := &provider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
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
	prefixed := newProvider(t, map[string]string{"": openai("openai/gpt-7", "openai/gpt-5", "openai/gpt-4x", "openai/o3", "other/gpt-8")})
	rewrite := newProvider(t, nil) // must never be asked
	unused := newProvider(t, nil)

	cfg, _, err := config.Parse([]byte(fmt.Sprintf(`
providers:
  multi:
    key: km
    endpoints:
      chat: {base_url: %s}
      anthropic: {base_url: %s}
  prefixed: {key: kp, endpoints: {chat: {base_url: %s}}}
  rewrite: {key: kr, headers: {x-extra: "1", x-drop: null}, endpoints: {chat: {base_url: %s}}}
  unused: {key: ku, endpoints: {chat: {base_url: %s}}}
routes:
  - model: kimi-k3
    targets: [{provider: unused}]
  - model: "gpt-4*"
    discover: false
    targets: [{provider: unused}]
  - model: "gpt-*"
    targets: [{provider: multi}, {provider: prefixed, model: "openai/gpt-*"}, {provider: rewrite, model: fixed}]
  - model: "claude-*"
    targets: [{provider: multi.anthropic}, {provider: rewrite, model: fixed}]
  - model: "*"
    discover: false
    targets: [{provider: multi.chat}]
`, chat.URL, anth.URL, prefixed.URL, rewrite.URL, unused.URL)), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, nil, time.Minute, time.Second)
	var fetched []string
	for _, e := range c.endpoints {
		fetched = append(fetched, e.name())
	}
	if want := []string{"multi.anthropic", "multi.chat", "prefixed.chat"}; !slices.Equal(fetched, want) {
		t.Fatalf("endpoints to fetch = %v, want %v", fetched, want)
	}
	if got := names(c); !slices.Equal(got, []string{"kimi-k3"}) {
		t.Errorf("before any fetch: %v", got)
	}
	for _, e := range c.endpoints {
		if err := c.refresh(context.Background(), e); err != nil {
			t.Fatalf("%s: %v", e.name(), err)
		}
	}

	// A provider target reads every endpoint's list, in protocol order:
	// multi's anthropic list gives gpt-6 under gpt-*, its chat list gpt-5.
	// gpt-4o belongs to the earlier gpt-4* route; o3 matches no pattern;
	// prefixed's ids map back through openai/gpt-* to gpt-7 and gpt-5 (already
	// listed) and gpt-4x (owned by gpt-4*); the others do not match it.
	want := []string{"kimi-k3", "gpt-6", "gpt-5", "gpt-7", "claude-a", "claude-b", "claude-c"}
	if got := names(c); !slices.Equal(got, want) {
		t.Errorf("Names() = %v\nwant %v", got, want)
	}
	// Each name carries the protocols whose lists name it and their routes.
	for _, l := range c.Names(context.Background()) {
		var want []Served
		switch l.Name {
		case "kimi-k3":
			want = []Served{{protocol.Chat, "kimi-k3"}}
		case "gpt-6":
			want = []Served{{protocol.Anthropic, "gpt-*"}}
		case "gpt-5":
			want = []Served{{protocol.Chat, "gpt-*"}}
		default:
			continue
		}
		if !slices.Equal(l.Served, want) {
			t.Errorf("Names() %s: %+v, want %+v", l.Name, l.Served, want)
		}
	}
	if rewrite.header.Load() != nil || unused.header.Load() != nil {
		t.Error("a provider discovery does not read was fetched")
	}

	h := *chat.header.Load()
	if h.Get("Authorization") != "Bearer km" {
		t.Errorf("chat auth: %v", h)
	}
	h = *anth.header.Load()
	if h.Get("X-Api-Key") != "km" || h.Get("Anthropic-Version") != "2023-06-01" || h.Get("Authorization") != "" {
		t.Errorf("anthropic headers: %v", h)
	}
	states := c.States()
	if s := states["multi.chat"]; len(states) != 3 || s.Fetched.IsZero() || s.Err != "" {
		t.Errorf("States() = %v", states)
	}

	// A failed refresh keeps the previous list and records the error; the
	// next success clears it.
	chat.fail.Store(true)
	c.Refresh(context.Background())
	if got := names(c); !slices.Equal(got, want) {
		t.Errorf("after a failed refresh: %v", got)
	}
	if s := c.States()["multi.chat"]; s.Fetched.IsZero() || !strings.Contains(s.Err, "HTTP 500") {
		t.Errorf("after a failed refresh: %+v", s)
	}
	chat.fail.Store(false)
	c.Refresh(context.Background())
	if s := c.States()["multi.chat"]; s.Err != "" {
		t.Errorf("after a successful refresh: %+v", s)
	}
}

func TestStartWarms(t *testing.T) {
	p := newProvider(t, map[string]string{"": openai("m-1")})
	cfg, _, err := config.Parse([]byte(fmt.Sprintf(`
providers:
  p: {key: k, endpoints: {chat: {base_url: %s}}}
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
	if got := names(c); !slices.Equal(got, []string{"m-1"}) {
		t.Errorf("Names() = %v", got)
	}
}

func TestNamesSkipsEmptyName(t *testing.T) {
	// Under "*" with model "local/*", the listed "local/" maps back to "".
	p := newProvider(t, map[string]string{"": openai("local/", "local/llama3")})
	cfg, _, err := config.Parse([]byte(fmt.Sprintf(`
providers:
  p: {key: k, endpoints: {chat: {base_url: %s}}}
routes:
  - model: "*"
    targets: [{provider: p, model: "local/*"}]
`, p.URL)), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, nil, time.Minute, time.Second)
	if err := c.refresh(context.Background(), endpoint{"p", "chat"}); err != nil {
		t.Fatal(err)
	}
	if got := names(c); !slices.Equal(got, []string{"llama3"}) {
		t.Errorf("Names() = %q", got)
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
  p: {key: k, headers: {Accept-Encoding: gzip}, endpoints: {chat: {base_url: %s}}}
routes:
  - model: "m-*"
    targets: [{provider: p}]
`, p.URL)), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, nil, time.Minute, time.Second)
	if err := c.refresh(context.Background(), endpoint{"p", "chat"}); err != nil {
		t.Fatal(err)
	}
	if got := names(c); !slices.Equal(got, []string{"m-1", "m-2"}) {
		t.Errorf("Names() = %v", got)
	}
	if got := asked.Load(); got != "gzip" {
		t.Errorf("provider was asked for Accept-Encoding %q, want the transport's gzip", got)
	}
}

func TestAuthorize(t *testing.T) {
	v := "set"
	h := http.Header{"X-Drop": {"1"}, "Authorization": {"Bearer client"}}
	Authorize(h, config.Provider{Key: "k", Headers: map[string]*string{"x-drop": nil, "x-api-key": &v, "x-new": &v}}, config.Endpoint{Auth: "x-api-key"})
	if h.Get("X-Drop") != "" || h.Get("X-New") != "set" || h.Get("X-Api-Key") != "k" {
		t.Errorf("headers = %v", h)
	}
	h = http.Header{}
	Authorize(h, config.Provider{Key: "k"}, config.Endpoint{Auth: "none"})
	if len(h) != 0 {
		t.Errorf("auth none set %v", h)
	}
}

// A failing endpoint warns when its fetches start failing, not on every retry.
func TestFailureWarnsOnce(t *testing.T) {
	p := newProvider(t, map[string]string{"": openai("m-1")})
	p.fail.Store(true)
	cfg, _, err := config.Parse([]byte(fmt.Sprintf(`
providers:
  p: {key: k, endpoints: {chat: {base_url: %s}}}
routes:
  - model: "m-*"
    targets: [{provider: p}]
`, p.URL)), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A TTL this short retries a failed fetch every 5ms.
	c := New(cfg, nil, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// await waits until the provider has served n more requests. A fetch logs
	// before the next one starts, so every fetch but the last has logged.
	await := func(n int64) {
		t.Helper()
		want := p.hits.Load() + n
		for deadline := time.Now().Add(5 * time.Second); p.hits.Load() < want; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the provider served %d of %d requests", p.hits.Load(), want)
			}
		}
	}
	c.Start(ctx)
	await(10)
	p.fail.Store(false)
	for deadline := time.Now().Add(5 * time.Second); len(names(c)) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the list was never fetched")
		}
	}
	p.fail.Store(true)
	await(10)
	cancel()

	out := logs.String()
	if warns, infos := strings.Count(out, "level=WARN"), strings.Count(out, "level=INFO"); warns != 2 || infos != 1 {
		t.Errorf("%d warnings and %d info lines, want 2 and 1:\n%s", warns, infos, out)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// names returns the listed names alone.
func names(c *Cache) []string {
	var out []string
	for _, l := range c.Names(context.Background()) {
		out = append(out, l.Name)
	}
	return out
}
