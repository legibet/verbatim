package config

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Warning is a configuration problem that does not stop the server.
type Warning struct {
	Path string // YAML path of the offending field
	Msg  string
}

func (w Warning) String() string { return w.Path + ": " + w.Msg }

// Load reads the file at path, expands ${ENV} and a leading ~/, applies
// defaults, and validates. Validation problems are returned as one error that
// lists every problem found, one per line, each naming its YAML path.
func Load(path string) (*Config, []Warning, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data, path)
}

// LoadLenient reads the file like Load but expands an unset ${VAR} to the
// empty string and skips validation except defaults and ~/ expansion. It
// is for commands that only read the log or the price table.
func LoadLenient(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, _, err := parse(data, path, true)
	return cfg, err
}

// Parse is Load on bytes; path is used in messages only.
func Parse(data []byte, path string) (*Config, []Warning, error) {
	return parse(data, path, false)
}

// parse checks the schema (unknown keys, durations, types) in both modes;
// lenient only changes how unset variables expand and skips the checker.
func parse(data []byte, path string, lenient bool) (*Config, []Warning, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}

	p := preparer{lenient: lenient, done: map[*yaml.Node]bool{}}
	p.prepare(&root, reflect.TypeFor[Config](), "")
	if len(p.problems) > 0 {
		return nil, nil, invalid(path, p.problems)
	}

	// Decoding onto the defaults keeps them for absent keys while an explicit
	// value, including 0, is kept as written and validated.
	cfg := defaults()
	if root.Kind == yaml.DocumentNode {
		if err := root.Decode(cfg); err != nil {
			if te, ok := errors.AsType[*yaml.TypeError](err); ok {
				return nil, nil, invalid(path, te.Errors)
			}
			return nil, nil, invalid(path, []string{err.Error()})
		}
	}
	if problems := expandPaths(cfg); len(problems) > 0 {
		return nil, nil, invalid(path, problems)
	}
	if lenient {
		return cfg, nil, nil
	}

	var c checker
	c.server(cfg)
	c.providers(cfg)
	c.routes(cfg)
	c.affinity(cfg)
	c.rest(cfg)
	if len(c.problems) > 0 {
		return nil, nil, invalid(path, c.problems)
	}
	return cfg, c.warnings, nil
}

func invalid(path string, problems []string) error {
	return fmt.Errorf("%s: invalid configuration:\n  %s", path, strings.Join(problems, "\n  "))
}

func defaults() *Config {
	return &Config{
		Server: Server{
			Listen:        "127.0.0.1:8080",
			MaxBodyBytes:  32 << 20,
			MaxInflight:   64,
			HeaderTimeout: Duration(60 * time.Second),
			IdleTimeout:   Duration(120 * time.Second),
		},
		Affinity: Affinity{TTL: Duration(time.Hour), Mode: "prefer", Fallback: "prefix_hash"},
		Breaker:  Breaker{FailureThreshold: 3, Cooldown: Duration(30 * time.Second)},
		Models:   Models{CacheTTL: Duration(time.Hour), Timeout: Duration(10 * time.Second)},
		Log: Log{
			Path:              "~/.verbatim/verbatim.db",
			Retention:         Duration(90 * 24 * time.Hour),
			ErrorExcerptBytes: 4096,
		},
		Pricing: Pricing{Cache: "~/.verbatim/pricing/"},
	}
}

// UnmarshalYAML fills the target defaults before decoding, so an explicit
// priority or weight of 0 is seen by validation instead of becoming 1.
func (t *Target) UnmarshalYAML(n *yaml.Node) error {
	type plain Target
	p := plain{Priority: 1, Weight: 1}
	if err := n.Decode(&p); err != nil {
		return err
	}
	*t = Target(p)
	return nil
}

var durationType = reflect.TypeFor[Duration]()

// preparer walks the YAML tree against the schema type before decoding. It
// expands ${VAR} in string values, reports keys the schema does not define,
// and checks durations, so that each problem carries a line number and a
// YAML path. Unknown keys are checked here because yaml.v3 offers KnownFields
// only on Decoder, not on Node.Decode.
type preparer struct {
	lenient bool // an unset variable expands to ""
	// done holds the nodes already prepared, so an anchored node reached
	// again through an alias or merge key is not expanded twice.
	done     map[*yaml.Node]bool
	problems []string
}

func (p *preparer) prepare(n *yaml.Node, t reflect.Type, path string) {
	if p.done[n] {
		return
	}
	p.done[n] = true
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			p.prepare(c, t, path)
		}
	case yaml.AliasNode:
		p.prepare(n.Alias, t, path)
	case yaml.ScalarNode:
		if n.ShortTag() == "!!str" && strings.Contains(n.Value, "${") {
			v, err := expandEnv(n.Value, p.lenient)
			if err != nil {
				p.problems = append(p.problems, fmt.Sprintf("line %d: %s: %v", n.Line, path, err))
				return
			}
			n.Value = v
		}
		if t == durationType {
			if _, err := ParseDuration(n.Value); err != nil {
				p.problems = append(p.problems, fmt.Sprintf("line %d: %s: %v", n.Line, path, err))
			}
		}
	case yaml.SequenceNode:
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Interface {
			return // a type mismatch, reported by the decoder
		}
		elem := t
		if t.Kind() == reflect.Slice {
			elem = t.Elem()
		}
		for i, c := range n.Content {
			p.prepare(c, elem, fmt.Sprintf("%s[%d]", path, i))
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.ShortTag() == "!!merge" {
				// The merged mappings become part of this one, so they are
				// checked against the same type at the same path.
				if v.Kind == yaml.SequenceNode {
					for _, m := range v.Content {
						p.prepare(m, t, path)
					}
				} else {
					p.prepare(v, t, path)
				}
				continue
			}
			child := k.Value
			if path != "" {
				child = path + "." + k.Value
			}
			switch t.Kind() {
			case reflect.Struct:
				f, ok := fieldByYAMLName(t, k.Value)
				if !ok {
					p.problems = append(p.problems, fmt.Sprintf("line %d: %s: unknown field %q", k.Line, child, k.Value))
					continue
				}
				p.prepare(v, f.Type, child)
			case reflect.Map:
				p.prepare(v, t.Elem(), child)
			case reflect.Interface:
				p.prepare(v, t, child)
			}
		}
	}
}

func fieldByYAMLName(t reflect.Type, name string) (reflect.StructField, bool) {
	for f := range t.Fields() {
		if tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); tag == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// expandEnv replaces each ${NAME} with the value of the environment variable
// NAME. An unset variable is an error unless lenient, when it expands to the
// empty string. Substituted values are not rescanned.
func expandEnv(s string, lenient bool) (string, error) {
	var b strings.Builder
	for {
		before, after, found := strings.Cut(s, "${")
		b.WriteString(before)
		if !found {
			return b.String(), nil
		}
		name, rest, ok := strings.Cut(after, "}")
		if !ok {
			return "", errors.New(`unterminated "${"`)
		}
		if name == "" {
			return "", errors.New(`empty variable name in "${}"`)
		}
		v, ok := os.LookupEnv(name)
		if !ok && !lenient {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		b.WriteString(v)
		s = rest
	}
}

// expandPaths replaces a leading "~/" in the file paths with the user's home
// directory and returns any problems.
func expandPaths(cfg *Config) []string {
	var problems []string
	for _, f := range []struct {
		path  string
		value *string
	}{
		{"log.path", &cfg.Log.Path},
		{"pricing.cache", &cfg.Pricing.Cache},
		{"pricing.overrides", &cfg.Pricing.Overrides},
	} {
		rest, ok := strings.CutPrefix(*f.value, "~/")
		if !ok {
			continue
		}
		home, err := os.UserHomeDir()
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: expand ~/: %v", f.path, err))
			continue
		}
		*f.value = filepath.Join(home, rest)
	}
	return problems
}

// checker validates a decoded configuration, filling the defaults that depend
// on other fields, and collects every problem rather than stopping at the first.
type checker struct {
	problems []string
	warnings []Warning
}

func (c *checker) errorf(path, format string, args ...any) {
	c.problems = append(c.problems, path+": "+fmt.Sprintf(format, args...))
}

func (c *checker) warnf(path, format string, args ...any) {
	c.warnings = append(c.warnings, Warning{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (c *checker) server(cfg *Config) {
	s := &cfg.Server
	if !isFieldValue(s.APIKey) {
		c.errorf("server.api_key", "contains a control character")
	}
	host, port, err := net.SplitHostPort(s.Listen)
	switch {
	case err != nil || port == "":
		c.errorf("server.listen", "must be host:port, got %q", s.Listen)
	case s.APIKey == "" && !isLoopback(host):
		c.warnf("server.api_key", "not set and server.listen %q is not a loopback address; the server will accept unauthenticated requests", s.Listen)
	}
	if s.MaxBodyBytes <= 0 {
		c.errorf("server.max_body_bytes", "must be greater than 0")
	}
	if s.MaxInflight <= 0 {
		c.errorf("server.max_inflight", "must be greater than 0")
	}
	if s.HeaderTimeout <= 0 {
		c.errorf("server.header_timeout", "must be greater than 0")
	}
	if s.IdleTimeout <= 0 {
		c.errorf("server.idle_timeout", "must be greater than 0")
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// protocols are the protocol ids, which name endpoints and affinity matches.
var protocols = []string{"chat", "responses", "anthropic", "images"}

func (c *checker) providers(cfg *Config) {
	if len(cfg.Providers) == 0 {
		c.errorf("providers", "at least one provider is required")
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[name]
		path := "providers." + name
		if strings.Contains(name, ".") {
			c.errorf(path, `a provider name cannot contain ".", which separates provider and protocol in a target`)
		}
		if !isFieldValue(p.Key) {
			c.errorf(path+".key", "contains a control character")
		}
		for _, h := range slices.Sorted(maps.Keys(p.Headers)) {
			hpath := path + ".headers." + h
			switch {
			case !isToken(h):
				c.errorf(hpath, "not a valid header name")
			case slices.ContainsFunc([]string{"Content-Length", "Host", "Transfer-Encoding", "Connection"},
				func(m string) bool { return strings.EqualFold(h, m) }):
				c.errorf(hpath, "managed by verbatim and cannot be set")
			}
			if v := p.Headers[h]; v != nil && !isFieldValue(*v) {
				c.errorf(hpath, "value contains a control character")
			}
		}

		if len(p.Endpoints) == 0 {
			c.errorf(path+".endpoints", "at least one endpoint is required")
		}
		usesKey := false
		for _, proto := range slices.Sorted(maps.Keys(p.Endpoints)) {
			e := p.Endpoints[proto]
			epath := path + ".endpoints." + proto
			var auth string
			switch proto {
			case "chat", "responses", "images":
				auth = "bearer"
			case "anthropic":
				auth = "x-api-key"
			default:
				c.errorf(epath, "unknown protocol; must be one of %s", strings.Join(protocols, ", "))
			}
			if e.Auth == "" {
				e.Auth = auth
			}
			switch e.Auth {
			case "bearer", "x-api-key":
				usesKey = true
			case "none", "":
				// "" is an unknown protocol, already reported.
			default:
				c.errorf(epath+".auth", "must be one of bearer, x-api-key, none, got %q", e.Auth)
			}

			u, err := url.Parse(e.BaseURL)
			switch {
			case e.BaseURL == "":
				c.errorf(epath+".base_url", "required")
			case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
				c.errorf(epath+".base_url", "must be an absolute http or https URL")
			case strings.ContainsAny(e.BaseURL, "?#"):
				c.errorf(epath+".base_url", "must not contain a query string or fragment")
			}
			// Operation suffixes start with "/", so a trailing slash would double it.
			e.BaseURL = strings.TrimRight(e.BaseURL, "/")

			switch {
			case e.HeaderTimeout < 0:
				c.errorf(epath+".header_timeout", "must be greater than 0")
			case e.HeaderTimeout == 0:
				e.HeaderTimeout = cfg.Server.HeaderTimeout
			}
			p.Endpoints[proto] = e
		}
		switch {
		case usesKey && p.Key == "":
			c.errorf(path+".key", "required unless every endpoint's auth is none")
		case !usesKey && p.Key != "" && len(p.Endpoints) > 0:
			c.warnf(path+".key", "ignored because every endpoint's auth is none")
		}

		_, images := p.Endpoints["images"]
		if images && len(p.Overrides) > 0 {
			c.errorf(path+".overrides", "not supported on a provider with an images endpoint: overrides edit JSON, and an images request may be multipart; give the images endpoint a provider of its own")
		}
		_, anthropic := p.Endpoints["anthropic"]
		c.overrides(path, anthropic, p.Overrides)
		for i, s := range p.FailoverOn {
			if s < 400 || s > 599 {
				c.errorf(fmt.Sprintf("%s.failover_on[%d]", path, i), "must be a 4xx or 5xx status, got %d", s)
			}
		}
	}
}

func (c *checker) overrides(path string, anthropic bool, ops []Override) {
	var seen [][]string // key segments per override, nil when invalid
	for i, o := range ops {
		opath := fmt.Sprintf("%s.overrides[%d]", path, i)
		var target, field string
		switch {
		case o.Delete != "" && o.Set == nil:
			target, field = o.Delete, opath+".delete"
		case o.Delete == "" && o.Set != nil:
			target, field = o.Set.Path, opath+".set.path"
			if err := checkJSONValue(o.Set.Value); err != nil {
				c.errorf(opath+".set.value", "%v", err)
			}
		default:
			c.errorf(opath, "must have exactly one of delete or set")
			seen = append(seen, nil)
			continue
		}
		segs, err := pathSegments(target)
		if err != nil {
			c.errorf(field, "%v", err)
			seen = append(seen, nil)
			continue
		}
		seen = append(seen, segs)
		if slices.Contains(segs, "model") || slices.Contains(segs, "stream") {
			c.errorf(field, "%q: model and stream cannot be overridden; they drive routing and usage parsing", target)
		}
		if anthropic && slices.Contains([]string{"system", "tools", "messages"}, segs[0]) {
			c.errorf(field, "%q: system, tools and messages cannot be overridden on a provider with an anthropic endpoint; it would change the preserved-thinking prefix", target)
		}
		for j, prev := range seen[:i] {
			if prev != nil && (hasPrefix(prev, segs) || hasPrefix(segs, prev)) {
				c.errorf(field, "%q overlaps overrides[%d]", target, j)
			}
		}
	}
}

// checkJSONValue rejects set values whose JSON encoding would not say what
// the YAML says: mappings with non-string keys, unquoted timestamps (decoded
// as time.Time), and infinities or NaN.
func checkJSONValue(v any) error {
	switch v := v.(type) {
	case map[any]any:
		return errors.New("mapping keys must be strings")
	case time.Time:
		return errors.New("a timestamp is not a JSON value; quote it to send a string")
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return errors.New("infinity and NaN are not JSON values")
		}
	case map[string]any:
		for _, e := range v {
			if err := checkJSONValue(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range v {
			if err := checkJSONValue(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// pathSegments splits an sjson path into its key segments, unescaped, so that
// "a\.model" is the single key "a.model". Only simple paths are accepted:
// sjson cannot delete through gjson's wildcard, query, or modifier syntax and
// sets through it only where a value already exists.
func pathSegments(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	var segs []string
	var cur strings.Builder
	for i := 0; i < len(path); i++ {
		switch ch := path[i]; ch {
		case '\\':
			i++
			if i == len(path) {
				return nil, fmt.Errorf("%q: ends with an escape", path)
			}
			cur.WriteByte(path[i])
		case '.':
			segs = append(segs, cur.String())
			cur.Reset()
		case '|', '#', '@', '*', '?':
			return nil, fmt.Errorf("%q: wildcards, queries and modifiers are not supported; use a plain dotted path", path)
		default:
			cur.WriteByte(ch)
		}
	}
	segs = append(segs, cur.String())
	for i, s := range segs {
		// A leading ':' only forces sjson to treat the key as an object key.
		s = strings.TrimPrefix(s, ":")
		if s == "" {
			return nil, fmt.Errorf("%q: empty path segment", path)
		}
		segs[i] = s
	}
	return segs, nil
}

func hasPrefix(prefix, segs []string) bool {
	return len(prefix) <= len(segs) && slices.Equal(prefix, segs[:len(prefix)])
}

// isToken reports whether s is a valid HTTP header field name (RFC 9110 token).
func isToken(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return r > 0x7e || r <= ' ' || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r)
	})
}

// isFieldValue reports whether s can be sent as an HTTP header field value:
// no control characters other than TAB, the rule Go's HTTP transport applies.
func isFieldValue(s string) bool {
	for i := range len(s) {
		if b := s[i]; b < ' ' && b != '\t' || b == 0x7f {
			return false
		}
	}
	return true
}

func (c *checker) routes(cfg *Config) {
	if len(cfg.Routes) == 0 {
		c.errorf("routes", "at least one route is required")
	}
	firstByModel := map[string]int{}
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		path := fmt.Sprintf("routes[%d]", i)
		if r.Model == "" {
			c.errorf(path+".model", "required")
		} else if j, dup := firstByModel[r.Model]; dup {
			c.errorf(path+".model", "%q duplicates routes[%d]", r.Model, j)
		} else {
			firstByModel[r.Model] = i
		}
		switch r.Balance {
		case "":
			r.Balance = "weighted"
		case "weighted", "round_robin":
		default:
			c.errorf(path+".balance", "must be weighted or round_robin, got %q", r.Balance)
		}
		if r.Discover != nil && IsExact(r.Model) {
			c.warnf(path+".discover", "ignored on an exact-name route")
		}
		if r.Discover == nil {
			r.Discover = new(true)
		}

		if len(r.Targets) == 0 {
			c.errorf(path+".targets", "at least one target is required")
		}
		// Two targets are the same when they send one model to one
		// endpoint: same provider, model, and protocol.
		type identity struct{ provider, model, proto string }
		firstTarget := map[identity]int{}
		for j, t := range r.Targets {
			tpath := fmt.Sprintf("%s.targets[%d]", path, j)
			name, proto := t.Split()
			p, ok := cfg.Providers[name]
			_, served := p.Endpoints[proto]
			switch {
			case t.Provider == "":
				c.errorf(tpath+".provider", "required")
			case !ok:
				c.errorf(tpath+".provider", "%q is not defined in providers", name)
			case proto == "" && name != t.Provider:
				c.errorf(tpath+".provider", "%q: a protocol must follow the \".\"", t.Provider)
			case proto != "" && !served:
				c.errorf(tpath+".provider", "provider %q has no %q endpoint", name, proto)
			}
			if t.Priority < 1 {
				c.errorf(tpath+".priority", "must be at least 1")
			}
			if t.Weight < 1 {
				c.errorf(tpath+".weight", "must be at least 1")
			}
			if n := strings.Count(t.Model, "*"); n > 1 {
				c.errorf(tpath+".model", "%q: at most one '*' is allowed", t.Model)
			} else if n == 1 && strings.Count(r.Model, "*") != 1 {
				c.errorf(tpath+".model", "%q: a '*' needs a route pattern with exactly one '*', got %q", t.Model, r.Model)
			}
			// An absent model sends the same name as the route's own pattern
			// would: the route name on an exact-name route, the client's name
			// on a pattern route.
			model := cmp.Or(t.Model, r.Model)
			for _, proto := range cfg.Protocols(t) {
				id := identity{name, model, proto}
				if k, dup := firstTarget[id]; dup {
					c.errorf(tpath, "duplicates targets[%d] (same provider, model and protocol %s)", k, proto)
					break
				}
				firstTarget[id] = j
			}
		}
	}

	// An exact-name route is unreachable when earlier pattern routes matching
	// its name already serve every protocol it has targets for
	// (docs/routing.md).
	for i, r := range cfg.Routes {
		if !IsExact(r.Model) {
			continue
		}
		left := routeProtocols(cfg, r)
		shadowedBy := -1
		for j, earlier := range cfg.Routes[:i] {
			if IsExact(earlier.Model) || !MatchGlob(earlier.Model, r.Model) {
				continue
			}
			if shadowedBy < 0 {
				shadowedBy = j
			}
			for p := range routeProtocols(cfg, earlier) {
				delete(left, p)
			}
		}
		if shadowedBy >= 0 && len(left) == 0 && len(r.Targets) > 0 {
			c.warnf(fmt.Sprintf("routes[%d]", i), "unreachable: earlier pattern routes starting at routes[%d] (%q) match %q for every protocol it serves",
				shadowedBy, cfg.Routes[shadowedBy].Model, r.Model)
		}
	}
}

func routeProtocols(cfg *Config, r Route) map[string]bool {
	set := map[string]bool{}
	for _, t := range r.Targets {
		for _, proto := range cfg.Protocols(t) {
			set[proto] = true
		}
	}
	return set
}

func (c *checker) affinity(cfg *Config) {
	a := &cfg.Affinity
	if a.TTL <= 0 {
		c.errorf("affinity.ttl", "must be greater than 0")
	}
	if a.Mode != "prefer" && a.Mode != "strict" {
		c.errorf("affinity.mode", "must be prefer or strict, got %q", a.Mode)
	}
	if a.Fallback != "prefix_hash" && a.Fallback != "none" {
		c.errorf("affinity.fallback", "must be prefix_hash or none, got %q", a.Fallback)
	}
	for i, r := range a.Rules {
		path := fmt.Sprintf("affinity.rules[%d]", i)
		if r.Match.Protocol != "" && !slices.Contains(protocols, r.Match.Protocol) {
			c.errorf(path+".match.protocol", "must be one of %s, got %q", strings.Join(protocols, ", "), r.Match.Protocol)
		}
		if len(r.Key) == 0 {
			c.errorf(path+".key", "at least one key source is required")
		}
		for j, k := range r.Key {
			kpath := fmt.Sprintf("%s.key[%d]", path, j)
			switch {
			case (k.Body == "") == (k.Header == ""):
				c.errorf(kpath, "must have exactly one of body or header")
			case k.Header != "" && !isToken(k.Header):
				c.errorf(kpath+".header", "not a valid header name")
			}
		}
	}
}

// rest checks the small sections that have no cross-references.
func (c *checker) rest(cfg *Config) {
	if cfg.Breaker.FailureThreshold < 1 {
		c.errorf("breaker.failure_threshold", "must be at least 1")
	}
	if cfg.Breaker.Cooldown <= 0 {
		c.errorf("breaker.cooldown", "must be greater than 0")
	}
	if cfg.Models.CacheTTL <= 0 {
		c.errorf("models.cache_ttl", "must be greater than 0")
	}
	if cfg.Models.Timeout <= 0 {
		c.errorf("models.timeout", "must be greater than 0")
	}
	if cfg.Log.Retention <= 0 {
		c.errorf("log.retention", "must be greater than 0")
	}
	if cfg.Log.ErrorExcerptBytes < 0 {
		c.errorf("log.error_excerpt_bytes", "must not be negative")
	}
	// Each source is cached under the last element of its URL path, with
	// ".json" added if missing; this must stay in step with cacheFiles in
	// internal/pricing, which refuses the same names at load time.
	cacheNames := map[string]int{}
	for i, s := range cfg.Pricing.Sources {
		field := fmt.Sprintf("pricing.sources[%d]", i)
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			c.errorf(field, "must be an absolute http or https URL")
			continue
		}
		name := path.Base(u.Path)
		if name == "/" || name == "." {
			c.errorf(field, "has no file name in its path, which names its cache file")
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			name += ".json"
		}
		if j, ok := cacheNames[name]; ok {
			c.errorf(field, "shares the cache file name %s with pricing.sources[%d]", name, j)
			continue
		}
		cacheNames[name] = i
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Pricing.Aliases)) {
		if cfg.Pricing.Aliases[name] == "" {
			c.errorf("pricing.aliases."+name, "must not be empty")
		}
	}

	if cfg.Log.Path == "" {
		c.errorf("log.path", "required")
	}
	if cfg.Pricing.Cache == "" {
		c.errorf("pricing.cache", "required")
	}
}

// Redacted returns a deep copy for display with secrets replaced by "***":
// server.api_key, every provider key, every non-nil headers value, and any
// userinfo or query in each endpoint's base_url. Override set values are
// shared with c; they are read-only after Load.
func (c *Config) Redacted() *Config {
	const mask = "***"
	r := *c
	if r.Server.APIKey != "" {
		r.Server.APIKey = mask
	}

	r.Providers = make(map[string]Provider, len(c.Providers))
	for name, p := range c.Providers {
		if p.Key != "" {
			p.Key = mask
		}
		eps := make(map[string]Endpoint, len(p.Endpoints))
		for proto, e := range p.Endpoints {
			if u, err := url.Parse(e.BaseURL); err == nil {
				u.User, u.RawQuery, u.ForceQuery = nil, "", false
				e.BaseURL = u.String()
			} else {
				e.BaseURL = mask
			}
			eps[proto] = e
		}
		p.Endpoints = eps
		if p.Headers != nil {
			h := make(map[string]*string, len(p.Headers))
			for k, v := range p.Headers {
				if v != nil {
					v = new(mask)
				}
				h[k] = v
			}
			p.Headers = h
		}
		p.Overrides = slices.Clone(p.Overrides)
		for i, o := range p.Overrides {
			if o.Set != nil {
				p.Overrides[i].Set = new(*o.Set)
			}
		}
		r.Providers[name] = p
	}

	r.Routes = slices.Clone(c.Routes)
	for i, rt := range r.Routes {
		r.Routes[i].Targets = slices.Clone(rt.Targets)
		if rt.Discover != nil {
			r.Routes[i].Discover = new(*rt.Discover)
		}
	}
	r.Affinity.Rules = slices.Clone(c.Affinity.Rules)
	for i, rule := range r.Affinity.Rules {
		r.Affinity.Rules[i].Key = slices.Clone(rule.Key)
	}
	r.Pricing.Sources = slices.Clone(c.Pricing.Sources)
	r.Pricing.Aliases = maps.Clone(c.Pricing.Aliases)
	return &r
}
