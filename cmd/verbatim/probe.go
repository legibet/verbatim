package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"verbatim/internal/config"
	"verbatim/internal/models"
	"verbatim/internal/protocol"
)

// probeTimeout bounds the probe of one endpoint.
const probeTimeout = 15 * time.Second

// probeResult is the outcome of probing one endpoint; problem is empty when
// nothing looks wrong.
type probeResult struct{ endpoint, problem string }

// probe sends an empty JSON request to every endpoint's request path, as the
// gateway would forward one, and checks the base_url and the key from the
// reply. An empty request generates nothing. Results are in endpoint order.
func probe(cfg *config.Config) []probeResult {
	var results []probeResult
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		for _, proto := range slices.Sorted(maps.Keys(cfg.Providers[name].Endpoints)) {
			results = append(results, probeResult{endpoint: config.EndpointName(name, proto)})
		}
	}
	// Like the gateway, the probe calls the transport directly and so never
	// follows a redirect.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	var wg sync.WaitGroup
	for i := range results {
		name, proto, _ := strings.Cut(results[i].endpoint, ".")
		wg.Go(func() {
			results[i].problem = probeEndpoint(transport, cfg.Providers[name], proto)
		})
	}
	wg.Wait()
	return results
}

// probeEndpoint reports what is wrong with one endpoint, or "":
//   - no reply: the host, port, or scheme of base_url is wrong;
//   - 401 or 403: the key is rejected;
//   - a redirect, 404, or a reply that is not JSON: base_url does not lead
//     to the API.
//
// Any other reply, typically 400 for the missing fields, means the API
// answered at that path.
func probeEndpoint(transport http.RoundTripper, p config.Provider, proto string) string {
	e := p.Endpoints[proto]
	pr := protocol.Lookup(protocol.ID(proto))
	op := protocol.Create
	if pr.ID() == protocol.Images {
		op = protocol.Generate
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+pr.Suffix(op), strings.NewReader("{}"))
	if err != nil {
		return "wrong base_url: " + err.Error()
	}
	where := "POST " + req.URL.Redacted()
	req.Header.Set("Content-Type", "application/json")
	pr.DefaultHeaders(req.Header)
	models.Authorize(req.Header, p, e)
	// Without Accept-Encoding the transport negotiates gzip and decodes it;
	// a configured one would leave the reply encoded, and an encoded JSON
	// body would not look like JSON.
	req.Header.Del("Accept-Encoding")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return fmt.Sprintf("cannot connect: %s: %v", where, err)
	}
	defer resp.Body.Close()
	switch code := resp.StatusCode; {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Sprintf("key rejected: %s returned %d", where, code)
	case code/100 == 3:
		return fmt.Sprintf("wrong base_url: %s redirects to %s", where, resp.Header.Get("Location"))
	case code == http.StatusNotFound:
		return fmt.Sprintf("wrong base_url: %s returned 404", where)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Sprintf("cannot connect: %s: %v", where, err)
	}
	if !json.Valid(body) {
		return fmt.Sprintf("wrong base_url: %s returned %d with a body that is not JSON", where, resp.StatusCode)
	}
	return ""
}
