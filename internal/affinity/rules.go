// Package affinity derives session keys from requests and keeps the
// in-memory pins that send one session to one target (design 7).
package affinity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"

	"github.com/tidwall/gjson"

	"verbatim/internal/config"
	"verbatim/internal/protocol"
)

// Rules is the compiled affinity.rules list plus the fallback setting.
type Rules struct {
	rules    []config.Rule
	fallback string
}

// Compile checks the affinity configuration and returns the rules.
func Compile(cfg config.Affinity) (*Rules, error) {
	if cfg.Fallback != "prefix_hash" && cfg.Fallback != "none" {
		return nil, fmt.Errorf("affinity.fallback: must be prefix_hash or none, got %q", cfg.Fallback)
	}
	known := []protocol.ID{protocol.Chat, protocol.Responses, protocol.Anthropic, protocol.Gemini, protocol.Images}
	for i, r := range cfg.Rules {
		if r.Match.Protocol != "" && !slices.Contains(known, protocol.ID(r.Match.Protocol)) {
			return nil, fmt.Errorf("affinity.rules[%d].match.protocol: unknown protocol %q", i, r.Match.Protocol)
		}
		if len(r.Key) == 0 {
			return nil, fmt.Errorf("affinity.rules[%d].key: at least one source is required", i)
		}
		for j, s := range r.Key {
			if (s.Body == "") == (s.Header == "") {
				return nil, fmt.Errorf("affinity.rules[%d].key[%d]: exactly one of body or header is required", i, j)
			}
		}
	}
	return &Rules{rules: slices.Clone(cfg.Rules), fallback: cfg.Fallback}, nil
}

// Key derives the affinity key for a request (design 7.1). The first rule
// whose match fits is used, and its first source yielding a non-empty value
// is the key; a body source takes a string or number exactly as sent, a
// header source the header's first value. Otherwise the fallback applies:
// prefix_hash hashes the protocol id and p.PrefixForHash(body), none yields
// no key.
//
// key is the lowercase hex SHA-256 of the material, for explicit values too,
// so pins and the log share one representation. source names what produced
// the key ("body:<path>", "header:<name>", "prefix_hash"), or is empty when
// ok is false.
func (r *Rules) Key(p protocol.Protocol, clientModel string, h http.Header, body []byte) (key, source string, ok bool) {
	for _, rule := range r.rules {
		if rule.Match.Protocol != "" && protocol.ID(rule.Match.Protocol) != p.ID() {
			continue
		}
		if rule.Match.Model != "" && !config.MatchGlob(rule.Match.Model, clientModel) {
			continue
		}
		for _, s := range rule.Key {
			if s.Header != "" {
				if v := h.Get(s.Header); v != "" {
					return hash([]byte(v)), "header:" + s.Header, true
				}
				continue
			}
			if v := bodyValue(body, s.Body); v != "" {
				return hash([]byte(v)), "body:" + s.Body, true
			}
		}
		break
	}
	if r.fallback != "prefix_hash" {
		return "", "", false
	}
	prefix, ok := p.PrefixForHash(body)
	if !ok {
		return "", "", false
	}
	return hash(prefix), "prefix_hash", true
}

// bodyValue returns the string or number at path as sent: a string's
// content, a number's literal text. Other JSON types yield nothing.
func bodyValue(body []byte, path string) string {
	switch v := gjson.GetBytes(body, path); v.Type {
	case gjson.String:
		return v.Str
	case gjson.Number:
		return v.Raw
	default:
		return ""
	}
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
