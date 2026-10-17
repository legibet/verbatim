package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fullExample is the configuration from design section 6.1, unchanged.
const fullExample = `
server:
  listen: 127.0.0.1:8080
  api_key: ${VERBATIM_KEY}          # optional on loopback; required elsewhere
  max_body_bytes: 33554432
  max_inflight: 64
  header_timeout: 60s               # upstream response headers
  idle_timeout: 120s                # upstream body inactivity, also used for error bodies

providers:
  openai-responses:
    type: responses
    base_url: https://api.openai.com/v1
    key: ${OPENAI_KEY}
  openai-chat:
    type: chat
    base_url: https://api.openai.com/v1
    key: ${OPENAI_KEY}
  moonshot-anthropic:
    type: anthropic
    base_url: https://api.moonshot.ai/anthropic
    key: ${MOONSHOT_KEY}
  moonshot-chat:
    type: chat
    base_url: https://api.moonshot.ai/v1
    key: ${MOONSHOT_KEY}
  openrouter-anthropic:
    type: anthropic
    base_url: https://openrouter.ai/api/v1
    key: ${OPENROUTER_KEY}
    auth: bearer                     # OpenRouter's Messages endpoint takes Authorization: Bearer
  relay-a-chat:
    type: chat
    base_url: https://relay.example/v1
    key: ${RELAY_A_KEY}
    headers:
      x-title: verbatim
      openai-project: null           # drop a client header this relay rejects
    overrides:
      - delete: metadata
      - set: {path: temperature, value: 1}
  local-llama:
    type: chat
    base_url: http://127.0.0.1:8081/v1
    auth: none

routes:
  - model: kimi-k3
    targets:
      - provider: moonshot-anthropic
      - provider: moonshot-chat
        weight: 3
      - provider: relay-a-chat
        model: kimi-k3-0905
        weight: 1
      - provider: openrouter-anthropic
        model: moonshotai/kimi-k3
        priority: 2
  - model: "gpt-*"
    targets:
      - provider: openai-responses
      - provider: openai-chat
  - model: "*"
    discover: false
    targets:
      - provider: openrouter-anthropic
      - provider: local-llama

affinity:
  ttl: 1h
  mode: prefer
  rules:
    - match: {protocol: anthropic}
      key: [{body: metadata.user_id}, {header: x-session-affinity}]
    - match: {protocol: responses}
      key: [{header: session-id}, {body: prompt_cache_key}, {body: id}]
    - match: {protocol: chat}
      key: [{header: x-session-affinity}, {body: prompt_cache_key}, {body: user}]
  fallback: prefix_hash

breaker:
  failure_threshold: 3
  cooldown: 30s

models:
  cache_ttl: 10m
  timeout: 3s

log:
  path: ~/.verbatim/verbatim.db
  retention: 90d
  error_excerpt_bytes: 4096

pricing:
  sources:
    - https://basellm.github.io/llm-metadata/api/providers/anthropic.json
    - https://basellm.github.io/llm-metadata/api/providers/openai.json
    - https://basellm.github.io/llm-metadata/api/providers/moonshotai.json
  cache: ~/.verbatim/pricing/
  overrides: ~/.verbatim/pricing-overrides.json
  aliases:
    claude-opus-5-5-20260301: claude-opus-5-5
    kimi-k3: kimi-k3-0905
`

const minimal = `
providers:
  p:
    type: chat
    base_url: https://example.com/v1
    key: k
routes:
  - model: m
    targets:
      - provider: p
`

func home(t *testing.T) string {
	t.Helper()
	h, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustParse(t *testing.T, src string) (*Config, []Warning) {
	t.Helper()
	cfg, warns, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg, warns
}

func TestFullExample(t *testing.T) {
	for _, v := range []string{"VERBATIM_KEY", "OPENAI_KEY", "MOONSHOT_KEY", "OPENROUTER_KEY", "RELAY_A_KEY"} {
		t.Setenv(v, "secret-"+v)
	}
	cfg, warns := mustParse(t, fullExample)
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}

	if cfg.Server.APIKey != "secret-VERBATIM_KEY" {
		t.Errorf("api_key = %q", cfg.Server.APIKey)
	}
	if len(cfg.Providers) != 7 {
		t.Fatalf("providers = %d, want 7", len(cfg.Providers))
	}
	auths := map[string]string{
		"openai-responses":     "bearer",
		"openai-chat":          "bearer",
		"moonshot-anthropic":   "x-api-key",
		"openrouter-anthropic": "bearer",
		"local-llama":          "none",
	}
	for name, want := range auths {
		if got := cfg.Providers[name].Auth; got != want {
			t.Errorf("providers.%s.auth = %q, want %q", name, got, want)
		}
	}
	relay := cfg.Providers["relay-a-chat"]
	if relay.Key != "secret-RELAY_A_KEY" {
		t.Errorf("relay key = %q", relay.Key)
	}
	if v, ok := relay.Headers["openai-project"]; !ok || v != nil {
		t.Errorf("openai-project header = %v, %v; want present and nil", v, ok)
	}
	if v := relay.Headers["x-title"]; v == nil || *v != "verbatim" {
		t.Errorf("x-title header = %v", v)
	}
	wantOverrides := []Override{{Delete: "metadata"}, {Set: &SetOp{Path: "temperature", Value: 1}}}
	if !reflect.DeepEqual(relay.Overrides, wantOverrides) {
		t.Errorf("overrides = %#v", relay.Overrides)
	}

	if len(cfg.Routes) != 3 {
		t.Fatalf("routes = %d", len(cfg.Routes))
	}
	kimi := cfg.Routes[0]
	wantTargets := []Target{
		{Provider: "moonshot-anthropic", Priority: 1, Weight: 1},
		{Provider: "moonshot-chat", Priority: 1, Weight: 3},
		{Provider: "relay-a-chat", Model: "kimi-k3-0905", Priority: 1, Weight: 1},
		{Provider: "openrouter-anthropic", Model: "moonshotai/kimi-k3", Priority: 2, Weight: 1},
	}
	if !reflect.DeepEqual(kimi.Targets, wantTargets) {
		t.Errorf("kimi targets = %+v", kimi.Targets)
	}
	if kimi.Balance != "weighted" || !*kimi.Discover {
		t.Errorf("kimi balance/discover = %q/%v", kimi.Balance, *kimi.Discover)
	}
	if *cfg.Routes[2].Discover {
		t.Error(`route "*" discover = true, want false`)
	}

	if got := cfg.Affinity.Rules[2].Key; !reflect.DeepEqual(got, []KeySource{{Header: "x-session-affinity"}, {Body: "prompt_cache_key"}, {Body: "user"}}) {
		t.Errorf("chat rule key = %+v", got)
	}
	if cfg.Log.Retention.Std() != 90*24*time.Hour {
		t.Errorf("retention = %v", cfg.Log.Retention)
	}
	h := home(t)
	if cfg.Log.Path != filepath.Join(h, ".verbatim/verbatim.db") {
		t.Errorf("log.path = %q", cfg.Log.Path)
	}
	if cfg.Pricing.Overrides != filepath.Join(h, ".verbatim/pricing-overrides.json") {
		t.Errorf("pricing.overrides = %q", cfg.Pricing.Overrides)
	}
	if len(cfg.Pricing.Sources) != 3 || cfg.Pricing.Aliases["kimi-k3"] != "kimi-k3-0905" {
		t.Errorf("pricing = %+v", cfg.Pricing)
	}
}

func TestDefaults(t *testing.T) {
	cfg, warns := mustParse(t, minimal)
	if len(warns) != 0 {
		t.Errorf("warnings = %v", warns)
	}
	h := home(t)
	want := Config{
		Server: Server{
			Listen:        "127.0.0.1:8080",
			MaxBodyBytes:  32 << 20,
			MaxInflight:   64,
			HeaderTimeout: Duration(60 * time.Second),
			IdleTimeout:   Duration(120 * time.Second),
		},
		Providers: map[string]Provider{"p": {Type: "chat", BaseURL: "https://example.com/v1", Key: "k", Auth: "bearer", HeaderTimeout: Duration(60 * time.Second)}},
		Routes: []Route{{
			Model:    "m",
			Targets:  []Target{{Provider: "p", Priority: 1, Weight: 1}},
			Balance:  "weighted",
			Discover: new(true),
		}},
		Affinity: Affinity{TTL: Duration(time.Hour), Mode: "prefer", Fallback: "prefix_hash"},
		Breaker:  Breaker{FailureThreshold: 3, Cooldown: Duration(30 * time.Second)},
		Models:   Models{CacheTTL: Duration(10 * time.Minute), Timeout: Duration(3 * time.Second)},
		Log:      Log{Path: filepath.Join(h, ".verbatim/verbatim.db"), Retention: Duration(90 * 24 * time.Hour), ErrorExcerptBytes: 4096},
		Pricing:  Pricing{Cache: filepath.Join(h, ".verbatim/pricing")},
	}
	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("defaults:\n got  %+v\n want %+v", *cfg, want)
	}
}

func TestAuthDefaultsByType(t *testing.T) {
	cfg, _ := mustParse(t, `
providers:
  c: {type: chat, base_url: "https://x/v1", key: k}
  r: {type: responses, base_url: "https://x/v1", key: k}
  a: {type: anthropic, base_url: "https://x/anthropic", key: k}
  g: {type: gemini, base_url: "https://x/v1beta", key: k}
  i: {type: images, base_url: "https://x/v1", key: k}
routes:
  - model: m
    targets: [{provider: c}]
`)
	for name, want := range map[string]string{"c": "bearer", "r": "bearer", "a": "x-api-key", "g": "x-goog-api-key", "i": "bearer"} {
		if got := cfg.Providers[name].Auth; got != want {
			t.Errorf("%s auth = %q, want %q", name, got, want)
		}
	}
}

func TestProviderHeaderTimeout(t *testing.T) {
	cfg, _ := mustParse(t, `
server: {header_timeout: 20s}
providers:
  slow: {type: images, base_url: "https://x/v1", key: k, header_timeout: 5m}
  fast: {type: chat, base_url: "https://x/v1", key: k}
routes:
  - model: m
    targets: [{provider: fast}, {provider: slow}]
`)
	if got := cfg.Providers["slow"].HeaderTimeout; got != Duration(5*time.Minute) {
		t.Errorf("slow header_timeout = %s", got)
	}
	if got := cfg.Providers["fast"].HeaderTimeout; got != Duration(20*time.Second) {
		t.Errorf("fast header_timeout = %s, want server.header_timeout", got)
	}
}

func TestExplicitZeroIsKept(t *testing.T) {
	cfg, _ := mustParse(t, minimal+"log:\n  error_excerpt_bytes: 0\n")
	if cfg.Log.ErrorExcerptBytes != 0 {
		t.Errorf("error_excerpt_bytes = %d, want 0", cfg.Log.ErrorExcerptBytes)
	}
}

func TestBaseURLTrailingSlash(t *testing.T) {
	cfg, _ := mustParse(t, strings.Replace(minimal, "https://example.com/v1", "https://example.com/v1/", 1))
	if got := cfg.Providers["p"].BaseURL; got != "https://example.com/v1" {
		t.Errorf("base_url = %q", got)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("VB_KEY", "k-123")
	t.Setenv("VB_HDR", "h-value")
	t.Setenv("VB_HOST", "example.org")
	t.Setenv("VB_LITERAL", "${VB_KEY}")
	t.Setenv("VB_TTL", "2h")
	cfg, _ := mustParse(t, `
# ${VB_UNSET_IN_COMMENT} must not matter
providers:
  p:
    type: chat
    base_url: https://${VB_HOST}/v1
    key: ${VB_KEY}
    headers:
      x-a: pre-${VB_HDR}-post
      x-b: ${VB_LITERAL}
    overrides:
      - set: {path: user, value: "${VB_KEY}"}
      - set: {path: extra, value: {nested: ["${VB_HDR}"]}}
routes:
  - model: m
    targets: [{provider: p}]
affinity:
  ttl: ${VB_TTL}
pricing:
  sources: ["https://${VB_HOST}/prices.json"]
`)
	p := cfg.Providers["p"]
	if p.Key != "k-123" || p.BaseURL != "https://example.org/v1" {
		t.Errorf("key/base_url = %q/%q", p.Key, p.BaseURL)
	}
	if *p.Headers["x-a"] != "pre-h-value-post" {
		t.Errorf("x-a = %q", *p.Headers["x-a"])
	}
	if *p.Headers["x-b"] != "${VB_KEY}" {
		t.Errorf("substituted value was rescanned: x-b = %q", *p.Headers["x-b"])
	}
	if p.Overrides[0].Set.Value != "k-123" {
		t.Errorf("set value = %v", p.Overrides[0].Set.Value)
	}
	if got := p.Overrides[1].Set.Value; !reflect.DeepEqual(got, map[string]any{"nested": []any{"h-value"}}) {
		t.Errorf("nested set value = %#v", got)
	}
	if cfg.Affinity.TTL.Std() != 2*time.Hour {
		t.Errorf("ttl = %v", cfg.Affinity.TTL)
	}
	if cfg.Pricing.Sources[0] != "https://example.org/prices.json" {
		t.Errorf("sources = %v", cfg.Pricing.Sources)
	}
}

func TestEnvExpansionErrors(t *testing.T) {
	t.Setenv("VB_EMPTY", "")
	tests := []struct {
		name, src string
		want      []string
	}{
		{"unset", strings.Replace(minimal, "key: k", "key: ${VB_TEST_UNSET}", 1),
			[]string{"line 6", "providers.p.key", "VB_TEST_UNSET is not set"}},
		{"unterminated", strings.Replace(minimal, "key: k", "key: ${VB_KEY", 1), []string{"providers.p.key", "unterminated"}},
		{"empty name", strings.Replace(minimal, "key: k", "key: a${}b", 1), []string{"providers.p.key", "empty variable name"}},
		// A set but empty variable expands to "" and is then caught by validation.
		{"empty value", strings.Replace(minimal, "key: k", "key: ${VB_EMPTY}", 1), []string{"providers.p.key: required unless auth is none"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Parse([]byte(tt.src), "test.yaml")
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestSchemaErrors(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"unknown top-level key", minimal + "extra: 1\n", `line 11: extra: unknown field "extra"`},
		{"unknown nested key", minimal + "server:\n  lisen: x\n", `server.lisen: unknown field "lisen"`},
		{"unknown target key", strings.Replace(minimal, "- provider: p", "- provider: p\n        wieght: 2", 1), `routes[0].targets[0].wieght: unknown field`},
		{"unknown provider key", strings.Replace(minimal, "key: k", "key: k\n    model: x", 1), `providers.p.model: unknown field`},
		{"bad duration", minimal + "breaker:\n  cooldown: 1w\n", `line 12: breaker.cooldown: invalid duration "1w"`},
		{"unknown key in inline merge", minimal + "server: {<<: {max_inflght: 1}}\n", `server.max_inflght: unknown field "max_inflght"`},
		{"unknown key in merge list", minimal + "server:\n  <<: [{listen: \"127.0.0.1:1\"}, {bogus: 1}]\n", `server.bogus: unknown field "bogus"`},
		{"bad duration in merge", minimal + "breaker: {<<: {cooldown: 1w}}\n", `breaker.cooldown: invalid duration "1w"`},
		{"type mismatch", minimal + "server:\n  max_inflight: lots\n", "line 12: cannot unmarshal"},
		{"syntax", "providers: [", "test.yaml: yaml:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Parse([]byte(tt.src), "test.yaml")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidation(t *testing.T) {
	provider := func(body string) string {
		return "providers:\n  p:\n" + body + "routes:\n  - model: m\n    targets: [{provider: p}]\n"
	}
	chat := "    type: chat\n    base_url: https://x/v1\n    key: k\n"
	anthropic := "    type: anthropic\n    base_url: https://x/anthropic\n    key: k\n"
	routes := func(body string) string {
		return "providers:\n  p: {type: chat, base_url: \"https://x/v1\", key: k}\n  q: {type: anthropic, base_url: \"https://x/a\", key: k}\nroutes:\n" + body
	}
	tests := []struct {
		name, src, want string
	}{
		// providers
		{"no providers", "routes: [{model: m, targets: [{provider: p}]}]\n", "providers: at least one provider is required"},
		{"bad type", provider("    type: grpc\n    base_url: https://x\n    key: k\n"), `providers.p.type: must be one of`},
		{"no base_url", provider("    type: chat\n    key: k\n"), "providers.p.base_url: required"},
		{"relative base_url", provider("    type: chat\n    base_url: /v1\n    key: k\n"), "providers.p.base_url: must be an absolute http or https URL"},
		{"ftp base_url", provider("    type: chat\n    base_url: ftp://x/v1\n    key: k\n"), "providers.p.base_url: must be an absolute"},
		{"query in base_url", provider("    type: chat\n    base_url: https://x/v1?a=b\n    key: k\n"), "providers.p.base_url: must not contain a query string or fragment"},
		{"fragment in base_url", provider("    type: chat\n    base_url: https://x/v1#f\n    key: k\n"), "providers.p.base_url: must not contain"},
		{"missing key", provider("    type: chat\n    base_url: https://x/v1\n"), "providers.p.key: required unless auth is none"},
		{"bad auth", provider(chat + "    auth: basic\n"), `providers.p.auth: must be one of bearer, x-api-key, x-goog-api-key, none, got "basic"`},
		{"bad header name", provider(chat + "    headers:\n      \"bad header\": x\n"), "providers.p.headers.bad header: not a valid header name"},
		{"managed header", provider(chat + "    headers:\n      content-length: \"5\"\n"), "providers.p.headers.content-length: managed by verbatim"},
		{"managed header host", provider(chat + "    headers:\n      HOST: x\n"), "providers.p.headers.HOST: managed by verbatim"},
		{"header value newline", provider(chat + "    headers:\n      x-a: \"a\\nb\"\n"), "providers.p.headers.x-a: value contains a control character"},
		{"header value NUL", provider(chat + "    headers:\n      x-a: \"a\\0b\"\n"), "providers.p.headers.x-a: value contains a control character"},
		{"header value control", provider(chat + "    headers:\n      x-a: \"a\\x01b\"\n"), "providers.p.headers.x-a: value contains a control character"},
		{"header value DEL", provider(chat + "    headers:\n      x-a: \"a\\x7fb\"\n"), "providers.p.headers.x-a: value contains a control character"},
		{"key control", provider("    type: chat\n    base_url: https://x/v1\n    key: \"k\\r\"\n"), "providers.p.key: contains a control character"},
		{"api_key control", minimal + "server:\n  api_key: \"s\\x1b\"\n", "server.api_key: contains a control character"},
		{"negative header_timeout", provider(chat + "    header_timeout: -1s\n"), "providers.p.header_timeout: must be greater than 0"},
		{"images overrides", provider("    type: images\n    base_url: https://x/v1\n    key: k\n    overrides:\n      - delete: quality\n"), "providers.p.overrides: not supported on an images provider"},
		// overrides
		{"override neither", provider(chat + "    overrides:\n      - {}\n"), "providers.p.overrides[0]: must have exactly one of delete or set"},
		{"override both", provider(chat + "    overrides:\n      - {delete: a, set: {path: b, value: 1}}\n"), "providers.p.overrides[0]: must have exactly one"},
		{"set without path", provider(chat + "    overrides:\n      - set: {value: 1}\n"), "providers.p.overrides[0].set.path: path is empty"},
		{"delete model", provider(chat + "    overrides:\n      - delete: model\n"), `providers.p.overrides[0].delete: "model": model and stream cannot be overridden`},
		{"set nested stream", provider(chat + "    overrides:\n      - set: {path: a.stream.b, value: 1}\n"), `overrides[0].set.path: "a.stream.b": model and stream`},
		{"forced key model", provider(chat + "    overrides:\n      - delete: \"a.:model\"\n"), "model and stream cannot be overridden"},
		{"anthropic system", provider(anthropic + "    overrides:\n      - delete: system\n"), `"system": system, tools and messages cannot be overridden on an anthropic provider`},
		{"anthropic messages nested", provider(anthropic + "    overrides:\n      - set: {path: messages.0.x, value: 1}\n"), "cannot be overridden on an anthropic provider"},
		{"anthropic tools", provider(anthropic + "    overrides:\n      - delete: tools\n"), "cannot be overridden on an anthropic provider"},
		{"overlap equal", provider(chat + "    overrides:\n      - delete: a\n      - set: {path: a, value: 1}\n"), `providers.p.overrides[1].set.path: "a" overlaps overrides[0]`},
		{"overlap prefix", provider(chat + "    overrides:\n      - set: {path: a.b, value: 1}\n      - delete: a\n"), `overrides[1].delete: "a" overlaps overrides[0]`},
		{"wildcard path", provider(chat + "    overrides:\n      - delete: tools.#.x\n"), "wildcards, queries and modifiers are not supported"},
		{"empty segment", provider(chat + "    overrides:\n      - delete: a..b\n"), "empty path segment"},
		{"non-string key", provider(chat + "    overrides:\n      - set: {path: a, value: {b: [{1: x}]}}\n"), "providers.p.overrides[0].set.value: mapping keys must be strings"},
		{"timestamp value", provider(chat + "    overrides:\n      - set: {path: a, value: 2026-10-15}\n"), "set.value: a timestamp is not a JSON value"},
		{"infinite value", provider(chat + "    overrides:\n      - set: {path: a, value: .inf}\n"), "set.value: infinity and NaN are not JSON values"},
		// routes
		{"no routes", "providers:\n  p: {type: chat, base_url: \"https://x\", key: k}\n", "routes: at least one route is required"},
		{"empty route model", routes("  - targets: [{provider: p}]\n"), "routes[0].model: required"},
		{"duplicate route", routes("  - model: m\n    targets: [{provider: p}]\n  - model: m\n    targets: [{provider: q}]\n"), `routes[1].model: "m" duplicates routes[0]`},
		{"no targets", routes("  - model: m\n"), "routes[0].targets: at least one target is required"},
		{"undefined provider", routes("  - model: m\n    targets: [{provider: nope}]\n"), `routes[0].targets[0].provider: "nope" is not defined in providers`},
		{"missing provider", routes("  - model: m\n    targets: [{weight: 2}]\n"), "routes[0].targets[0].provider: required"},
		{"zero weight", routes("  - model: m\n    targets: [{provider: p, weight: 0}]\n"), "routes[0].targets[0].weight: must be at least 1"},
		{"negative priority", routes("  - model: m\n    targets: [{provider: p, priority: -1}]\n"), "routes[0].targets[0].priority: must be at least 1"},
		{"duplicate target", routes("  - model: m\n    targets: [{provider: p, model: x}, {provider: p, model: x, priority: 2}]\n"), "routes[0].targets[1]: duplicates targets[0]"},
		{"duplicate target by route name", routes("  - model: m\n    targets: [{provider: p}, {provider: p, model: m, priority: 2}]\n"), "routes[0].targets[1]: duplicates targets[0]"},
		{"bad balance", routes("  - model: m\n    balance: random\n    targets: [{provider: p}]\n"), `routes[0].balance: must be weighted or round_robin, got "random"`},
		// affinity
		{"bad mode", minimal + "affinity:\n  mode: sticky\n", `affinity.mode: must be prefer or strict, got "sticky"`},
		{"bad fallback", minimal + "affinity:\n  fallback: random\n", "affinity.fallback: must be prefix_hash or none"},
		{"zero ttl", minimal + "affinity:\n  ttl: 0s\n", "affinity.ttl: must be greater than 0"},
		{"rule without key", minimal + "affinity:\n  rules:\n    - match: {protocol: chat}\n", "affinity.rules[0].key: at least one key source is required"},
		{"rule bad protocol", minimal + "affinity:\n  rules:\n    - match: {protocol: grpc}\n      key: [{body: user}]\n", "affinity.rules[0].match.protocol: must be one of"},
		{"source both", minimal + "affinity:\n  rules:\n    - key: [{body: user, header: x-a}]\n", "affinity.rules[0].key[0]: must have exactly one of body or header"},
		{"source neither", minimal + "affinity:\n  rules:\n    - key: [{}]\n", "affinity.rules[0].key[0]: must have exactly one"},
		{"source bad header", minimal + "affinity:\n  rules:\n    - key: [{header: \"x a\"}]\n", "affinity.rules[0].key[0].header: not a valid header name"},
		// server and small sections
		{"bad listen", minimal + "server:\n  listen: localhost\n", `server.listen: must be host:port, got "localhost"`},
		{"zero body", minimal + "server:\n  max_body_bytes: 0\n", "server.max_body_bytes: must be greater than 0"},
		{"zero inflight", minimal + "server:\n  max_inflight: 0\n", "server.max_inflight: must be greater than 0"},
		{"negative header timeout", minimal + "server:\n  header_timeout: -1s\n", "server.header_timeout: must be greater than 0"},
		{"zero idle timeout", minimal + "server:\n  idle_timeout: 0s\n", "server.idle_timeout: must be greater than 0"},
		{"zero threshold", minimal + "breaker:\n  failure_threshold: 0\n", "breaker.failure_threshold: must be at least 1"},
		{"zero cooldown", minimal + "breaker:\n  cooldown: 0s\n", "breaker.cooldown: must be greater than 0"},
		{"zero cache ttl", minimal + "models:\n  cache_ttl: 0s\n", "models.cache_ttl: must be greater than 0"},
		{"zero models timeout", minimal + "models:\n  timeout: 0s\n", "models.timeout: must be greater than 0"},
		{"zero retention", minimal + "log:\n  retention: 0d\n", "log.retention: must be greater than 0"},
		{"negative excerpt", minimal + "log:\n  error_excerpt_bytes: -1\n", "log.error_excerpt_bytes: must not be negative"},
		{"empty log path", minimal + "log:\n  path: \"\"\n", "log.path: required"},
		{"relative source", minimal + "pricing:\n  sources: [prices.json]\n", "pricing.sources[0]: must be an absolute http or https URL"},
		{"empty alias", minimal + "pricing:\n  aliases: {a: \"\"}\n", "pricing.aliases.a: must not be empty"},
		{"source without file name", minimal + "pricing:\n  sources: [\"https://x/\"]\n", "pricing.sources[0]: has no file name in its path"},
		{"shared cache file name", minimal + "pricing:\n  sources: [\"https://a/v1/prices.json\", \"https://b/other\", \"https://b/prices\"]\n",
			"pricing.sources[2]: shares the cache file name prices.json with pricing.sources[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Parse([]byte(tt.src), "test.yaml")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v\nwant containing %q", err, tt.want)
			}
		})
	}
}

func TestValidOverrides(t *testing.T) {
	cfg, _ := mustParse(t, `
providers:
  a:
    type: anthropic
    base_url: https://x/anthropic
    key: k
    overrides:
      - delete: metadata
      - delete: ab               # does not overlap "a"
      - delete: a
      - set: {path: 'x\.model', value: 1}   # the key "x.model", not a nested model
      - set: {path: thinking.budget_tokens, value: 1024}
      - set: {path: system_extra, value: [1, "two", {three: 3}]}
routes:
  - model: m
    targets: [{provider: a}]
`)
	if n := len(cfg.Providers["a"].Overrides); n != 6 {
		t.Errorf("overrides = %d, want 6", n)
	}
}

func TestAllProblemsReported(t *testing.T) {
	_, _, err := Parse([]byte(`
providers:
  p: {type: grpc, base_url: "https://x"}
routes:
  - model: m
    targets: [{provider: nope, weight: 0}]
breaker:
  failure_threshold: 0
`), "cfg.yaml")
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "cfg.yaml: invalid configuration:\n") {
		t.Errorf("error header: %q", msg)
	}
	for _, w := range []string{"providers.p.type", "routes[0].targets[0].provider", "routes[0].targets[0].weight", "breaker.failure_threshold"} {
		if !strings.Contains(msg, "\n  "+w+": ") {
			t.Errorf("error does not list %s on its own line:\n%s", w, msg)
		}
	}
}

func TestWarnings(t *testing.T) {
	tests := []struct {
		name, src string
		want      []string // Warning.String() of each expected warning
	}{
		{"loopback without key", minimal + "server:\n  listen: localhost:9000\n", nil},
		{"ipv6 loopback without key", minimal + "server:\n  listen: \"[::1]:9000\"\n", nil},
		{"public without key", minimal + "server:\n  listen: 0.0.0.0:8080\n",
			[]string{`server.api_key: not set and server.listen "0.0.0.0:8080" is not a loopback address; the server will accept unauthenticated requests`}},
		{"all interfaces without key", minimal + "server:\n  listen: \":8080\"\n", []string{"server.api_key: not set"}},
		{"public with key", minimal + "server:\n  listen: 0.0.0.0:8080\n  api_key: s\n", nil},
		{"key with auth none", "providers:\n  p: {type: chat, base_url: \"http://x\", auth: none, key: k}\nroutes: [{model: m, targets: [{provider: p}]}]\n",
			[]string{"providers.p.key: ignored because auth is none"}},
		{"discover on exact route", strings.Replace(minimal, "  - model: m\n", "  - model: m\n    discover: true\n", 1),
			[]string{"routes[0].discover: ignored on an exact-name route"}},
		{"shadowed exact route", `
providers:
  p: {type: chat, base_url: "https://x", key: k}
routes:
  - model: "gpt-*"
    targets: [{provider: p}]
  - model: gpt-5
    targets: [{provider: p}]
`, []string{`routes[1]: unreachable: earlier pattern routes starting at routes[0] ("gpt-*") match "gpt-5"`}},
		{"exact route reachable for another protocol", `
providers:
  p: {type: chat, base_url: "https://x", key: k}
  q: {type: anthropic, base_url: "https://x", key: k}
routes:
  - model: "gpt-*"
    targets: [{provider: p}]
  - model: gpt-5
    targets: [{provider: p}, {provider: q}]
`, nil},
		{"exact route before pattern", `
providers:
  p: {type: chat, base_url: "https://x", key: k}
routes:
  - model: gpt-5
    targets: [{provider: p}]
  - model: "*"
    targets: [{provider: p}]
`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, warns := mustParse(t, tt.src)
			if len(warns) != len(tt.want) {
				t.Fatalf("warnings = %v, want %d", warns, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.HasPrefix(warns[i].String(), w) {
					t.Errorf("warning %q, want prefix %q", warns[i], w)
				}
			}
		})
	}
}

func TestRedacted(t *testing.T) {
	cfg, _ := mustParse(t, `
server:
  api_key: client-secret
providers:
  p:
    type: chat
    base_url: https://user:pass@x.example/v1
    key: provider-secret
    headers:
      x-token: header-secret
      x-drop: null
    overrides:
      - set: {path: a, value: 1}
  n:
    type: chat
    base_url: http://127.0.0.1/v1
    auth: none
routes:
  - model: m
    targets: [{provider: p}]
affinity:
  rules:
    - key: [{header: x-s}]
pricing:
  aliases: {a: b}
`)
	r := cfg.Redacted()
	if r.Server.APIKey != "***" {
		t.Errorf("api_key = %q", r.Server.APIKey)
	}
	p := r.Providers["p"]
	if p.Key != "***" || r.Providers["n"].Key != "" {
		t.Errorf("keys = %q, %q", p.Key, r.Providers["n"].Key)
	}
	if p.BaseURL != "https://x.example/v1" {
		t.Errorf("base_url = %q", p.BaseURL)
	}
	if *p.Headers["x-token"] != "***" || p.Headers["x-drop"] != nil {
		t.Errorf("headers = %v / %v", *p.Headers["x-token"], p.Headers["x-drop"])
	}

	// Mutating the copy must not reach the original.
	p.Overrides[0].Set.Path = "changed"
	r.Routes[0].Targets[0].Provider = "changed"
	*r.Routes[0].Discover = false
	r.Affinity.Rules[0].Key[0].Header = "changed"
	r.Pricing.Aliases["a"] = "changed"

	o := cfg.Providers["p"]
	if cfg.Server.APIKey != "client-secret" || o.Key != "provider-secret" || *o.Headers["x-token"] != "header-secret" ||
		o.BaseURL != "https://user:pass@x.example/v1" || o.Overrides[0].Set.Path != "a" ||
		cfg.Routes[0].Targets[0].Provider != "p" || !*cfg.Routes[0].Discover ||
		cfg.Affinity.Rules[0].Key[0].Header != "x-s" || cfg.Pricing.Aliases["a"] != "b" {
		t.Errorf("original mutated: %+v", cfg)
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verbatim.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"90d", 90 * 24 * time.Hour, true},
		{"1h30m", 90 * time.Minute, true},
		{"30s", 30 * time.Second, true},
		{"0d", 0, true},
		{"1w", 0, false},
		{"-1d", 0, false},
		{"d", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, err := ParseDuration(tt.in)
		if (err == nil) != tt.ok || (tt.ok && got.Std() != tt.want) {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v, ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
	}
	if s := Duration(90 * 24 * time.Hour).String(); s != "90d" {
		t.Errorf("String() = %q, want 90d", s)
	}
}

func TestAnchorsAndMerge(t *testing.T) {
	t.Setenv("VB_KEY", "k-123")
	cfg, _ := mustParse(t, `
providers:
  a: &base
    type: chat
    base_url: https://x/v1
    key: ${VB_KEY}
  b:
    <<: *base
    base_url: https://y/v1
routes:
  - model: m
    targets: [{provider: a}, {provider: b}]
`)
	b := cfg.Providers["b"]
	if b.Key != "k-123" || b.Type != "chat" || b.BaseURL != "https://y/v1" {
		t.Errorf("merged provider = %+v", b)
	}
}

func TestPatternRouteTargetWithoutModel(t *testing.T) {
	// On a pattern route an absent model forwards whatever name matched, so it
	// is a different target from any literal name.
	mustParse(t, `
providers:
  p: {type: chat, base_url: "https://x/v1", key: k}
routes:
  - model: "m*"
    targets: [{provider: p}, {provider: p, model: "m*"}]
`)
}

func TestAnchorsExpansion(t *testing.T) {
	t.Run("alias carrying an unset variable", func(t *testing.T) {
		// The anchor sits inside an inline merge mapping and is used again
		// through an alias.
		_, _, err := Parse([]byte(`
providers:
  a:
    <<: {type: chat, base_url: "https://x/v1", key: &k "${VB_TEST_UNSET}"}
  b: {type: chat, base_url: "https://y/v1", key: *k}
routes:
  - model: m
    targets: [{provider: a}, {provider: b}]
`), "test.yaml")
		if err == nil || !strings.Contains(err.Error(), "providers.a.key: environment variable VB_TEST_UNSET is not set") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("anchor reused twice is expanded once", func(t *testing.T) {
		// A second expansion would rescan the substituted value.
		t.Setenv("VB_ONCE", "k-${VB_ONCE_INNER}")
		t.Setenv("VB_ONCE_INNER", "rescanned")
		cfg, _ := mustParse(t, `
providers:
  a: &base
    type: chat
    base_url: https://x/v1
    key: ${VB_ONCE}
    headers:
      x-a: &h "${VB_ONCE}"
      x-b: *h
  b:
    <<: *base
  c:
    <<: [*base]
    base_url: https://y/v1
routes:
  - model: m
    targets: [{provider: a}, {provider: b}, {provider: c}]
`)
		const want = "k-${VB_ONCE_INNER}"
		for _, name := range []string{"a", "b", "c"} {
			p := cfg.Providers[name]
			if p.Key != want || *p.Headers["x-a"] != want || *p.Headers["x-b"] != want {
				t.Errorf("provider %s: key %q, x-a %q, x-b %q; want %q", name, p.Key, *p.Headers["x-a"], *p.Headers["x-b"], want)
			}
		}
		if got := cfg.Providers["c"].BaseURL; got != "https://y/v1" {
			t.Errorf("c base_url = %q", got)
		}
	})
}

func TestLoadLenient(t *testing.T) {
	write := func(t *testing.T, src string) string {
		path := filepath.Join(t.TempDir(), "verbatim.yaml")
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := write(t, strings.Replace(minimal, "key: k", "key: ${VB_MISSING_KEY}", 1)+"log:\n  path: ~/logs/v.db\n")

	cfg, err := LoadLenient(path)
	if err != nil {
		t.Fatalf("LoadLenient: %v", err)
	}
	if k := cfg.Providers["p"].Key; k != "" {
		t.Errorf("key = %q, want empty", k)
	}
	if want := filepath.Join(home(t), "logs/v.db"); cfg.Log.Path != want {
		t.Errorf("log.path = %q, want %q", cfg.Log.Path, want)
	}
	if want := filepath.Join(home(t), ".verbatim/pricing"); cfg.Pricing.Cache != want {
		t.Errorf("pricing.cache = %q, want %q", cfg.Pricing.Cache, want)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Errorf("server.listen = %q", cfg.Server.Listen)
	}

	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "VB_MISSING_KEY is not set") {
		t.Errorf("Load error = %v", err)
	}

	// Schema errors are still errors; semantic validation is skipped.
	if _, err := LoadLenient(write(t, minimal+"log:\n  pth: x\n")); err == nil || !strings.Contains(err.Error(), `log.pth: unknown field`) {
		t.Errorf("unknown key: error = %v", err)
	}
	if _, err := LoadLenient(write(t, minimal+"log:\n  retention: 1w\n")); err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Errorf("bad duration: error = %v", err)
	}
	if _, err := LoadLenient(write(t, "log:\n  path: /tmp/v.db\n")); err != nil {
		t.Errorf("config without providers: %v", err)
	}
}
