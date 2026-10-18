// Package proxy is the data plane: it serves the protocol entry points,
// runs the attempt loop with failover, circuit breaking, session affinity
// and response ownership, streams the committed response back unchanged,
// and records one log row per request (design 4, 5, 7, 8, 9, 10, 11).
package proxy

import (
	"cmp"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"verbatim/internal/affinity"
	"verbatim/internal/config"
	"verbatim/internal/models"
	"verbatim/internal/pricing"
	"verbatim/internal/protocol"
	"verbatim/internal/route"
	"verbatim/internal/store"
)

// Options are the gateway's collaborators that outlive a configuration.
type Options struct {
	Config  *config.Config // the initial configuration; SetConfig replaces it
	Models  *models.Cache  // the model lists for Config
	Pins    *affinity.Pins
	Store   *store.Store
	Pricing *pricing.Table // initial table; SetPricing replaces it; nil prices nothing
	Logger  *slog.Logger   // nil means slog.Default()
}

// Handler serves the data plane: the protocol entry points, the model
// listings, /healthz, and the Responses operations verbatim rejects. The UI
// is mounted next to it by the caller.
type Handler struct {
	rt      atomic.Pointer[runtime]
	pins    *affinity.Pins
	store   *store.Store
	pricing atomic.Pointer[pricing.Table]
	log     *slog.Logger
	// inflight counts forwarded requests against server.max_inflight. It
	// lives here rather than in the runtime so that requests still running
	// after a reload keep counting.
	inflight  atomic.Int64
	exchanges sync.WaitGroup // forwarded requests that have not yet recorded their row
	mux       *http.ServeMux
}

// runtime is everything the handler builds from one configuration. A reload
// replaces it whole; a request keeps the one it started with, so its attempts
// and limits all come from one configuration.
type runtime struct {
	cfg      *config.Config
	routes   *route.Table
	rules    *affinity.Rules
	breakers *route.Breakers
	models   *models.Cache
	// transports holds each endpoint's transport, by config.EndpointName;
	// endpoints with the same header_timeout share one, and with it their
	// idle connections.
	transports map[string]*http.Transport
}

// New builds the handler for o.Config.
func New(o Options) *Handler {
	h := &Handler{
		pins:  o.Pins,
		store: o.Store,
		log:   cmp.Or(o.Logger, slog.Default()),
		mux:   http.NewServeMux(),
	}
	h.SetConfig(o.Config, o.Models)
	h.pricing.Store(o.Pricing)

	h.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("GET /v1/models", h.listModels)
	h.mux.HandleFunc("GET /v1beta/models", h.listGeminiModels)
	// Responses resource operations and the WebSocket transport (a GET on
	// /v1/responses) are not supported in version 1 (design 4.4).
	for _, pattern := range []string{
		"GET /v1/responses",
		"GET /v1/responses/{id}",
		"DELETE /v1/responses/{id}",
		"POST /v1/responses/{id}/cancel",
		"GET /v1/responses/{id}/input_items",
		"POST /v1/responses/compact",
	} {
		h.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeError(w, protocol.Lookup(protocol.Responses), http.StatusNotFound, "unsupported_operation",
				r.Method+" "+r.URL.Path+" is not supported by verbatim")
		})
	}
	h.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return h
}

// SetConfig installs a configuration, validated by config.Load, and the model
// lists built for it. Requests already running finish under the previous
// one. Pins are kept; breaker state and upstream connections start afresh.
func (h *Handler) SetConfig(cfg *config.Config, mc *models.Cache) {
	// A transport never asks for or decodes a content encoding, so the client
	// gets the provider's bytes, and it is used directly rather than through
	// a Client, so redirects are forwarded, not followed.
	byTimeout := map[time.Duration]*http.Transport{}
	transports := map[string]*http.Transport{}
	for name, p := range cfg.Providers {
		for proto, e := range p.Endpoints {
			d := e.HeaderTimeout.Std()
			if byTimeout[d] == nil {
				t := http.DefaultTransport.(*http.Transport).Clone()
				t.DisableCompression = true
				t.ResponseHeaderTimeout = d
				t.MaxIdleConnsPerHost = 32
				byTimeout[d] = t
			}
			transports[config.EndpointName(name, proto)] = byTimeout[d]
		}
	}
	h.rt.Store(&runtime{
		cfg:        cfg,
		routes:     route.New(cfg),
		rules:      affinity.NewRules(cfg.Affinity),
		breakers:   route.NewBreakers(cfg.Breaker.FailureThreshold, cfg.Breaker.Cooldown.Std()),
		models:     mc,
		transports: transports,
	})
}

// Config returns the configuration new requests are served with.
func (h *Handler) Config() *config.Config { return h.rt.Load().cfg }

// ServeHTTP tags every response with a request id, sends the protocol entry
// points to the attempt loop, and everything else to the internal mux. The
// Gemini entry point is not served in version 1 (design 2).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := newRequestID()
	w.Header().Set("X-Verbatim-Request-Id", id)
	if p, op, _, ok := protocol.MatchPath(r.Method, r.URL.Path); ok && p.ID() != protocol.Gemini {
		h.forward(w, r, id, p, op)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// Wait waits, at most timeout, until every forwarded request has handed its
// log row to the store, and reports whether they all did. A server that cut
// connections at shutdown calls it before closing the store, so that the
// handlers still finishing do not lose their rows.
func (h *Handler) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		h.exchanges.Wait()
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// SetPricing replaces the price table used for new log rows.
func (h *Handler) SetPricing(t *pricing.Table) { h.pricing.Store(t) }

// Stats returns a snapshot of the gateway's in-memory state.
func (h *Handler) Stats() Stats {
	rt := h.rt.Load()
	return Stats{
		Breakers:   rt.breakers.Snapshot(),
		Pins:       h.pins.ByEndpoint(),
		PinsTotal:  h.pins.Len(),
		ModelsAge:  rt.models.Ages(),
		LogDropped: h.store.Dropped(),
	}
}

// authorized checks the client key (design 4.1): any one of the four
// carriers matching is enough.
func (rt *runtime) authorized(r *http.Request) bool {
	key := []byte(rt.cfg.Server.APIKey)
	if len(key) == 0 {
		return true
	}
	match := func(v string) bool { return v != "" && subtle.ConstantTimeCompare([]byte(v), key) == 1 }
	if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") && match(token) {
		return true
	}
	return match(r.Header.Get("X-Api-Key")) || match(r.Header.Get("X-Goog-Api-Key")) || match(r.URL.Query().Get("key"))
}

// listModels serves the OpenAI/Anthropic superset listing (design 6.5).
func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	rt := h.rt.Load()
	if !rt.authorized(r) {
		writeError(w, protocol.Lookup(protocol.Chat), http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
		return
	}
	type model struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Created     int64  `json:"created"`
		OwnedBy     string `json:"owned_by"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	}
	names := rt.models.Names(r.Context())
	data := make([]model, 0, len(names))
	for _, n := range names {
		data = append(data, model{ID: n, Object: "model", OwnedBy: "verbatim", Type: "model", DisplayName: n, CreatedAt: "1970-01-01T00:00:00Z"})
	}
	var first, last *string
	if len(names) > 0 {
		first, last = &names[0], &names[len(names)-1]
	}
	writeJSON(w, http.StatusOK, struct {
		Object  string  `json:"object"`
		Data    []model `json:"data"`
		HasMore bool    `json:"has_more"`
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
	}{"list", data, false, first, last})
}

// listGeminiModels serves the listing in Gemini's shape.
func (h *Handler) listGeminiModels(w http.ResponseWriter, r *http.Request) {
	rt := h.rt.Load()
	if !rt.authorized(r) {
		writeError(w, protocol.Lookup(protocol.Gemini), http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
		return
	}
	type model struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	names := rt.models.Names(r.Context())
	list := make([]model, 0, len(names))
	for _, n := range names {
		list = append(list, model{Name: "models/" + n, DisplayName: n})
	}
	writeJSON(w, http.StatusOK, map[string][]model{"models": list})
}

// writeError writes a verbatim-generated error in the protocol's native
// shape, marked with x-verbatim-error (design 10).
func writeError(w http.ResponseWriter, p protocol.Protocol, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Verbatim-Error", code)
	w.WriteHeader(status)
	w.Write(p.ErrorBody(status, code, msg))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}
