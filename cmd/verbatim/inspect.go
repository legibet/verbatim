package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"verbatim/internal/config"
	"verbatim/internal/pricing"
	"verbatim/internal/protocol"
	"verbatim/internal/store"
	"verbatim/internal/ui"
)

func usage(args []string) int {
	fs, cfgPath := newFlags("usage", "usage [--since 7d] [--until t] [--by model|provider|route|day] [--protocol p] [--provider p] [--json]")
	since := fs.String("since", "7d", "start of the range: a duration back from now (90m, 7d), a date, or an RFC 3339 time")
	until := fs.String("until", "", "end of the range, same syntax (default now)")
	by := fs.String("by", "model", "group by model, provider, route, or day (UTC)")
	proto := fs.String("protocol", "", "only this protocol")
	provider := fs.String("provider", "", "only this provider")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		fs.Usage()
		return 2
	}

	f := store.UsageFilter{Protocol: *proto, Provider: *provider}
	now := time.Now()
	var err error
	if f.Since, err = parseWhen(*since, now); err != nil {
		fmt.Fprintln(os.Stderr, "--since:", err)
		return 2
	}
	if f.Until, err = parseWhen(*until, now); err != nil {
		fmt.Fprintln(os.Stderr, "--until:", err)
		return 2
	}

	st, cfg, err := openStore(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	rows, total, err := st.Usage(f, *by)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	p := cfg.Pricing
	prices, _, err := pricing.Load(p.Cache, p.Sources, p.Overrides, p.Aliases)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pricing:", err)
		return 1
	}
	unpriced, err := st.UnpricedModels(f, func(protocolID, client, upstream string) bool {
		return prices.Prices(protocol.ID(protocolID), client, upstream)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(struct {
			Rows           []store.Aggregate  `json:"rows"`
			Total          store.Aggregate    `json:"total"`
			UnpricedModels []store.ModelCount `json:"unpriced_models"`
		}{rows, total, unpriced})
		return 0
	}

	end := "now"
	if !f.Until.IsZero() {
		end = f.Until.Format(time.DateTime)
	}
	start := "the beginning"
	if !f.Since.IsZero() {
		start = f.Since.Format(time.DateTime)
	}
	fmt.Printf("usage from %s to %s, by %s\n\n", start, end, *by)

	t := newTable(column{name: strings.ToUpper(*by)},
		column{"REQUESTS", true}, column{"ERRORS", true},
		column{"INPUT", true}, column{"CACHE READ", true}, column{"CACHE WRITE", true},
		column{"OUTPUT", true}, column{"REASONING", true}, column{"COST USD", true},
		column{"UNPRICED", true}, column{"NO USAGE", true})
	line := func(key string, a store.Aggregate) []string {
		return []string{key, count(a.Requests), count(a.Errors),
			count(a.Input), count(a.CacheRead), count(a.CacheWrite),
			count(a.Output), count(a.Reasoning), usd(a.CostUSD),
			count(a.Unpriced), count(a.NoUsage)}
	}
	var lines [][]string
	for _, a := range rows {
		key := a.Key
		if key == "" {
			key = "-"
		}
		lines = append(lines, line(key, a))
	}
	lines = append(lines, line("total", total))
	t.print(os.Stdout, lines, true)

	fmt.Println("\nCost is at configured list prices. UNPRICED: requests with usage but no price; NO USAGE: requests without usage.")
	if len(unpriced) > 0 {
		var names []string
		for _, m := range unpriced {
			names = append(names, fmt.Sprintf("%s (%d)", m.Model, m.Count))
		}
		fmt.Printf("Models with no price in the price table: %s\n", strings.Join(names, ", "))
	}
	return 0
}

func logCmd(args []string) int {
	fs, cfgPath := newFlags("log", "log [--tail] [--since 1h] [--status 2xx|4xx|5xx] [--provider p] [--protocol p] [--errors] [--limit n] [--json]\n       verbatim log <request_id> [--json]")
	tail := fs.Bool("tail", false, "keep printing new requests as they are logged")
	since := fs.String("since", "", "only requests after this: a duration back from now (1h, 7d), a date, or an RFC 3339 time")
	statusClass := fs.String("status", "", "only this status class: 2xx, 3xx, 4xx, or 5xx")
	provider := fs.String("provider", "", "only this provider")
	proto := fs.String("protocol", "", "only this protocol")
	errorsOnly := fs.Bool("errors", false, "only requests with an error status or an outcome other than completed")
	limit := fs.Int("limit", 50, "number of most recent requests to print (at most 1000)")
	asJSON := fs.Bool("json", false, "print one JSON object per request")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 1 {
		fs.Usage()
		return 2
	}

	st, _, err := openStore(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	if len(pos) == 1 {
		return showRequest(st, pos[0], *asJSON)
	}

	f := store.RequestFilter{Provider: *provider, Protocol: *proto, ErrorsOnly: *errorsOnly, Limit: *limit}
	if f.Since, err = parseWhen(*since, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "--since:", err)
		return 2
	}
	if *statusClass != "" {
		if f.StatusClass, err = parseStatusClass(*statusClass); err != nil {
			fmt.Fprintln(os.Stderr, "--status:", err)
			return 2
		}
	}

	t := newTable(column{name: "TIME"}, column{name: "PROTOCOL"}, column{name: "CLIENT MODEL"},
		column{name: "PROVIDER"}, column{name: "UPSTREAM MODEL"}, column{"STATUS", true}, column{name: "OUTCOME"},
		column{name: "STREAM"}, column{"ATT", true}, column{"TTFB", true}, column{"TOTAL", true},
		column{"INPUT", true}, column{"CACHE R", true}, column{"CACHE W", true}, column{"OUTPUT", true},
		column{"COST", true}, column{name: "REQUEST ID"})
	enc := json.NewEncoder(os.Stdout)
	header := true
	// emit prints rows oldest first, so the newest request is at the bottom.
	emit := func(rows []store.Request) {
		if *asJSON {
			for _, r := range rows {
				enc.Encode(r)
			}
			return
		}
		lines := make([][]string, 0, len(rows))
		for _, r := range rows {
			lines = append(lines, requestLine(r))
		}
		if len(lines) > 0 || header {
			t.print(os.Stdout, lines, header)
			header = false
		}
	}

	rows, err := st.Requests(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	slices.Reverse(rows)
	emit(rows)
	if !*tail {
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	if len(rows) > 0 {
		f.AfterID = rows[len(rows)-1].ID
	}
	f.Limit = 1000
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-tick.C:
		}
		rows, err := st.Requests(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if len(rows) == 0 {
			continue
		}
		// Without an AfterID (nothing matched yet) the store returns newest
		// first; with one, oldest first.
		if f.AfterID == 0 {
			slices.Reverse(rows)
		}
		emit(rows)
		f.AfterID = rows[len(rows)-1].ID
	}
}

func requestLine(r store.Request) []string {
	stream := ""
	if r.Stream {
		stream = "yes"
	}
	ttfb := "-"
	if r.TTFBMS != nil {
		ttfb = millis(*r.TTFBMS)
	}
	return []string{
		r.TS.Local().Format(time.DateTime), r.Protocol + "/" + r.Operation, r.ClientModel,
		optString(r.Provider), optString(r.UpstreamModel), strconv.Itoa(r.Status), r.Outcome,
		stream, strconv.Itoa(r.Attempts), ttfb, millis(r.TotalMS),
		optCount(r.InputTokens), optCount(r.CacheReadTokens), optCount(r.CacheWriteTokens), optCount(r.OutputTokens),
		optCost(r.CostUSD), r.RequestID,
	}
}

func parseStatusClass(v string) (int, error) {
	if len(v) == 3 && v[0] >= '1' && v[0] <= '5' && strings.EqualFold(v[1:], "xx") {
		return int(v[0] - '0'), nil
	}
	return 0, fmt.Errorf("want 1xx, 2xx, 3xx, 4xx, or 5xx, got %q", v)
}

// showRequest prints one log row in full.
func showRequest(st *store.Store, requestID string, asJSON bool) int {
	r, err := st.RequestByRequestID(requestID)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "no request with id %q\n", requestID)
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(r)
		return 0
	}

	ttfb := "-"
	if r.TTFBMS != nil {
		ttfb = millis(*r.TTFBMS)
	}
	fields := [][2]string{
		{"request_id", r.RequestID},
		{"time", r.TS.Local().Format("2006-01-02 15:04:05.000 MST")},
		{"protocol", r.Protocol},
		{"operation", r.Operation},
		{"client_model", r.ClientModel},
		{"route", optString(r.Route)},
		{"provider", optString(r.Provider)},
		{"upstream_model", optString(r.UpstreamModel)},
		{"upstream_request_id", optString(r.UpstreamRequestID)},
		{"status", strconv.Itoa(r.Status)},
		{"outcome", r.Outcome},
		{"stream", strconv.FormatBool(r.Stream)},
		{"attempts", strconv.Itoa(r.Attempts)},
		{"attempts_ms", millis(r.AttemptsMS)},
		{"ttfb_ms", ttfb},
		{"total_ms", millis(r.TotalMS)},
		{"affinity_key", optString(r.AffinityKey)},
		{"input_tokens", optCount(r.InputTokens)},
		{"cache_read_tokens", optCount(r.CacheReadTokens)},
		{"cache_write_tokens", optCount(r.CacheWriteTokens)},
		{"cache_write_1h_tokens", optCount(r.CacheWrite1hTokens)},
		{"output_tokens", optCount(r.OutputTokens)},
		{"reasoning_tokens", optCount(r.ReasoningTokens)},
		{"input_image_tokens", optCount(r.InputImageTokens)},
		{"output_image_tokens", optCount(r.OutputImageTokens)},
		{"cost_usd", optCost(r.CostUSD)},
		{"price_key", optString(r.PriceKey)},
	}
	for _, f := range fields {
		fmt.Printf("%-22s %s\n", f[0], f[1])
	}
	for _, j := range []struct {
		name string
		text *string
	}{
		{"routing_json", &r.RoutingJSON},
		{"attempt_log", &r.AttemptLog},
		{"usage_json", r.UsageJSON},
		{"price_json", r.PriceJSON},
	} {
		fmt.Printf("\n%s:\n", j.name)
		if j.text == nil {
			fmt.Println("  -")
			continue
		}
		var b bytes.Buffer
		if err := json.Indent(&b, []byte(*j.text), "  ", "  "); err != nil {
			fmt.Println("  " + *j.text) // not valid JSON: shown as stored
			continue
		}
		fmt.Println("  " + b.String())
	}
	if r.Error != nil {
		fmt.Printf("\nerror:\n%s\n", *r.Error)
	}
	return 0
}

func status(args []string) int {
	fs, cfgPath := newFlags("status", "status [-c config.yaml]")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		fs.Usage()
		return 2
	}
	cfg, err := config.LoadLenient(configPath(*cfgPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// A server listening on every interface is reached over loopback.
	host, port, _ := net.SplitHostPort(cfg.Server.Listen)
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
		if ip != nil && ip.To4() == nil {
			host = "::1"
		}
	}
	addr := net.JoinHostPort(host, port)
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/ui/api/providers", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if cfg.Server.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Server.APIKey)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verbatim is not reachable at %s: %v\n", addr, err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "verbatim at %s: %v\n", addr, err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "verbatim at %s answered %s: %s\n", addr, resp.Status, bytes.TrimSpace(body))
		return 1
	}
	var v ui.ProvidersView
	if err := json.Unmarshal(body, &v); err != nil {
		fmt.Fprintf(os.Stderr, "verbatim at %s: unexpected response: %v\n", addr, err)
		return 1
	}

	t := newTable(column{name: "PROVIDER"}, column{name: "PROTOCOL"}, column{name: "BREAKER"},
		column{"FAILURES", true}, column{name: "OPEN UNTIL"}, column{"PINS", true}, column{"MODELS AGE", true},
		column{"COMMITTED", true}, column{"FAILOVER", true}, column{"ERROR EVENTS", true})
	var lines [][]string
	for _, e := range v.Endpoints {
		openUntil := "-"
		if e.Breaker.OpenUntil != nil {
			openUntil = e.Breaker.OpenUntil.Local().Format(time.TimeOnly)
		}
		age := "never"
		if e.ModelsAgeS != nil {
			age = (time.Duration(*e.ModelsAgeS) * time.Second).String()
		}
		lines = append(lines, []string{e.Provider, e.Protocol, e.Breaker.State, strconv.Itoa(e.Breaker.Failures), openUntil,
			strconv.Itoa(e.Pins), age, count(e.Committed), count(e.Failover), count(e.ErrorEvents)})
	}
	t.print(os.Stdout, lines, true)
	fmt.Printf("\npins: %d   log rows dropped: %d   (COMMITTED, FAILOVER and ERROR EVENTS cover the last hour)\n", v.PinsTotal, v.LogDropped)
	return 0
}
