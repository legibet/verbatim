package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"verbatim/internal/config"
)

func TestProbe(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method != http.MethodPost || string(body) != "{}":
			w.WriteHeader(http.StatusTeapot)
		case r.URL.Path == "/ok/v1/messages" && r.Header.Get("X-Api-Key") == "k" && r.Header.Get("Anthropic-Version") != "",
			r.URL.Path == "/ok/v1/images/generations" && r.Header.Get("Authorization") == "Bearer k":
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"model is required"}}`)
		case strings.HasPrefix(r.URL.Path, "/denied/"):
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"invalid key"}}`)
		case strings.HasPrefix(r.URL.Path, "/moved/"):
			http.Redirect(w, r, "https://elsewhere/v1/chat/completions", http.StatusMovedPermanently)
		case strings.HasPrefix(r.URL.Path, "/page/"):
			io.WriteString(w, "<!doctype html><html></html>")
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	cfg, _, err := config.Parse(fmt.Appendf(nil, `
providers:
  anth: {key: k, endpoints: {anthropic: {base_url: %[1]s/ok}, images: {base_url: %[1]s/ok/v1}}}
  denied: {key: k, endpoints: {chat: {base_url: %[1]s/denied/v1}}}
  down: {key: k, endpoints: {chat: {base_url: %[2]s/v1}}}
  moved: {key: k, endpoints: {chat: {base_url: %[1]s/moved/v1}}}
  page: {key: k, endpoints: {chat: {base_url: %[1]s/page/v1}}}
  wrong: {key: k, endpoints: {responses: {base_url: %[1]s/wrong}}}
routes:
  - model: m
    targets: [{provider: anth}]
`, up.URL, down.URL), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"anth.anthropic":  "",
		"anth.images":     "",
		"denied.chat":     "key rejected: POST " + up.URL + "/denied/v1/chat/completions returned 401",
		"down.chat":       "cannot connect: POST " + down.URL + "/v1/chat/completions: ",
		"moved.chat":      "wrong base_url: POST " + up.URL + "/moved/v1/chat/completions redirects to https://elsewhere/v1/chat/completions",
		"page.chat":       "wrong base_url: POST " + up.URL + "/page/v1/chat/completions returned 200 with a body that is not JSON",
		"wrong.responses": "wrong base_url: POST " + up.URL + "/wrong/responses returned 404",
	}
	results := probe(cfg)
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for i, r := range results {
		if i > 0 && results[i-1].endpoint >= r.endpoint {
			t.Errorf("results out of order at %s", r.endpoint)
		}
		// A connection error's text comes from the operating system.
		if w, ok := want[r.endpoint]; !ok || r.problem != w && !(strings.HasSuffix(w, ": ") && strings.HasPrefix(r.problem, w)) {
			t.Errorf("%s: %q\nwant %q", r.endpoint, r.problem, w)
		}
	}
}
