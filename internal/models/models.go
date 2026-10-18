// Package models keeps the provider endpoints' own model lists and assembles
// the model listing served at /v1/models and /v1beta/models (design 6.5).
package models

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"verbatim/internal/config"
	"verbatim/internal/protocol"
	"verbatim/internal/route"
)

const (
	// maxPages bounds the pages read of one model list (design 6.5).
	maxPages = 10
	// maxListBytes bounds one page of an endpoint's model list.
	maxListBytes = 16 << 20
	// retryInterval is how soon a failed fetch is retried, when the TTL is
	// longer; an endpoint unreachable at startup should not stay unlisted for
	// a whole TTL.
	retryInterval = time.Minute
)

// Cache holds the model list of every provider endpoint that discovery reads
// and refreshes each one in the background. Readers wait only for the first
// round of fetches after Start, never on a refresh.
type Cache struct {
	cfg       *config.Config
	routes    *route.Table
	transport http.RoundTripper
	ttl       time.Duration
	timeout   time.Duration
	endpoints []endpoint // endpoints discovery reads, sorted

	mu    sync.Mutex
	lists map[string]list // by config.EndpointName
	warm  chan struct{}   // closed when Start's first round of fetches has ended
}

type endpoint struct{ provider, proto string }

func (e endpoint) name() string { return config.EndpointName(e.provider, e.proto) }

type list struct {
	ids     []string
	fetched time.Time
}

// New returns a cache for cfg. Only endpoints that a discovering pattern
// route uses without a literal model are ever fetched: no other list can
// contribute to the listing. A nil transport means http.DefaultTransport.
func New(cfg *config.Config, transport http.RoundTripper, ttl, timeout time.Duration) *Cache {
	if transport == nil {
		transport = http.DefaultTransport
	}
	need := map[endpoint]bool{}
	for _, r := range cfg.Routes {
		if !discovers(r) {
			continue
		}
		for _, t := range r.Targets {
			if t.Model != "" && config.IsExact(t.Model) {
				continue
			}
			provider, _ := t.Split()
			for _, proto := range cfg.Protocols(t) {
				need[endpoint{provider, proto}] = true
			}
		}
	}
	return &Cache{
		cfg:       cfg,
		routes:    route.New(cfg),
		transport: transport,
		ttl:       ttl,
		timeout:   timeout,
		endpoints: slices.SortedFunc(maps.Keys(need), func(a, b endpoint) int { return strings.Compare(a.name(), b.name()) }),
		lists:     map[string]list{},
	}
}

// discovers reports whether a route contributes provider-listed names.
func discovers(r config.Route) bool {
	return !config.IsExact(r.Model) && (r.Discover == nil || *r.Discover)
}

// Start fetches every list in the background now and again every TTL until
// ctx is done. A failed fetch keeps the previous list and logs a warning.
func (c *Cache) Start(ctx context.Context) {
	warm := make(chan struct{})
	c.mu.Lock()
	c.warm = warm
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range c.endpoints {
		wg.Add(1)
		go c.keepFresh(ctx, e, wg.Done)
	}
	go func() {
		wg.Wait()
		close(warm)
	}()
}

// keepFresh refreshes one endpoint's list until ctx is done, calling warmed
// once the first fetch has ended. It warns when fetches start failing, not on
// every retry: an endpoint with no model list would otherwise warn every
// retryInterval for good.
func (c *Cache) keepFresh(ctx context.Context, e endpoint, warmed func()) {
	failing := false
	for {
		wait := c.ttl
		err := c.refresh(ctx, e)
		if warmed != nil {
			warmed()
			warmed = nil
		}
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case err != nil:
			if !failing {
				slog.Warn("model list fetch failed; keeping the previous list and retrying", "endpoint", e.name(), "err", err)
			}
			failing = true
			wait = min(c.ttl, retryInterval)
		case failing:
			slog.Info("model list fetch succeeded again", "endpoint", e.name())
			failing = false
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// refresh fetches one endpoint's list and replaces the cached one.
func (c *Cache) refresh(ctx context.Context, e endpoint) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ids, err := c.fetch(ctx, e)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.lists[e.name()] = list{ids: ids, fetched: time.Now()}
	c.mu.Unlock()
	return nil
}

// fetch reads the endpoint's model list, following the next-page cursor for
// at most maxPages pages.
func (c *Cache) fetch(ctx context.Context, e endpoint) ([]string, error) {
	p := c.cfg.Providers[e.provider]
	ep := p.Endpoints[e.proto]
	proto := protocol.Lookup(protocol.ID(e.proto))
	var all []string
	after := ""
	for page := 1; page <= maxPages; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.BaseURL+proto.ModelsPath(after), nil)
		if err != nil {
			return nil, err
		}
		where := "GET " + req.URL.Redacted()
		proto.DefaultHeaders(req.Header)
		Authorize(req.Header, p, ep)
		// A configured Accept-Encoding would stop the transport from
		// decompressing the reply, and the list would arrive as encoded
		// bytes; without it the transport negotiates and decodes on its own.
		req.Header.Del("Accept-Encoding")
		resp, err := c.transport.RoundTrip(req)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("%s: HTTP %d", where, resp.StatusCode)
		}
		ids, next := proto.ModelList(body)
		if ids == nil {
			return nil, fmt.Errorf("%s: response is not a model list", where)
		}
		all = append(all, ids...)
		if next == "" {
			break
		}
		after = next
	}
	return all, nil
}

// Names assembles the listing (design 6.5): per route in configuration
// order, an exact route contributes its own name, and a discovering pattern
// route contributes the client names its targets' endpoints list: an id that
// matches the pattern for a target without a model, or the pattern with its
// '*' replaced by what the target model's '*' matched in the id. Targets with
// a literal model contribute nothing. A name is kept only if routing would
// send it to this route for that endpoint's protocol. The first occurrence of
// a name wins.
//
// A call made while Start's first round of fetches is still running waits
// for it, or until ctx is done, so that a client asking right after startup
// gets the full listing; models.timeout bounds that round.
func (c *Cache) Names(ctx context.Context) []string {
	c.mu.Lock()
	warm := c.warm
	c.mu.Unlock()
	if warm != nil {
		select {
		case <-warm:
		case <-ctx.Done():
		}
	}

	c.mu.Lock()
	lists := make(map[string][]string, len(c.lists))
	for name, l := range c.lists {
		lists[name] = l.ids
	}
	c.mu.Unlock()

	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, r := range c.cfg.Routes {
		if config.IsExact(r.Model) {
			add(r.Model)
			continue
		}
		if !discovers(r) {
			continue
		}
		for _, t := range r.Targets {
			if t.Model != "" && config.IsExact(t.Model) {
				continue
			}
			provider, _ := t.Split()
			for _, proto := range c.cfg.Protocols(t) {
				for _, id := range lists[config.EndpointName(provider, proto)] {
					name := id
					if t.Model == "" {
						if !config.MatchGlob(r.Model, id) {
							continue
						}
					} else {
						s, ok := config.Capture(t.Model, id)
						if !ok {
							continue
						}
						name = strings.Replace(r.Model, "*", s, 1)
					}
					if name == "" { // no request can name it
						continue
					}
					if owner, _, ok := c.routes.Match(protocol.ID(proto), name); ok && owner == r.Model {
						add(name)
					}
				}
			}
		}
	}
	return out
}

// Ages returns, per endpoint name, the age of its cached list. Endpoints
// never fetched successfully have no entry.
func (c *Cache) Ages() map[string]time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	out := make(map[string]time.Duration, len(c.lists))
	for name, l := range c.lists {
		out[name] = now.Sub(l.fetched)
	}
	return out
}

// Authorize applies a provider's configured headers and then the
// endpoint's authentication header to h (design 4.2 step 4). A nil header
// value removes the header; the authentication header always wins. Headers
// are applied in sorted order so that two names differing only in case
// resolve the same way every time.
func Authorize(h http.Header, p config.Provider, e config.Endpoint) {
	for _, name := range slices.Sorted(maps.Keys(p.Headers)) {
		if v := p.Headers[name]; v != nil {
			h.Set(name, *v)
		} else {
			h.Del(name)
		}
	}
	switch protocol.AuthScheme(e.Auth) {
	case protocol.AuthBearer:
		h.Set("Authorization", "Bearer "+p.Key)
	case protocol.AuthXAPIKey:
		h.Set("X-Api-Key", p.Key)
	case protocol.AuthXGoogAPIKey:
		h.Set("X-Goog-Api-Key", p.Key)
	}
}
