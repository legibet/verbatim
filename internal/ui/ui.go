// Package ui serves the read-only web UI (design 14): the embedded static
// assets under /ui/ and the JSON API under /ui/api/.
package ui

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"verbatim/internal/config"
	"verbatim/internal/proxy"
	"verbatim/internal/store"
)

// dist is the built web UI (web/ compiles into it); it is committed so a Go
// build never needs Node.
//
//go:embed dist
var dist embed.FS

// Options are the UI's data sources.
type Options struct {
	Store  *store.Store
	Stats  func() proxy.Stats // live gateway state
	Config *config.Config     // the loaded configuration; the UI only ever exposes its redacted copy
	APIKey string             // server.api_key; when set, /ui/api/* requires it as a bearer token
}

type server struct {
	store  *store.Store
	stats  func() proxy.Stats
	config *config.Config // redacted
	apiKey string
	static fs.FS
	index  []byte
}

// New returns the handler for everything under /ui/.
func New(o Options) http.Handler {
	static, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed directive guarantees dist exists
	}
	index, err := fs.ReadFile(static, "index.html")
	if err != nil {
		panic(fmt.Errorf("embedded UI has no index.html: %w", err))
	}
	s := &server{
		store:  o.Store,
		stats:  o.Stats,
		config: o.Config.Redacted(),
		apiKey: o.APIKey,
		static: static,
		index:  index,
	}

	api := http.NewServeMux()
	api.HandleFunc("GET /ui/api/requests", s.requests)
	api.HandleFunc("GET /ui/api/requests/{id}", s.request)
	api.HandleFunc("GET /ui/api/usage", s.usage)
	api.HandleFunc("GET /ui/api/providers", s.providers)
	api.HandleFunc("GET /ui/api/config", s.configView)
	api.HandleFunc("/ui/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})

	mux := http.NewServeMux()
	mux.Handle("/ui/api/", s.authorize(api))
	mux.HandleFunc("/ui/", s.serveStatic)
	return mux
}

// authorize requires server.api_key as a bearer token when one is set. The
// static assets stay public: they hold no data, and the page needs to load
// to ask for the key.
func (s *server) authorize(next http.Handler) http.Handler {
	if s.apiKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveStatic serves an embedded file, or index.html for any other path so
// that client-side routes load the app.
func (s *server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	// http.ServeFileFS redirects ".../index.html" to its directory, so the
	// index is always served from memory below.
	if name != "" && fs.ValidPath(name) && path.Base(name) != "index.html" {
		if fi, err := fs.Stat(s.static, name); err == nil && !fi.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				// Vite content-hashes everything under assets/.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeFileFS(w, r, s.static, name)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(s.index))
}

// requestsPage is the response of /ui/api/requests. NextBefore is the
// before= cursor for the next (older) page, nil when there is none.
type requestsPage struct {
	Rows       []store.Request `json:"rows"`
	NextBefore *int64          `json:"next_before"`
}

func (s *server) requests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var f store.RequestFilter
	var err error
	if f.Since, err = parseTime(q.Get("since")); err != nil {
		writeError(w, http.StatusBadRequest, "since: "+err.Error())
		return
	}
	if f.Until, err = parseTime(q.Get("until")); err != nil {
		writeError(w, http.StatusBadRequest, "until: "+err.Error())
		return
	}
	f.Protocol = q.Get("protocol")
	f.Provider = q.Get("provider")
	f.Route = q.Get("route")
	f.Outcome = q.Get("outcome")
	f.ModelContains = q.Get("model")
	if v := q.Get("status"); v != "" {
		if f.StatusClass, err = parseStatusClass(v); err != nil {
			writeError(w, http.StatusBadRequest, "status: "+err.Error())
			return
		}
	}
	if v := q.Get("errors"); v != "" {
		if f.ErrorsOnly, err = strconv.ParseBool(v); err != nil {
			writeError(w, http.StatusBadRequest, "errors: want 1 or 0")
			return
		}
	}
	for _, p := range []struct {
		name string
		dst  *int64
	}{{"before", &f.BeforeID}, {"after", &f.AfterID}} {
		if v := q.Get(p.name); v != "" {
			if *p.dst, err = strconv.ParseInt(v, 10, 64); err != nil || *p.dst < 0 {
				writeError(w, http.StatusBadRequest, p.name+": want a row id")
				return
			}
		}
	}
	if v := q.Get("limit"); v != "" {
		if f.Limit, err = strconv.Atoi(v); err != nil || f.Limit < 1 {
			writeError(w, http.StatusBadRequest, "limit: want a positive integer")
			return
		}
	}

	rows, err := s.store.Requests(f)
	if err != nil {
		s.internalError(w, err)
		return
	}
	page := requestsPage{Rows: rows}
	if page.Rows == nil {
		page.Rows = []store.Request{}
	}
	// A full page may have older rows behind it. A live tail (after=) reads
	// forward and has no older page.
	if f.AfterID == 0 && len(rows) > 0 && len(rows) == effectiveLimit(f.Limit) {
		page.NextBefore = &rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, page)
}

// effectiveLimit mirrors store.Requests' default and cap, so a full page can
// be recognized.
func effectiveLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	return min(limit, 1000)
}

func (s *server) request(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	row, err := s.store.Request(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// usageView is the response of /ui/api/usage. Daily is always the by-day
// series over the same filter, for the chart.
type usageView struct {
	Rows           []store.Aggregate  `json:"rows"`
	Total          store.Aggregate    `json:"total"`
	UnpricedModels []store.ModelCount `json:"unpriced_models"`
	Daily          []store.Aggregate  `json:"daily"`
}

func (s *server) usage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var f store.UsageFilter
	var err error
	if f.Since, err = parseTime(q.Get("since")); err != nil {
		writeError(w, http.StatusBadRequest, "since: "+err.Error())
		return
	}
	if f.Until, err = parseTime(q.Get("until")); err != nil {
		writeError(w, http.StatusBadRequest, "until: "+err.Error())
		return
	}
	f.Protocol = q.Get("protocol")
	f.Provider = q.Get("provider")
	f.Route = q.Get("route")
	f.Model = q.Get("model")
	by := q.Get("by")
	if by == "" {
		by = "model"
	}
	if !slices.Contains([]string{"model", "provider", "route", "day"}, by) {
		writeError(w, http.StatusBadRequest, "by: want model, provider, route, or day")
		return
	}

	var v usageView
	if v.Rows, v.Total, err = s.store.Usage(f, by); err != nil {
		s.internalError(w, err)
		return
	}
	if by == "day" {
		v.Daily = v.Rows
	} else if v.Daily, _, err = s.store.Usage(f, "day"); err != nil {
		s.internalError(w, err)
		return
	}
	if v.UnpricedModels, err = s.store.UnpricedModels(f); err != nil {
		s.internalError(w, err)
		return
	}
	v.Rows = nonNil(v.Rows)
	v.Daily = nonNil(v.Daily)
	v.UnpricedModels = nonNil(v.UnpricedModels)
	writeJSON(w, http.StatusOK, v)
}

// ProvidersView is the response of /ui/api/providers, also read by
// `verbatim status`: one entry per provider endpoint, sorted by provider and
// protocol. Counters cover the last hour.
type ProvidersView struct {
	Endpoints  []EndpointView `json:"endpoints"`
	LogDropped int64          `json:"log_dropped"`
	PinsTotal  int            `json:"pins_total"`
}

// EndpointView is one provider endpoint's live state.
type EndpointView struct {
	Provider    string      `json:"provider"`
	Protocol    string      `json:"protocol"`
	BaseURL     string      `json:"base_url"` // redacted
	Breaker     BreakerView `json:"breaker"`
	Pins        int         `json:"pins"`
	ModelsAgeS  *int64      `json:"models_age_s"` // nil: the model list was never fetched
	Committed   int64       `json:"committed"`
	Failover    int64       `json:"failover"`
	ErrorEvents int64       `json:"error_events"`
}

// BreakerView is an endpoint's circuit breaker state.
type BreakerView struct {
	State     string     `json:"state"` // closed | open | half_open
	Failures  int        `json:"failures"`
	OpenUntil *time.Time `json:"open_until"`
}

func (s *server) providers(w http.ResponseWriter, r *http.Request) {
	st := s.stats()
	counts, err := s.store.EndpointStats(time.Now().Add(-time.Hour))
	if err != nil {
		s.internalError(w, err)
		return
	}
	v := ProvidersView{Endpoints: []EndpointView{}, LogDropped: st.LogDropped, PinsTotal: st.PinsTotal}
	for _, name := range slices.Sorted(maps.Keys(s.config.Providers)) {
		p := s.config.Providers[name]
		for _, proto := range slices.Sorted(maps.Keys(p.Endpoints)) {
			ep := config.EndpointName(name, proto)
			c := counts[store.Endpoint{Provider: name, Protocol: proto}]
			ev := EndpointView{
				Provider:    name,
				Protocol:    proto,
				BaseURL:     p.Endpoints[proto].BaseURL,
				Breaker:     BreakerView{State: "closed"}, // an endpoint the breakers have not seen yet
				Pins:        st.Pins[ep],
				Committed:   c.Committed,
				Failover:    c.Failover,
				ErrorEvents: c.ErrorEvents,
			}
			if b, ok := st.Breakers[ep]; ok {
				ev.Breaker = BreakerView{State: b.State, Failures: b.Failures}
				if !b.OpenUntil.IsZero() {
					ev.Breaker.OpenUntil = &b.OpenUntil
				}
			}
			if age, ok := st.ModelsAge[ep]; ok {
				ev.ModelsAgeS = new(int64(age / time.Second))
			}
			v.Endpoints = append(v.Endpoints, ev)
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// configView returns the redacted configuration. The config structs carry
// only yaml tags, so it goes through YAML to keep the file's own key names
// and duration syntax.
func (s *server) configView(w http.ResponseWriter, r *http.Request) {
	b, err := yaml.Marshal(s.config)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var v map[string]any
	if err := yaml.Unmarshal(b, &v); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// parseTime accepts unix milliseconds (all digits) or RFC 3339; empty is the
// zero time, meaning no bound.
func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(ms), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("want unix milliseconds or RFC 3339, got %q", v)
	}
	return t, nil
}

// parseStatusClass parses "2xx" through "5xx" into the class digit.
func parseStatusClass(v string) (int, error) {
	if len(v) == 3 && v[0] >= '1' && v[0] <= '5' && strings.EqualFold(v[1:], "xx") {
		return int(v[0] - '0'), nil
	}
	return 0, fmt.Errorf("want 1xx, 2xx, 3xx, 4xx, or 5xx, got %q", v)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (s *server) internalError(w http.ResponseWriter, err error) {
	slog.Warn("ui api", "err", err)
	writeError(w, http.StatusInternalServerError, err.Error())
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Warn("ui api: encode response", "err", err)
		http.Error(w, `{"error":"encode response"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b)
}
