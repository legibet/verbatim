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

// Options are the gateway's collaborators, all built from one configuration.
type Options struct {
	Config   *config.Config
	Routes   *route.Table
	Breakers *route.Breakers
	Pins     *affinity.Pins
	Rules    *affinity.Rules
	Store    *store.Store
	Models   *models.Cache
	Pricing  *pricing.Table // initial table; SetPricing replaces it; nil prices nothing
	Logger   *slog.Logger   // nil means slog.Default()
}

// Handler serves the data plane: the protocol entry points, the model
// listings, /healthz, and the Responses operations verbatim rejects. The UI
// is mounted next to it by the caller.
type Handler struct {
	cfg      *config.Config
	routes   *route.Table
	breakers *route.Breakers
	pins     *affinity.Pins
	rules    *affinity.Rules
	store    *store.Store
	models   *models.Cache
	pricing  atomic.Pointer[pricing.Table]
	log      *slog.Logger
	// transports holds each provider's transport; providers with the same
	// header_timeout share one, and with it their idle connections.
	transports map[string]*http.Transport
	inflight   chan struct{}  // semaphore of server.max_inflight forwarded requests
	exchanges  sync.WaitGroup // forwarded requests that have not yet recorded their row
	mux        *http.ServeMux
}

// New builds the handler and its upstream transports.
func New(o Options) *Handler {
	// A transport never asks for or decodes a content encoding, so the client
	// gets the provider's bytes, and it is used directly rather than through
	// a Client, so redirects are forwarded, not followed.
	byTimeout := map[time.Duration]*http.Transport{}
	transports := map[string]*http.Transport{}
	for name, p := range o.Config.Providers {
		d := p.HeaderTimeout.Std()
		if byTimeout[d] == nil {
			t := http.DefaultTransport.(*http.Transport).Clone()
			t.DisableCompression = true
			t.ResponseHeaderTimeout = d
			t.MaxIdleConnsPerHost = 32
			byTimeout[d] = t
		}
		transports[name] = byTimeout[d]
	}

	h := &Handler{
		cfg:        o.Config,
		routes:     o.Routes,
		breakers:   o.Breakers,
		pins:       o.Pins,
		rules:      o.Rules,
		store:      o.Store,
		models:     o.Models,
		log:        cmp.Or(o.Logger, slog.Default()),
		transports: transports,
		inflight:   make(chan struct{}, o.Config.Server.MaxInflight),
		mux:        http.NewServeMux(),
	}
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
	return Stats{
		Breakers:   h.breakers.Snapshot(),
		Pins:       h.pins.ByProvider(),
		PinsTotal:  h.pins.Len(),
		ModelsAge:  h.models.Ages(),
		LogDropped: h.store.Dropped(),
	}
}

// authorized checks the client key (design 4.1): any one of the four
// carriers matching is enough.
func (h *Handler) authorized(r *http.Request) bool {
	key := []byte(h.cfg.Server.APIKey)
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
	if !h.authorized(r) {
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
	names := h.models.Names(r.Context())
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
	if !h.authorized(r) {
		writeError(w, protocol.Lookup(protocol.Gemini), http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
		return
	}
	type model struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	names := h.models.Names(r.Context())
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
