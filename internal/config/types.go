// Package config loads, expands, and validates the verbatim YAML configuration.
//
// The types in this file are the schema. Load fills every default so that
// consumers read fields directly and never re-derive defaults.
package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Server    Server              `yaml:"server"`
	Providers map[string]Provider `yaml:"providers"`
	Routes    []Route             `yaml:"routes"`
	Affinity  Affinity            `yaml:"affinity"`
	Breaker   Breaker             `yaml:"breaker"`
	Models    Models              `yaml:"models"`
	Log       Log                 `yaml:"log"`
	Pricing   Pricing             `yaml:"pricing"`
}

type Server struct {
	Listen        string   `yaml:"listen"`         // default 127.0.0.1:8080
	APIKey        string   `yaml:"api_key"`        // optional on loopback
	MaxBodyBytes  int64    `yaml:"max_body_bytes"` // default 32 MiB
	MaxInflight   int      `yaml:"max_inflight"`   // default 64
	HeaderTimeout Duration `yaml:"header_timeout"` // default 60s
	IdleTimeout   Duration `yaml:"idle_timeout"`   // default 120s
}

// Provider is one upstream service. It serves each protocol in Endpoints,
// with one key, one set of headers, and one list of overrides for all.
type Provider struct {
	Key string `yaml:"key"`
	// Headers are applied after client headers are copied and before auth.
	// A nil value removes the header.
	Headers   map[string]*string `yaml:"headers"`
	Overrides []Override         `yaml:"overrides"` // not allowed with an images endpoint
	// FailoverOn lists statuses that fail over from this provider in addition
	// to the built-in ones (design 8.1). They never feed the breaker.
	FailoverOn []int               `yaml:"failover_on"`
	Endpoints  map[string]Endpoint `yaml:"endpoints"` // keyed by protocol id
}

// Endpoint is where a provider serves one protocol.
type Endpoint struct {
	BaseURL string `yaml:"base_url"` // the base URL the protocol's official SDK takes
	Auth    string `yaml:"auth"`     // bearer | x-api-key | none; Load fills the protocol's default
	// HeaderTimeout bounds the wait for this endpoint's response headers;
	// Load fills server.header_timeout. A non-streamed image is answered
	// only once it is drawn.
	HeaderTimeout Duration `yaml:"header_timeout"`
}

// EndpointName names a provider's endpoint for one protocol the way a
// target names it, "provider.protocol". Circuit breakers and model lists
// are kept per endpoint under this name.
func EndpointName(provider, proto string) string { return provider + "." + proto }

// Override is exactly one of Delete or Set.
type Override struct {
	Delete string `yaml:"delete,omitempty"`
	Set    *SetOp `yaml:"set,omitempty"`
}

type SetOp struct {
	Path  string `yaml:"path"`
	Value any    `yaml:"value"`
}

type Route struct {
	Model    string   `yaml:"model"` // glob, '*' matches any run including '/'; route identity
	Targets  []Target `yaml:"targets"`
	Balance  string   `yaml:"balance"`  // weighted (default) | round_robin
	Discover *bool    `yaml:"discover"` // Load fills true
}

type Target struct {
	Provider string `yaml:"provider"` // a provider, or "provider.protocol" for one of its endpoints
	Model    string `yaml:"model"`    // upstream model name; empty forwards the client's name; a '*' is what the route's '*' matched
	Priority int    `yaml:"priority"` // default 1, lower first
	Weight   int    `yaml:"weight"`   // default 1
}

// Split returns the target's provider name and, when the target names a
// single endpoint, that endpoint's protocol.
func (t Target) Split() (provider, proto string) {
	provider, proto, _ = strings.Cut(t.Provider, ".")
	return provider, proto
}

// Protocols returns the protocols a target serves, sorted: the one its
// endpoint names, or else every endpoint of its provider.
func (c *Config) Protocols(t Target) []string {
	provider, proto := t.Split()
	if proto != "" {
		return []string{proto}
	}
	return slices.Sorted(maps.Keys(c.Providers[provider].Endpoints))
}

type Affinity struct {
	TTL      Duration `yaml:"ttl"`      // default 1h
	Mode     string   `yaml:"mode"`     // prefer (default) | strict
	Rules    []Rule   `yaml:"rules"`    // ordered, first matching rule wins
	Fallback string   `yaml:"fallback"` // prefix_hash (default) | none
}

type Rule struct {
	Match RuleMatch   `yaml:"match"`
	Key   []KeySource `yaml:"key"` // ordered, first non-empty value wins
}

type RuleMatch struct {
	Protocol string `yaml:"protocol"` // optional protocol id
	Model    string `yaml:"model"`    // optional glob against the client model name
}

// KeySource is exactly one of Body (a gjson path) or Header (a header name).
type KeySource struct {
	Body   string `yaml:"body,omitempty"`
	Header string `yaml:"header,omitempty"`
}

type Breaker struct {
	FailureThreshold int      `yaml:"failure_threshold"` // default 3
	Cooldown         Duration `yaml:"cooldown"`          // default 30s
}

type Models struct {
	CacheTTL Duration `yaml:"cache_ttl"` // default 1h
	Timeout  Duration `yaml:"timeout"`   // default 10s
}

type Log struct {
	Path              string   `yaml:"path"`                // default ~/.verbatim/verbatim.db, expanded
	Retention         Duration `yaml:"retention"`           // default 90d
	ErrorExcerptBytes int      `yaml:"error_excerpt_bytes"` // default 4096
}

type Pricing struct {
	Sources   []string          `yaml:"sources"`
	Cache     string            `yaml:"cache"`     // default ~/.verbatim/pricing/, expanded
	Overrides string            `yaml:"overrides"` // optional, expanded
	Aliases   map[string]string `yaml:"aliases"`
}

// Duration is a time.Duration that additionally accepts a "d" (days) suffix,
// as in "90d". It marshals back to the same syntax it was given when possible.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string {
	v := time.Duration(d)
	if v != 0 && v%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(v/(24*time.Hour)), 10) + "d"
	}
	return v.String()
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// ParseDuration parses a Go duration or an integer followed by "d".
func ParseDuration(s string) (Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseInt(n, 10, 64)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return Duration(time.Duration(days) * 24 * time.Hour), nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return Duration(v), nil
}
