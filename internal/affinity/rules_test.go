package affinity

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"

	"verbatim/internal/config"
	"verbatim/internal/protocol"
)

// fakeProto implements only what Key uses; the embedded nil interface
// panics if anything else is called.
type fakeProto struct {
	protocol.Protocol
	id protocol.ID
}

func (f fakeProto) ID() protocol.ID { return f.id }

// PrefixForHash follows the real protocols' layout: the protocol id, the
// "system" field, and the "user" field as raw bytes, NUL-separated.
func (f fakeProto) PrefixForHash(body []byte) ([]byte, bool) {
	u := gjson.GetBytes(body, "user")
	if !u.Exists() {
		return nil, false
	}
	return []byte(string(f.id) + "\x00" + gjson.GetBytes(body, "system").Raw + "\x00" + u.Raw), true
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func mustCompile(t *testing.T, a config.Affinity) *Rules {
	t.Helper()
	r, err := Compile(a)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestKey(t *testing.T) {
	rules := mustCompile(t, config.Affinity{
		Fallback: "prefix_hash",
		Rules: []config.Rule{
			{Match: config.RuleMatch{Protocol: "anthropic", Model: "claude-*"},
				Key: []config.KeySource{{Body: "metadata.user_id"}, {Header: "x-session-affinity"}}},
			{Match: config.RuleMatch{Protocol: "anthropic"},
				Key: []config.KeySource{{Header: "x-other"}}},
			{Match: config.RuleMatch{Protocol: "chat"},
				Key: []config.KeySource{{Header: "x-session-affinity"}, {Body: "prompt_cache_key"}, {Body: "user"}}},
		},
	})
	anth := fakeProto{id: protocol.Anthropic}
	chat := fakeProto{id: protocol.Chat}
	resp := fakeProto{id: protocol.Responses}
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	tests := []struct {
		name   string
		p      protocol.Protocol
		model  string
		h      http.Header
		body   string
		key    string
		source string
		ok     bool
	}{
		{"body before header in rule order", anth, "claude-x", hdr("X-Session-Affinity", "h1"),
			`{"metadata":{"user_id":"u1"},"user":"x"}`, sha("u1"), "body:metadata.user_id", true},
		{"empty body value falls to header, case-insensitive", anth, "claude-x", hdr("x-session-affinity", "h1"),
			`{"metadata":{"user_id":""}}`, sha("h1"), "header:x-session-affinity", true},
		{"model glob mismatch selects next rule", anth, "kimi-k3", hdr("x-session-affinity", "h1", "x-other", "o1"),
			`{"metadata":{"user_id":"u1"}}`, sha("o1"), "header:x-other", true},
		{"header listed first wins over body", chat, "m", hdr("x-session-affinity", "h2"),
			`{"prompt_cache_key":"pck","user":"bob"}`, sha("h2"), "header:x-session-affinity", true},
		{"number taken as sent", chat, "m", nil,
			`{"user":12.50}`, sha("12.50"), "body:user", true},
		{"non-string non-number ignored: fallback", chat, "m", nil,
			`{"prompt_cache_key":true,"user":{"a":1}}`, sha("chat\x00\x00{\"a\":1}"), "prefix_hash", true},
		{"first matching rule only, then fallback", anth, "kimi-k3", nil,
			`{"system":"s","user":"hi"}`, sha("anthropic\x00\"s\"\x00\"hi\""), "prefix_hash", true},
		{"no rule for protocol: fallback", resp, "m", hdr("x-session-affinity", "h"),
			`{"system":"s","user":"hi"}`, sha("responses\x00\"s\"\x00\"hi\""), "prefix_hash", true},
		{"fallback without user message: no key", resp, "m", nil,
			`{"system":"s"}`, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.h
			if h == nil {
				h = http.Header{}
			}
			key, source, ok := rules.Key(tt.p, tt.model, h, []byte(tt.body))
			if key != tt.key || source != tt.source || ok != tt.ok {
				t.Fatalf("Key = %q %q %v, want %q %q %v", key, source, ok, tt.key, tt.source, tt.ok)
			}
		})
	}
}

func TestKeyPrefixHashDeterministicAndProtocolScoped(t *testing.T) {
	rules := mustCompile(t, config.Affinity{Fallback: "prefix_hash"})
	body := []byte(`{"system":"s","user":{"content":"hi"},"later":1}`)
	k1, _, _ := rules.Key(fakeProto{id: protocol.Chat}, "m", http.Header{}, body)
	k2, _, _ := rules.Key(fakeProto{id: protocol.Chat}, "m", http.Header{}, []byte(`{"system":"s","user":{"content":"hi"},"later":2}`))
	k3, _, _ := rules.Key(fakeProto{id: protocol.Anthropic}, "m", http.Header{}, body)
	if k1 == "" || k1 != k2 {
		t.Fatalf("same prefix gave %q and %q", k1, k2)
	}
	if k1 == k3 {
		t.Fatal("prefix hash does not include the protocol id")
	}
}

func TestKeyFallbackNone(t *testing.T) {
	rules := mustCompile(t, config.Affinity{Fallback: "none"})
	if key, source, ok := rules.Key(fakeProto{id: protocol.Chat}, "m", http.Header{}, []byte(`{"user":"hi"}`)); ok || key != "" || source != "" {
		t.Fatalf("Key = %q %q %v", key, source, ok)
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Affinity
		want string
	}{
		{"fallback", config.Affinity{Fallback: "hash"}, `affinity.fallback: must be prefix_hash or none, got "hash"`},
		{"protocol", config.Affinity{Fallback: "none", Rules: []config.Rule{
			{Match: config.RuleMatch{Protocol: "openai"}, Key: []config.KeySource{{Header: "x"}}}}},
			`affinity.rules[0].match.protocol: unknown protocol "openai"`},
		{"no key", config.Affinity{Fallback: "none", Rules: []config.Rule{{}}},
			"affinity.rules[0].key: at least one source is required"},
		{"both", config.Affinity{Fallback: "none", Rules: []config.Rule{
			{Key: []config.KeySource{{Header: "x"}, {Header: "y", Body: "z"}}}}},
			"affinity.rules[0].key[1]: exactly one of body or header is required"},
		{"neither", config.Affinity{Fallback: "none", Rules: []config.Rule{
			{Key: []config.KeySource{{}}}}},
			"affinity.rules[0].key[0]: exactly one of body or header is required"},
	}
	for _, tt := range tests {
		if _, err := Compile(tt.cfg); err == nil || err.Error() != tt.want {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}
