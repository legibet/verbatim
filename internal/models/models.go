// Package models keeps the providers' own model lists and assembles the
// model listing served at /v1/models and /v1beta/models (design 6.5).
package models

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"verbatim/internal/config"
	"verbatim/internal/protocol"
	"verbatim/internal/route"
)

const (
	// maxPages bounds Anthropic-style pagination (design 6.5).
	maxPages = 10
	// maxListBytes bounds one page of a provider's model list.
	maxListBytes = 16 << 20
	// retryInterval is how soon a failed fetch is retried, when the TTL is
	// longer; a provider unreachable at startup should not stay unlisted for
	// a whole TTL.
	retryInterval = time.Minute
)

// Cache holds the model list of every provider that discovery reads and
// refreshes each one in the background. Readers wait only for the first
// round of fetches after Start, never on a refresh.
type Cache struct {
	cfg       *config.Config
	routes    *route.Table
	transport http.RoundTripper
	ttl       time.Duration
	timeout   time.Duration
	providers []string // providers discovery reads, sorted

	mu    sync.Mutex
	lists map[string]list
	warm  chan struct{} // closed when Start's first round of fetches has ended
}

type list struct {
	ids     []string
	fetched time.Time
}

// New returns a cache for cfg. Only providers that a discovering pattern
// route uses without a model rewrite are ever fetched: no other list can
// contribute to the listing. A nil transport means http.DefaultTransport.
func New(cfg *config.Config, transport http.RoundTripper, ttl, timeout time.Duration) *Cache {
	if transport == nil {
		transport = http.DefaultTransport
	}
	need := map[string]bool{}
	for _, r := range cfg.Routes {
		if !discovers(r) {
			continue
		}
		for _, t := range r.Targets {
			if t.Model == "" {
				need[t.Provider] = true
			}
		}
	}
	return &Cache{
		cfg:       cfg,
		routes:    route.New(cfg),
		transport: transport,
		ttl:       ttl,
		timeout:   timeout,
		providers: slices.Sorted(maps.Keys(need)),
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
	for _, name := range c.providers {
		wg.Add(1)
		go c.keepFresh(ctx, name, wg.Done)
	}
	go func() {
		wg.Wait()
		close(warm)
	}()
}

// keepFresh refreshes one provider's list until ctx is done, calling warmed
// once the first fetch has ended.
func (c *Cache) keepFresh(ctx context.Context, name string, warmed func()) {
	for {
		wait := c.ttl
		err := c.refresh(ctx, name)
		if warmed != nil {
			warmed()
			warmed = nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("model list fetch failed; keeping the previous list", "provider", name, "err", err)
			wait = min(c.ttl, retryInterval)
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

// refresh fetches one provider's list and replaces the cached one.
func (c *Cache) refresh(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ids, err := c.fetch(ctx, c.cfg.Providers[name])
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.lists[name] = list{ids: ids, fetched: time.Now()}
	c.mu.Unlock()
	return nil
}

// fetch reads the provider's model list, following has_more through ?after_id=
// for at most maxPages pages.
func (c *Cache) fetch(ctx context.Context, p config.Provider) ([]string, error) {
	proto := protocol.Lookup(protocol.ID(p.Type))
	var all []string
	after := ""
	for page := 1; page <= maxPages; page++ {
		u := p.BaseURL + proto.ModelsSuffix()
		if after != "" {
			u += "?after_id=" + url.QueryEscape(after)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		where := "GET " + req.URL.Redacted()
		proto.DefaultHeaders(req.Header)
		Authorize(req.Header, p)
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
		ids, more, last := proto.ModelList(body)
		if ids == nil {
			return nil, fmt.Errorf("%s: response is not a model list", where)
		}
		all = append(all, ids...)
		if !more {
			break
		}
		after = last
	}
	return all, nil
}

// Names assembles the listing (design 6.5): per route in configuration
// order, an exact route contributes its own name, a discovering pattern route
// contributes the names its rewrite-free targets' providers list that match
// the pattern and that routing would send to this route for that target's
// protocol. The first occurrence of a name wins.
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
			if t.Model != "" {
				continue
			}
			proto := protocol.ID(c.cfg.Providers[t.Provider].Type)
			for _, id := range lists[t.Provider] {
				if !config.MatchGlob(r.Model, id) {
					continue
				}
				if owner, _, ok := c.routes.Match(proto, id); ok && owner == r.Model {
					add(id)
				}
			}
		}
	}
	return out
}

// Ages returns, per provider, the age of its cached list. Providers never
// fetched successfully have no entry.
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

// Authorize applies a provider's configured headers and then its
// authentication header to h (design 4.2 step 4). A nil header value
// removes the header; the authentication header always wins. Headers are
// applied in sorted order so that two names differing only in case resolve
// the same way every time.
func Authorize(h http.Header, p config.Provider) {
	for _, name := range slices.Sorted(maps.Keys(p.Headers)) {
		if v := p.Headers[name]; v != nil {
			h.Set(name, *v)
		} else {
			h.Del(name)
		}
	}
	switch protocol.AuthScheme(p.Auth) {
	case protocol.AuthBearer:
		h.Set("Authorization", "Bearer "+p.Key)
	case protocol.AuthXAPIKey:
		h.Set("X-Api-Key", p.Key)
	case protocol.AuthXGoogAPIKey:
		h.Set("X-Goog-Api-Key", p.Key)
	}
}
