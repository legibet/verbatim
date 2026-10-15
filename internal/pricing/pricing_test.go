package pricing

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"verbatim/internal/protocol"
)

var realSources = []string{
	"https://basellm.github.io/llm-metadata/api/providers/anthropic.json",
	"https://basellm.github.io/llm-metadata/api/providers/openai.json",
}

func n(v int64) *int64 { return &v }

func loadReal(t *testing.T, overrides string, aliases map[string]string) (*Table, []string) {
	t.Helper()
	tab, warnings, err := Load("testdata", realSources, overrides, aliases)
	if err != nil {
		t.Fatal(err)
	}
	return tab, warnings
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestLoadReal(t *testing.T) {
	tab, warnings := loadReal(t, "", nil)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	models := tab.Models()
	if !slices.IsSorted(models) || !slices.Contains(models, "claude-opus-5-5") || !slices.Contains(models, "gpt-5.5") {
		t.Fatalf("models = %v", models)
	}
	if slices.Contains(models, "gpt-image-1") {
		t.Fatal("model with null cost was loaded")
	}
	_, p, ok := tab.Lookup("claude-opus-5-5", "")
	if !ok || p.Source != "anthropic" || p.Input != 4 || *p.CacheWrite1hMultiple != 2 {
		t.Fatalf("claude-opus-5-5 = %+v", p)
	}
	_, p, _ = tab.Lookup("gpt-5.5", "")
	if p.Source != "openai" || p.CacheWrite != nil || p.CacheWrite1hMultiple != nil || len(p.Tiers) != 1 || p.Tiers[0].Size != 272000 {
		t.Fatalf("gpt-5.5 = %+v", p)
	}
}

func TestLookup(t *testing.T) {
	tab, _ := loadReal(t, "", map[string]string{
		"opus-dated":     "claude-opus-5-5",
		"relay-gpt":      "gpt-5.5",
		"dead-alias":     "no-such-model",
		"claude-fable-5": "gpt-4", // the exact name wins over its alias
	})
	tests := []struct {
		client, upstream string
		want             string
	}{
		{"claude-opus-5-5", "gpt-5.5", "claude-opus-5-5"}, // client exact first
		{"opus-dated", "gpt-5.5", "claude-opus-5-5"},      // client alias before upstream
		{"unknown", "gpt-5.5", "gpt-5.5"},                 // upstream exact
		{"unknown", "relay-gpt", "gpt-5.5"},               // upstream alias
		{"dead-alias", "gpt-4", "gpt-4"},                  // alias to nothing falls through
		{"claude-fable-5", "", "claude-fable-5"},
		{"claude-opus-5-5-20260301", "", ""}, // no prefix or fuzzy matching
		{"unknown", "", ""},
	}
	for _, tt := range tests {
		key, _, ok := tab.Lookup(tt.client, tt.upstream)
		if key != tt.want || ok != (tt.want != "") {
			t.Errorf("Lookup(%q, %q) = %q, %v; want %q", tt.client, tt.upstream, key, ok, tt.want)
		}
	}
}

func TestCost(t *testing.T) {
	tab, _ := loadReal(t, "", nil)
	price := func(m string) Price {
		_, p, ok := tab.Lookup(m, "")
		if !ok {
			t.Fatalf("%s not priced", m)
		}
		return p
	}
	tests := []struct {
		name  string
		model string
		u     protocol.Usage
		want  float64
		units string
	}{
		{
			// 1h writes cost input * 2; the rest of the writes cost cache_write.
			name: "anthropic with 1h writes", model: "claude-opus-5-5",
			u:     protocol.Usage{Input: n(1000), CacheRead: n(10000), CacheWrite: n(3000), CacheWrite1h: n(1000), Output: n(500), Reasoning: n(100)},
			want:  (1000*4 + 10000*0.2 + 2000*5 + 1000*8 + 500*20) / 1e6,
			units: `{"source":"anthropic","input":4,"cache_read":0.2,"cache_write":5,"cache_write_1h":8,"output":20}`,
		},
		{
			name: "null 1h writes count as 0", model: "claude-opus-5-5",
			u:    protocol.Usage{Input: n(1000), CacheWrite: n(3000), Output: n(500)},
			want: (1000*4 + 3000*5 + 500*20) / 1e6,
		},
		{
			name: "haiku below tier", model: "claude-haiku-5-5",
			u:    protocol.Usage{Input: n(50000), CacheRead: n(50000), Output: n(1000)},
			want: (50000*0.1 + 50000*0.01 + 1000*0.5) / 1e6,
		},
		{
			// context = input + cache read + cache write = 100001 > 100000.
			name: "haiku above tier, 1h multiple uses tier input", model: "claude-haiku-5-5",
			u:     protocol.Usage{Input: n(1), CacheRead: n(90000), CacheWrite: n(10000), CacheWrite1h: n(10000), Output: n(1000)},
			want:  (1*0.5 + 90000*0.05 + 10000*1.0 + 1000*2.5) / 1e6,
			units: `{"source":"anthropic","tier_size":100000,"input":0.5,"cache_read":0.05,"cache_write":0.625,"cache_write_1h":1,"output":2.5}`,
		},
		{
			// No cache_write price: writes are priced at input.
			name: "gpt-5.5 below tier, cache write at input", model: "gpt-5.5",
			u:     protocol.Usage{Input: n(1000), CacheRead: n(2000), CacheWrite: n(500), Output: n(100)},
			want:  (1000*5 + 2000*0.5 + 500*5 + 100*30) / 1e6,
			units: `{"source":"openai","input":5,"cache_read":0.5,"cache_write":5,"cache_write_1h":5,"output":30}`,
		},
		{
			name: "gpt-5.5 above tier", model: "gpt-5.5",
			u:     protocol.Usage{Input: n(200000), CacheRead: n(100000), CacheWrite: n(1000), Output: n(1000)},
			want:  (200000*10 + 100000*1 + 1000*10 + 1000*45) / 1e6,
			units: `{"source":"openai","tier_size":272000,"input":10,"cache_read":1,"cache_write":10,"cache_write_1h":10,"output":45}`,
		},
		{
			name: "exactly at the bound is below the tier", model: "gpt-5.5",
			u:    protocol.Usage{Input: n(272000), Output: n(0)},
			want: 272000 * 5 / 1e6,
		},
		{
			// gpt-5.4-pro has no cache_read price anywhere; without cache
			// reads the missing price does not matter.
			name: "tier inherits, no cache reads without a cache read price", model: "gpt-5.4-pro",
			u:     protocol.Usage{Input: n(272001), Output: n(10)},
			want:  (272001*60 + 10*270) / 1e6,
			units: `{"source":"openai","tier_size":272000,"input":60,"cache_read":null,"cache_write":60,"cache_write_1h":60,"output":270}`,
		},
		{
			name: "zero cache reads without a cache read price", model: "gpt-5.4-pro",
			u:    protocol.Usage{Input: n(1000), CacheRead: n(0), Output: n(10)},
			want: (1000*30 + 10*180) / 1e6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, units, ok := Cost(tt.u, price(tt.model))
			if !ok || !near(got, tt.want) {
				t.Fatalf("cost = %v, %v; want %v", got, ok, tt.want)
			}
			if tt.units != "" && string(units) != tt.units {
				t.Fatalf("price json = %s\nwant %s", units, tt.units)
			}
		})
	}

	for _, tt := range []struct {
		name  string
		model string
		u     protocol.Usage
	}{
		{"cache reads only", "gpt-5.5", protocol.Usage{CacheRead: n(5)}},
		{"output only", "gpt-5.5", protocol.Usage{Output: n(1000)}},
		{"input only", "gpt-5.5", protocol.Usage{Input: n(1000)}},
		{"cache reads without a cache read price", "gpt-5.4-pro", protocol.Usage{Input: n(1000), CacheRead: n(1), Output: n(10)}},
	} {
		if usd, _, ok := Cost(tt.u, price(tt.model)); ok {
			t.Errorf("%s: priced at %v, want no cost", tt.name, usd)
		}
	}
}

func writeJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

type m = map[string]any

func TestLoadMergeAndWarnings(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "a.json"), m{"id": "prov-a", "billing": m{"cacheWrite1h": 2}, "models": m{
		"shared":  m{"cost": m{"input": 1, "output": 2}},
		"yuan":    m{"cost": m{"input": 1, "output": 2, "currency": "CNY"}},
		"only-a":  m{"cost": m{"input": 3, "output": 4, "cache_write": 3.75}},
		"no-cost": m{"cost": nil},
	}})
	writeJSON(t, filepath.Join(dir, "b"+".json"), m{"id": "prov-b", "models": m{
		"shared": m{"cost": m{"input": 10, "output": 20, "currency": "USD"}},
	}})
	sources := []string{"https://x.example/p/a.json", "https://y.example/b"}

	tab, warnings, err := Load(dir, sources, filepath.Join(dir, "missing-overrides.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(warnings, "\n")
	if len(warnings) != 2 || !strings.Contains(joined, `"yuan"`) || !strings.Contains(joined, "CNY") ||
		!strings.Contains(joined, `"shared" is priced by both prov-a and prov-b`) {
		t.Fatalf("warnings = %q", warnings)
	}
	if _, p, _ := tab.Lookup("shared", ""); p.Input != 10 || p.Source != "prov-b" {
		t.Fatalf("later source did not win: %+v", p)
	}
	if _, _, ok := tab.Lookup("yuan", ""); ok {
		t.Fatal("non-USD model loaded")
	}

	// Overrides replace the whole cost, keep the source's 1h multiple, and may add models.
	ov := filepath.Join(dir, "overrides.json")
	writeJSON(t, ov, m{"models": m{
		"only-a": m{"cost": m{"input": 30, "output": 40}},
		"local":  m{"cost": m{"input": 0, "output": 0}},
	}})
	tab, _, err = Load(dir, sources, ov, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, p, _ := tab.Lookup("only-a", "")
	if p.Input != 30 || p.CacheWrite != nil || p.Source != "overrides" || p.CacheWrite1hMultiple == nil || *p.CacheWrite1hMultiple != 2 {
		t.Fatalf("override = %+v", p)
	}
	if _, p, ok := tab.Lookup("local", ""); !ok || p.CacheWrite1hMultiple != nil {
		t.Fatalf("added model = %+v, %v", p, ok)
	}

	// A source that was never synced is a warning, not an error.
	_, warnings, err = Load(dir, append(sources, "https://z.example/c.json"), "", nil)
	if err != nil || !strings.Contains(strings.Join(warnings, "\n"), "not synced") {
		t.Fatalf("unsynced source: %v, %q", err, warnings)
	}
	if _, _, err := Load(dir, []string{"https://x.example/a.json", "https://y.example/a.json"}, "", nil); err == nil {
		t.Fatal("sources sharing a cache file name accepted")
	}
	writeJSON(t, ov, "not an object")
	if _, _, err := Load(dir, sources, ov, nil); err == nil {
		t.Fatal("malformed overrides accepted")
	}
}

func TestSync(t *testing.T) {
	anthropic, err := os.ReadFile("testdata/anthropic.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/providers/anthropic.json":
			w.Write(anthropic)
		case "/providers/broken.json":
			w.Write([]byte(`<html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "cache")
	src := srv.URL + "/providers/anthropic.json"
	if err := Sync(context.Background(), []string{src}, dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "anthropic.json"))
	if err != nil || string(got) != string(anthropic) {
		t.Fatalf("cached file differs (err %v)", err)
	}
	tab, _, err := Load(dir, []string{src}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := tab.Lookup("claude-sonnet-5-5", ""); !ok {
		t.Fatal("synced file not loadable")
	}

	// A failing source aborts the sync before anything is written.
	before, _ := os.Stat(filepath.Join(dir, "anthropic.json"))
	for _, bad := range []string{"/providers/broken.json", "/providers/missing.json"} {
		if err := Sync(context.Background(), []string{src, srv.URL + bad}, dir); err == nil {
			t.Fatalf("sync with %s succeeded", bad)
		}
	}
	after, _ := os.Stat(filepath.Join(dir, "anthropic.json"))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("failed sync rewrote the cache")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("cache dir has leftovers: %v", entries)
	}
}

func TestWatch(t *testing.T) {
	dir := t.TempDir()
	sources := []string{"https://x.example/a.json"}
	file := filepath.Join(dir, "a.json")
	ov := filepath.Join(dir, "overrides.json")
	writeJSON(t, file, m{"id": "a", "models": m{"one": m{"cost": m{"input": 1, "output": 1}}}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tables := make(chan *Table, 4)
	go Watch(ctx, 10*time.Millisecond, dir, sources, ov, nil, func(t *Table) { tables <- t })

	select {
	case <-tables:
		t.Fatal("reload without a change")
	case <-time.After(50 * time.Millisecond):
	}

	writeJSON(t, file, m{"id": "a", "models": m{"one": m{"cost": m{"input": 1, "output": 1}}, "two": m{"cost": m{"input": 2, "output": 2}}}})
	os.Chtimes(file, time.Now().Add(time.Second), time.Now().Add(time.Second))
	select {
	case tab := <-tables:
		if len(tab.Models()) != 2 {
			t.Fatalf("reloaded models = %v", tab.Models())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("source change not detected")
	}

	writeJSON(t, ov, m{"models": m{"three": m{"cost": m{"input": 3, "output": 3}}}})
	select {
	case tab := <-tables:
		if len(tab.Models()) != 3 {
			t.Fatalf("reloaded models = %v", tab.Models())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("overrides change not detected")
	}
}
