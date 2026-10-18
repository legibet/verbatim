package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"verbatim/internal/affinity"
	"verbatim/internal/config"
	"verbatim/internal/models"
	"verbatim/internal/pricing"
	"verbatim/internal/proxy"
	"verbatim/internal/route"
	"verbatim/internal/store"
	"verbatim/internal/ui"
)

const (
	maxPins         = 100_000
	shutdownTimeout = 30 * time.Second
	// handlerDrainTimeout bounds the wait for handlers to record their rows
	// after their connections were closed at shutdown.
	handlerDrainTimeout = 5 * time.Second
	pricingInterval     = time.Minute
)

func serve(args []string) int {
	fs, cfgPath := newFlags("serve", "[serve] [-c config.yaml] [-v]")
	verbose := fs.Bool("v", false, "log at debug level")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		fs.Usage()
		return 2
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	if err := runServer(configPath(*cfgPath)); err != nil {
		slog.Error(err.Error())
		return 1
	}
	return 0
}

func runServer(path string) error {
	cfg, warnings, err := config.Load(path)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		slog.Warn("config: " + w.String())
	}
	rules, err := affinity.Compile(cfg.Affinity)
	if err != nil {
		return err
	}
	prices := loadPricing(cfg)

	st, err := store.Open(cfg.Log.Path, cfg.Log.Retention.Std())
	if err != nil {
		return err
	}
	defer st.Close()

	// bg outlives the HTTP server so that the model cache and the pricing
	// watcher keep running while in-flight requests drain.
	bg, stopBG := context.WithCancel(context.Background())
	defer stopBG()
	mc := models.New(cfg, http.DefaultTransport, cfg.Models.CacheTTL.Std(), cfg.Models.Timeout.Std())
	mc.Start(bg)

	h := proxy.New(proxy.Options{
		Config:   cfg,
		Routes:   route.New(cfg),
		Breakers: route.NewBreakers(cfg.Breaker.FailureThreshold, cfg.Breaker.Cooldown.Std()),
		Pins:     affinity.NewPins(cfg.Affinity.TTL.Std(), maxPins),
		Rules:    rules,
		Store:    st,
		Models:   mc,
		Pricing:  prices,
		Logger:   slog.Default(),
	})
	// The watcher and a price sync from the UI both reload the table; one
	// lock around loading and installing keeps an older table from replacing
	// a newer one.
	var pricingMu sync.Mutex
	reloadPricing := func() (*pricing.Table, []string, error) {
		pricingMu.Lock()
		defer pricingMu.Unlock()
		p := cfg.Pricing
		t, warnings, err := pricing.Load(p.Cache, p.Sources, p.Overrides, p.Aliases)
		if err == nil {
			h.SetPricing(t)
		}
		return t, warnings, err
	}
	go pricing.Watch(bg, pricingInterval, cfg.Pricing.Cache, cfg.Pricing.Sources, cfg.Pricing.Overrides, reloadPricing)

	mux := http.NewServeMux()
	mux.Handle("/ui/", ui.New(ui.Options{Store: st, Stats: h.Stats, Config: cfg, APIKey: cfg.Server.APIKey, ReloadPricing: reloadPricing}))
	mux.Handle("/", h)
	// No WriteTimeout: streamed responses run as long as the upstream does,
	// bounded by server.idle_timeout inside the proxy.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	slog.Info("verbatim listening", "addr", ln.Addr().String(), "providers", len(cfg.Providers),
		"routes", len(cfg.Routes), "db", cfg.Log.Path, "config", path)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	stop() // a second signal terminates immediately

	slog.Info("shutting down, waiting for in-flight requests", "timeout", shutdownTimeout)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		slog.Warn("in-flight requests did not finish in time; closing their connections", "err", err)
		srv.Close()
	}
	// Close does not wait for handlers whose connections it cut; their rows
	// must reach the store before it closes.
	if !h.Wait(handlerDrainTimeout) {
		slog.Warn("some requests were still running after their connections were closed; their log rows are lost")
	}
	stopBG()
	// Close drains the log queue, so every finished request is written.
	if err := st.Close(); err != nil {
		return err
	}
	slog.Info("stopped")
	return nil
}

// loadPricing loads the price table for serving. Pricing only labels the log
// with costs, so a problem with it is a warning and the gateway starts with
// an empty table; the watcher replaces it once the files are fixed.
func loadPricing(cfg *config.Config) *pricing.Table {
	t, warnings, err := pricing.Load(cfg.Pricing.Cache, cfg.Pricing.Sources, cfg.Pricing.Overrides, cfg.Pricing.Aliases)
	if err != nil {
		slog.Warn("pricing table not loaded; costs will be empty", "err", err)
		t, _, _ = pricing.Load(cfg.Pricing.Cache, nil, "", nil) // no files: cannot fail
		return t
	}
	for _, w := range warnings {
		slog.Warn("pricing: " + w)
	}
	slog.Info("pricing table loaded", "models", len(t.Models()))
	return t
}

func check(args []string) int {
	fs, cfgPath := newFlags("check", "check [-c config.yaml]")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		fs.Usage()
		return 2
	}
	path := configPath(*cfgPath)
	cfg, warnings, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, w := range warnings {
		fmt.Printf("warning: %s\n", w)
	}

	fmt.Printf("config %s: %d providers, %d routes\n\n", path, len(cfg.Providers), len(cfg.Routes))
	t := newTable(column{name: "#", right: true}, column{name: "ROUTE"}, column{name: "MATCH"}, column{name: "TARGETS"})
	var rows [][]string
	for i, r := range cfg.Routes {
		match := "exact"
		if !config.IsExact(r.Model) {
			match = "pattern"
			if !*r.Discover {
				match += ", no discovery"
			}
		}
		var targets []string
		for _, tg := range r.Targets {
			s := tg.Provider
			if tg.Model != "" {
				s += " as " + tg.Model
			}
			if tg.Priority != 1 {
				s += " priority " + strconv.Itoa(tg.Priority)
			}
			targets = append(targets, s)
		}
		rows = append(rows, []string{strconv.Itoa(i), r.Model, match, strings.Join(targets, ", ")})
	}
	t.print(os.Stdout, rows, true)
	fmt.Println()

	prices, pw, err := pricing.Load(cfg.Pricing.Cache, cfg.Pricing.Sources, cfg.Pricing.Overrides, cfg.Pricing.Aliases)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pricing:", err)
		return 1
	}
	for _, w := range pw {
		fmt.Printf("warning: pricing: %s\n", w)
	}
	fmt.Printf("pricing: %d models from %d sources (%s)\n", len(prices.Models()), len(cfg.Pricing.Sources), cfg.Pricing.Cache)
	fmt.Println("ok")
	return 0
}
