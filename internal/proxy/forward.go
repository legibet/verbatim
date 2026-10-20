package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/sjson"

	"verbatim/internal/affinity"
	"verbatim/internal/config"
	"verbatim/internal/models"
	"verbatim/internal/pricing"
	"verbatim/internal/protocol"
	"verbatim/internal/route"
	"verbatim/internal/store"
)

const (
	// maxErrorBody bounds the body kept from a failed attempt (design 8.1).
	maxErrorBody = 1 << 20
	// statusClientClosed is logged when the client went away before any
	// response was written to it (the nginx convention); nothing is sent.
	statusClientClosed = 499
)

// Log row outcomes (design 11.2).
const (
	outcomeCompleted       = "completed"
	outcomeErrorEvent      = "upstream_error_event"
	outcomeTruncated       = "truncated"
	outcomeClientCancelled = "client_cancelled"
	outcomeGatewayError    = "gateway_error"
)

// Pin states recorded in routing_json.
const (
	pinNone     = "none"
	pinHit      = "hit"
	pinReserved = "reserved"
	pinMoved    = "moved"
)

// exchange is one forwarded request: the attempt loop's state and the log
// row it fills in.
type exchange struct {
	h     *Handler
	rt    *runtime
	w     http.ResponseWriter
	r     *http.Request
	proto protocol.Protocol
	op    protocol.Operation
	start time.Time

	pin       pinRef
	committed bool // status and headers have been written to the client
	forwarded bool // the last attempt's response is what the client got
	abort     bool // abort the client connection once the row is recorded

	row      store.Request
	routing  routingJSON
	attempts []attemptJSON
}

// pinRef is the pin this request looked up or reserved.
type pinRef struct {
	key      affinity.PinKey
	target   route.TargetID // the pinned target at selection time
	has      bool
	reserved bool // this request created the pin
}

type targetJSON struct {
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
}

type candidateJSON struct {
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	Priority      int    `json:"priority"`
	Weight        int    `json:"weight"`
}

type ownerJSON struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
}

type pinJSON struct {
	State  string      `json:"state"`
	Source string      `json:"source,omitempty"` // what produced the affinity key
	Target *targetJSON `json:"target"`           // the pinned target at selection time
}

// routingJSON is the log's explanation of how the request was routed.
type routingJSON struct {
	Route       string          `json:"route"`
	Candidates  []candidateJSON `json:"candidates"`
	Order       []targetJSON    `json:"order"`
	Owner       *ownerJSON      `json:"owner"`
	Pin         pinJSON         `json:"pin"`
	SkippedOpen []string        `json:"skipped_open"`
}

// attemptJSON is one entry of attempt_log.
type attemptJSON struct {
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	Status        int    `json:"status,omitempty"`
	Error         string `json:"error,omitempty"`
	MS            int64  `json:"ms"`
	Excerpt       string `json:"excerpt,omitempty"`
}

func target(t route.TargetID) *targetJSON {
	return &targetJSON{Provider: t.Provider, UpstreamModel: t.Model}
}

func newRequestID() string { return strings.ToLower(rand.Text()) }

// forward handles one protocol request end to end and records its log row.
// Requests that fail client authentication are answered but not logged, so
// that unauthenticated clients cannot write to the database.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, id string, p protocol.Protocol, op protocol.Operation) {
	rt := h.rt.Load()
	if !rt.authorized(r) {
		writeError(w, p, http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
		return
	}
	h.exchanges.Add(1)
	defer h.exchanges.Done()
	start := time.Now()
	x := &exchange{
		h: h, rt: rt, w: w, r: r, proto: p, op: op, start: start,
		row: store.Request{TS: start, RequestID: id, Protocol: string(p.ID()), Operation: string(op)},
		routing: routingJSON{
			Candidates:  []candidateJSON{},
			Order:       []targetJSON{},
			Pin:         pinJSON{State: pinNone},
			SkippedOpen: []string{},
		},
		attempts: []attemptJSON{},
	}
	x.run()
	x.record()
	if x.abort {
		// Abort rather than close cleanly, so the client's SDK sees an error
		// instead of a truncated success (design 4.3).
		panic(http.ErrAbortHandler)
	}
}

func (x *exchange) run() {
	rt := x.rt
	defer x.h.inflight.Add(-1)
	if limit := rt.cfg.Server.MaxInflight; x.h.inflight.Add(1) > int64(limit) {
		x.fail(http.StatusServiceUnavailable, "too_many_inflight", fmt.Sprintf("more than %d requests in flight", limit))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(x.w, x.r.Body, rt.cfg.Server.MaxBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			x.fail(http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("request body exceeds %d bytes", rt.cfg.Server.MaxBodyBytes))
			return
		}
		x.cancelled(fmt.Sprintf("read request body: %v", err))
		return
	}
	contentType := x.r.Header.Get("Content-Type")
	model, err := x.proto.ModelFromBody(body, contentType)
	if err != nil {
		x.fail(http.StatusBadRequest, "no_model", err.Error())
		return
	}
	x.row.ClientModel = model
	x.row.Stream = x.proto.Stream(body, contentType)

	routeName, cands, ok := rt.routes.Match(x.proto.ID(), model)
	if !ok {
		x.fail(http.StatusNotFound, "no_route", fmt.Sprintf("no route serves model %q over the %s protocol", model, x.proto.ID()))
		return
	}
	x.row.Route = &routeName
	x.routing.Route = routeName
	for _, c := range cands {
		x.routing.Candidates = append(x.routing.Candidates, candidateJSON{c.Provider, c.Model, c.Priority, c.Weight})
	}

	order, ok := x.order(body, model, routeName, cands)
	if !ok {
		return
	}
	for _, c := range order {
		x.routing.Order = append(x.routing.Order, *target(c.TargetID))
	}
	x.attemptAll(body, order)
}

// order resolves ownership and affinity into the attempt order (design 6.4,
// 7). It reports false after answering the client itself.
func (x *exchange) order(body []byte, model, routeName string, cands []route.Candidate) ([]route.Candidate, bool) {
	h := x.h
	key, source, hasKey := x.rt.rules.Key(x.proto, model, x.r.Header, body)
	if hasKey {
		x.row.AffinityKey = &key
		x.routing.Pin.Source = source
	}

	// A continuation can only be served by the upstream that holds the
	// referenced state. The owner is used as recorded, even when the route's
	// targets have changed since; only its provider must still serve this
	// protocol.
	if id, ok := x.proto.ContinuationRef(body); ok {
		provider, upstreamModel, found, err := h.store.Owner(id)
		if err != nil {
			x.fail(http.StatusInternalServerError, "internal_error", err.Error())
			return nil, false
		}
		if !found {
			x.fail(http.StatusNotFound, "unknown_response_id",
				fmt.Sprintf("response %q was not produced through this gateway, or its record has expired", id))
			return nil, false
		}
		if _, ok := x.rt.cfg.Providers[provider].Endpoints[string(x.proto.ID())]; !ok {
			x.fail(http.StatusNotFound, "unknown_response_id",
				fmt.Sprintf("response %q belongs to provider %q, which no longer has a %s endpoint", id, provider, x.proto.ID()))
			return nil, false
		}
		owner := route.Candidate{TargetID: route.TargetID{Provider: provider, Model: upstreamModel}, Priority: 1, Weight: 1}
		if i := slices.IndexFunc(cands, func(c route.Candidate) bool { return c.TargetID == owner.TargetID }); i >= 0 {
			owner = cands[i]
		}
		x.routing.Owner = &ownerJSON{ID: id, Provider: provider, UpstreamModel: upstreamModel}
		return []route.Candidate{owner}, true
	}

	order := x.rt.routes.Order(routeName, x.proto.ID(), cands)
	if !hasKey {
		return order, true
	}
	x.pin.key = affinity.PinKey{Protocol: x.proto.ID(), Key: key, Model: model}
	if x.op.Auxiliary() {
		// count_tokens and search follow a pin but never create one (design 7.3).
		x.pin.target, x.pin.has = h.pins.Get(x.pin.key)
	} else {
		// Reserving before the first attempt makes concurrent first requests
		// of a session agree on one target (design 7.2).
		x.pin.target, x.pin.reserved = h.pins.Reserve(x.pin.key, order[0].TargetID)
		x.pin.has = true
	}
	if !x.pin.has {
		return order, true
	}
	x.routing.Pin.Target = target(x.pin.target)
	switch {
	case x.pin.reserved:
		x.routing.Pin.State = pinReserved
	case slices.ContainsFunc(order, func(c route.Candidate) bool { return c.TargetID == x.pin.target }):
		x.routing.Pin.State = pinHit
		order = route.Front(order, x.pin.target)
		if x.rt.cfg.Affinity.Mode == "strict" {
			// strict: the pinned target's failure is the request's result.
			order = order[:1]
		}
	}
	// A pin to a target the route no longer offers stays "none" here and is
	// moved to whichever target gets committed.
	return order, true
}

// failedResponse is the buffered response of a failed attempt, kept in case
// it turns out to be the last one (design 8.4).
type failedResponse struct {
	status   int
	header   http.Header
	trailer  http.Header
	body     []byte
	complete bool // the whole body was read
}

// attemptAll runs the attempt loop (design 5, 6.4, 8): each candidate once,
// the breaker checked at dispatch, failover decided on the status line, and
// the first other response committed.
func (x *exchange) attemptAll(body []byte, order []route.Candidate) {
	h := x.h
	var last *failedResponse // response of the last attempt, nil if it had none
	var failures []string    // "provider: error" for the 502 message
	for _, c := range order {
		if err := x.r.Context().Err(); err != nil {
			x.releasePin()
			x.cancelled("client cancelled during failover")
			return
		}
		req, err := x.build(body, c)
		if err != nil {
			// Deterministic for this target and not the provider's fault.
			msg := "build request: " + err.Error()
			x.attempts = append(x.attempts, attemptJSON{Provider: c.Provider, UpstreamModel: c.Model, Error: msg})
			x.setTarget(c.TargetID)
			failures = append(failures, c.Provider+": "+msg)
			last = nil
			continue
		}
		endpoint := config.EndpointName(c.Provider, string(x.proto.ID()))
		admitted, probe := x.rt.breakers.Admit(endpoint)
		if !admitted {
			x.routing.SkippedOpen = append(x.routing.SkippedOpen, c.Provider)
			failures = append(failures, c.Provider+": breaker open")
			continue
		}
		x.setTarget(c.TargetID)
		sent := time.Now()
		a := attemptJSON{Provider: c.Provider, UpstreamModel: c.Model}
		resp, err := x.rt.transports[endpoint].RoundTrip(req)
		if err != nil {
			a.MS = time.Since(sent).Milliseconds()
			if x.r.Context().Err() != nil {
				// The client left; that says nothing about the provider.
				a.Error = "client cancelled"
				x.attempts = append(x.attempts, a)
				x.verdict(endpoint, probe, false)
				x.releasePin()
				x.cancelled("client cancelled while waiting for " + c.Provider)
				return
			}
			a.Error = err.Error()
			x.attempts = append(x.attempts, a)
			x.verdict(endpoint, probe, true)
			h.log.Warn("upstream request failed", "provider", c.Provider, "url", req.URL.Redacted(),
				"err", err, "request_id", x.row.RequestID)
			failures = append(failures, c.Provider+": "+err.Error())
			last = nil
			continue
		}
		a.Status = resp.StatusCode
		if !failover(resp.StatusCode) {
			a.MS = time.Since(sent).Milliseconds()
			x.attempts = append(x.attempts, a)
			if x.op.Auxiliary() {
				x.verdict(endpoint, probe, false)
			} else {
				x.rt.breakers.Success(endpoint)
			}
			x.commitPin(c.TargetID)
			x.commit(resp, c)
			return
		}

		last = x.drain(resp)
		a.MS = time.Since(sent).Milliseconds()
		a.Excerpt = excerpt(last.body, last.header, x.rt.cfg.Log.ErrorExcerptBytes)
		x.attempts = append(x.attempts, a)
		x.verdict(endpoint, probe, counted(resp.StatusCode))
		failures = append(failures, fmt.Sprintf("%s: HTTP %d", c.Provider, resp.StatusCode))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			h.log.Warn("upstream refused the request; check the provider's key, base_url and model name",
				"provider", c.Provider, "url", req.URL.Redacted(), "upstream_model", c.Model, "status", resp.StatusCode,
				"request_id", x.row.RequestID)
		}
		if x.r.Context().Err() != nil {
			// The client left while the failed body was read: nothing more is
			// attempted, and nothing is returned to it.
			x.releasePin()
			x.cancelled("client cancelled during failover")
			return
		}
	}

	// Exhaustion (design 8.4).
	x.releasePin()
	x.w.Header().Set("X-Verbatim-Attempts", strconv.Itoa(len(x.attempts)))
	switch {
	case last != nil:
		x.returnLast(last)
	case len(x.attempts) == 0:
		x.fail(http.StatusBadGateway, "all_upstreams_open", "every candidate's circuit breaker is open: "+strings.Join(failures, "; "))
	default:
		x.fail(http.StatusBadGateway, "all_upstreams_failed", "no upstream returned a response: "+strings.Join(failures, "; "))
	}
}

// failover reports whether a status ends the attempt and moves on (design 8.1).
func failover(status int) bool {
	switch status {
	case 401, 403, 404, 408, 409, 425, 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

// counted reports whether a failover status says the provider as a whole is
// unhealthy and so feeds the breaker (design 8.3).
func counted(status int) bool {
	return status == 429 || status == 529 || status >= 500
}

// verdict reports a failed or verdict-less attempt to the breaker.
// Auxiliary operations never feed it: a healthy provider may well lack
// count_tokens or search. A probe that ends without a counted failure is
// abandoned so the next request can probe.
func (x *exchange) verdict(endpoint string, probe, failure bool) {
	if failure && !x.op.Auxiliary() {
		x.rt.breakers.Failure(endpoint, probe)
	} else if probe {
		x.rt.breakers.Abandon(endpoint)
	}
}

// setTarget records the target of the latest attempt on the row.
func (x *exchange) setTarget(t route.TargetID) {
	x.row.Provider, x.row.UpstreamModel = &t.Provider, &t.Model
}

// commitPin records the committed target on the session's pin, moving the
// pin there if it pointed elsewhere; a pin that has served a committed
// response is never released (design 7.2, 7.3).
func (x *exchange) commitPin(t route.TargetID) {
	if x.op.Auxiliary() || !x.pin.has {
		return
	}
	x.h.pins.Commit(x.pin.key, t)
	if x.pin.target != t {
		x.routing.Pin.State = pinMoved
	}
}

// releasePin drops this request's own reservation when nothing was committed.
func (x *exchange) releasePin() {
	if x.pin.reserved {
		x.h.pins.Release(x.pin.key, x.pin.target)
	}
}

// build makes the attempt's upstream request from the original client
// request (design 4.2): model name, overrides, URL, and headers.
func (x *exchange) build(body []byte, c route.Candidate) (*http.Request, error) {
	p := x.rt.cfg.Providers[c.Provider]
	e := p.Endpoints[string(x.proto.ID())]
	var err error
	if c.Model != x.row.ClientModel {
		if body, err = x.proto.SetModel(body, x.r.Header.Get("Content-Type"), c.Model); err != nil {
			return nil, err
		}
	}
	if body, err = applyOverrides(body, p.Overrides); err != nil {
		return nil, err
	}
	u := e.BaseURL + x.proto.Suffix(x.op)
	if q := x.r.URL.RawQuery; q != "" {
		u += "?" + q
	}
	req, err := http.NewRequestWithContext(x.r.Context(), x.r.Method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyEndToEnd(req.Header, x.r.Header, "Host", "Content-Length", "Authorization", "X-Api-Key")
	x.proto.DefaultHeaders(req.Header)
	models.Authorize(req.Header, p, e)
	if len(req.Header.Values("User-Agent")) == 0 {
		// An empty value stops net/http from adding its own User-Agent.
		req.Header["User-Agent"] = []string{""}
	}
	return req, nil
}

// applyOverrides runs a provider's static edits in order (design 9). sjson
// returns a new slice, so the caller's body is never modified.
func applyOverrides(body []byte, ops []config.Override) ([]byte, error) {
	for i, o := range ops {
		var err error
		if o.Set != nil {
			var v []byte
			if v, err = json.Marshal(o.Set.Value); err == nil {
				body, err = sjson.SetRawBytes(body, o.Set.Path, v)
			}
		} else {
			body, err = sjson.DeleteBytes(body, o.Delete)
		}
		if err != nil {
			return nil, fmt.Errorf("overrides[%d]: %w", i, err)
		}
	}
	return body, nil
}

// hopHeaders are the hop-by-hop headers of RFC 9110 section 7.6.1 plus the
// legacy ones net/http's reverse proxy also drops.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// copyEndToEnd adds src's headers to dst except hop-by-hop headers, those
// named in src's Connection header, and the canonical names in drop.
func copyEndToEnd(dst, src http.Header, drop ...string) {
	skip := map[string]bool{}
	for _, k := range slices.Concat(hopHeaders, drop) {
		skip[k] = true
	}
	for _, v := range src["Connection"] {
		for f := range strings.SplitSeq(v, ",") {
			if f = textproto.TrimString(f); f != "" {
				skip[http.CanonicalHeaderKey(f)] = true
			}
		}
	}
	for k, vv := range src {
		if !skip[k] {
			dst[k] = append(dst[k], vv...)
		}
	}
}

// drain reads a failed attempt's body, bounded in size and by the idle
// timeout, and closes it.
func (x *exchange) drain(resp *http.Response) *failedResponse {
	defer resp.Body.Close()
	b := &idleBody{rc: resp.Body, d: x.rt.cfg.Server.IdleTimeout.Std()}
	defer b.stop()
	body, err := io.ReadAll(io.LimitReader(b, maxErrorBody+1))
	complete := err == nil && len(body) <= maxErrorBody
	body = body[:min(len(body), maxErrorBody)]
	return &failedResponse{status: resp.StatusCode, header: resp.Header, trailer: resp.Trailer, body: body, complete: complete}
}

// writeHead commits status and headers to the client: the point after which
// nothing is retried (design 4.3).
func (x *exchange) writeHead(status int, header, trailer http.Header) {
	h := x.w.Header()
	copyEndToEnd(h, header)
	if len(trailer) > 0 {
		h.Add("Trailer", strings.Join(slices.Sorted(maps.Keys(trailer)), ", "))
	}
	x.w.WriteHeader(status)
	x.committed, x.forwarded = true, true
	x.row.Status = status
	x.row.TTFBMS = new(time.Since(x.start).Milliseconds())
	x.row.UpstreamRequestID = upstreamRequestID(header)
}

// writeTrailers sends the upstream trailers once the body is complete.
func (x *exchange) writeTrailers(trailer http.Header) {
	for k, vv := range trailer {
		for _, v := range vv {
			x.w.Header().Add(http.TrailerPrefix+k, v)
		}
	}
}

func upstreamRequestID(h http.Header) *string {
	for _, k := range []string{"X-Request-Id", "Request-Id"} {
		if v := h.Get(k); v != "" {
			return &v
		}
	}
	return nil
}

// returnLast returns the last failed attempt's response unchanged, apart from
// x-verbatim-attempts (design 8.4).
func (x *exchange) returnLast(last *failedResponse) {
	x.writeHead(last.status, last.header, last.trailer)
	x.row.Error = new(excerpt(last.body, last.header, x.rt.cfg.Log.ErrorExcerptBytes))
	switch _, err := x.w.Write(last.body); {
	case err != nil:
		x.cancelled("client went away: " + err.Error())
	case !last.complete:
		// Only part of the body was read; the client must not take it for whole.
		x.truncated("upstream error body cut off after " + strconv.Itoa(len(last.body)) + " bytes")
	default:
		x.writeTrailers(last.trailer)
		x.row.Outcome = outcomeCompleted
	}
}

// commit forwards the committed response: status and headers, then the body
// with a flush after every write, observed by the usage tee (design 4.3,
// 11.3).
func (x *exchange) commit(resp *http.Response, c route.Candidate) {
	defer resp.Body.Close()
	h := x.h
	x.writeHead(resp.StatusCode, resp.Header, resp.Trailer)
	ok2xx := resp.StatusCode/100 == 2
	tee := newUsageTee(x.proto.NewUsageParser(x.op, eventStream(resp.Header, x.row.Stream)), resp.Header)
	// A Responses client may send its next turn the moment it has read this
	// one, so the owner is recorded before the bytes revealing the id reach it.
	needOwner := ok2xx && x.proto.ID() == protocol.Responses && x.op == protocol.Create
	recordOwner := func() {
		if id := tee.responseID(); id != "" {
			needOwner = false
			if err := h.store.RecordOwner(id, c.Provider, c.Model); err != nil {
				h.log.Warn("response ownership not recorded", "request_id", x.row.RequestID, "err", err)
			}
		}
	}
	// A committed error body is kept for the row's error column. A compressed
	// body is kept in larger measure so that its excerpt still decompresses
	// to the configured size.
	var errBody []byte
	errMax := 0
	if resp.StatusCode >= 400 {
		errMax = x.rt.cfg.Log.ErrorExcerptBytes
		if resp.Header.Get("Content-Encoding") != "" {
			errMax = min(maxErrorBody, 16*errMax)
		}
	}
	rc := http.NewResponseController(x.w)
	body := &idleBody{rc: resp.Body, d: x.rt.cfg.Server.IdleTimeout.Std()}
	defer body.stop()
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			tee.write(chunk)
			if needOwner {
				recordOwner()
			}
			if len(errBody) < errMax {
				errBody = append(errBody, chunk[:min(n, errMax-len(errBody))]...)
			}
			if _, werr := x.w.Write(chunk); werr != nil {
				x.cancelled("client went away: " + werr.Error())
				break
			}
			if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				x.cancelled("client went away: " + ferr.Error())
				break
			}
		}
		if err == io.EOF {
			x.writeTrailers(resp.Trailer)
			x.row.Outcome = outcomeCompleted
			break
		}
		if err != nil {
			switch {
			case x.r.Context().Err() != nil:
				x.cancelled("client went away")
			case body.expired.Load():
				x.truncated(fmt.Sprintf("upstream body idle for %s", x.rt.cfg.Server.IdleTimeout))
			default:
				x.truncated("read upstream body: " + err.Error())
			}
			break
		}
	}
	tee.close()
	if needOwner {
		// A decoder may hold the end of a body, and with it the id of a
		// non-streamed response, until its input has ended.
		recordOwner()
	}
	if x.row.Outcome == outcomeCompleted && ok2xx && tee.errorEvent() {
		x.row.Outcome = outcomeErrorEvent
	}
	if len(errBody) > 0 {
		x.row.Error = new(excerpt(errBody, resp.Header, x.rt.cfg.Log.ErrorExcerptBytes))
	}
	x.setUsage(tee, c)
}

// setUsage fills the token columns and, unless the operation is auxiliary,
// the cost (design 11.3, 12).
func (x *exchange) setUsage(tee *usageTee, c route.Candidate) {
	u, ok := tee.usage()
	if !ok {
		return
	}
	x.row.InputTokens, x.row.CacheReadTokens, x.row.CacheWriteTokens = u.Input, u.CacheRead, u.CacheWrite
	x.row.CacheWrite1hTokens, x.row.OutputTokens, x.row.ReasoningTokens = u.CacheWrite1h, u.Output, u.Reasoning
	x.row.InputImageTokens, x.row.OutputImageTokens = u.InputImage, u.OutputImage
	if u.Raw != nil {
		x.row.UsageJSON = new(string(u.Raw))
	}
	t := x.h.pricing.Load()
	if x.op.Auxiliary() || t == nil {
		return
	}
	key, price, ok := t.Lookup(x.row.ClientModel, c.Model)
	if !ok {
		return
	}
	x.row.PriceKey = &key
	if x.proto.ID() == protocol.Images && !price.HasImagePrice() {
		return
	}
	if usd, priceJSON, ok := pricing.Cost(u, price); ok {
		x.row.CostUSD, x.row.PriceJSON = &usd, new(string(priceJSON))
	}
}

// fail answers with a verbatim-generated error (design 10).
func (x *exchange) fail(status int, code, msg string) {
	writeError(x.w, x.proto, status, code, msg)
	x.committed = true
	x.row.Status, x.row.Outcome = status, outcomeGatewayError
	x.row.Error = new(code + ": " + msg)
}

// cancelled records that the client went away. If nothing was written yet,
// the connection is aborted so that net/http does not send an implicit 200.
func (x *exchange) cancelled(msg string) {
	x.row.Outcome, x.row.Error = outcomeClientCancelled, &msg
	if !x.committed {
		x.row.Status, x.abort = statusClientClosed, true
	}
}

// truncated records a response cut after commit; the client connection is
// aborted.
func (x *exchange) truncated(msg string) {
	x.row.Outcome, x.row.Error, x.abort = outcomeTruncated, &msg, true
}

// record completes the log row and hands it to the store's writer.
func (x *exchange) record() {
	x.row.TotalMS = time.Since(x.start).Milliseconds()
	x.row.Attempts = len(x.attempts)
	for i, a := range x.attempts {
		if !(x.forwarded && i == len(x.attempts)-1) {
			x.row.AttemptsMS += a.MS
		}
	}
	routing, _ := json.Marshal(x.routing)
	attempts, _ := json.Marshal(x.attempts)
	x.row.RoutingJSON, x.row.AttemptLog = string(routing), string(attempts)
	x.h.store.Record(&x.row)
	x.h.log.Debug("request", "request_id", x.row.RequestID, "protocol", x.row.Protocol, "model", x.row.ClientModel,
		"status", x.row.Status, "outcome", x.row.Outcome, "attempts", x.row.Attempts, "total_ms", x.row.TotalMS)
}

// errIdle is returned by idleBody once the idle timeout has fired.
var errIdle = errors.New("upstream body idle timeout")

// idleBody closes the upstream body when one Read waits longer than d
// (server.idle_timeout). The timer runs only while a Read is in progress,
// so a slow client is not mistaken for a stalled upstream.
type idleBody struct {
	rc      io.ReadCloser
	d       time.Duration
	timer   *time.Timer
	expired atomic.Bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.timer == nil {
		b.timer = time.AfterFunc(b.d, func() {
			b.expired.Store(true)
			b.rc.Close()
		})
	} else {
		b.timer.Reset(b.d)
	}
	n, err := b.rc.Read(p)
	b.timer.Stop()
	if b.expired.Load() {
		return n, errIdle
	}
	return n, err
}

func (b *idleBody) stop() {
	if b.timer != nil {
		b.timer.Stop()
	}
}

// excerpt returns the first max bytes of an error body for the log. A
// compressed body is decompressed first, so the excerpt is readable; a
// truncated or corrupt one yields what could be decoded, and one in an
// encoding verbatim cannot read yields nothing.
func excerpt(body []byte, header http.Header, max int) string {
	var r io.Reader = bytes.NewReader(body)
	open, ok := decoder(header)
	if !ok {
		return ""
	}
	if open != nil {
		zr, err := open(r)
		if err != nil {
			return ""
		}
		defer zr.Close()
		r = zr
	}
	out, _ := io.ReadAll(io.LimitReader(r, int64(max)))
	return string(out)
}
