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
	mc := newModels(cfg)
	h := proxy.New(proxy.Options{
		Config:  cfg,
		Models:  mc,
		Pins:    affinity.NewPins(cfg.Affinity.TTL.Std(), maxPins),
		Store:   st,
		Pricing: prices,
		Logger:  slog.Default(),
	})
	// The watcher, a price sync from the UI and a configuration reload all
	// reload the table; one lock around loading and installing keeps an
	// older table from replacing a newer one.
	var pricingMu sync.Mutex
	reloadPricing := func() (*pricing.Table, []string, error) {
		pricingMu.Lock()
		defer pricingMu.Unlock()
		p := h.Config().Pricing
		t, warnings, err := pricing.Load(p.Cache, p.Sources, p.Overrides, p.Aliases)
		if err == nil {
			h.SetPricing(t)
		}
		return t, warnings, err
	}
	// start runs a configuration's background work, its model list
	// refreshes and the pricing watcher, until the next one starts.
	stopCurrent := func() {}
	start := func(cfg *config.Config, mc *models.Cache) {
		stopCurrent()
		ctx, cancel := context.WithCancel(bg)
		stopCurrent = cancel
		mc.Start(ctx)
		go pricing.Watch(ctx, pricingInterval, cfg.Pricing.Cache, cfg.Pricing.Sources, cfg.Pricing.Overrides, reloadPricing)
	}
	start(cfg, mc)

	// SIGHUP and the UI both reload the configuration; one at a time.
	var configMu sync.Mutex
	reloadConfig := func() ([]string, error) {
		configMu.Lock()
		defer configMu.Unlock()
		next, cw, err := config.Load(path)
		if err == nil {
			if changed := restartOnly(h.Config(), next); len(changed) > 0 {
				err = fmt.Errorf("%s changed; restart verbatim to apply the new configuration", strings.Join(changed, ", "))
			}
		}
		if err != nil {
			slog.Warn("configuration not reloaded; still serving the previous one", "err", err)
			return nil, err
		}
		mc := newModels(next)
		start(next, mc)
		h.SetConfig(next, mc)
		var warnings []string
		for _, w := range cw {
			warnings = append(warnings, "config: "+w.String())
		}
		// The pricing section may have changed with the rest.
		if _, pw, err := reloadPricing(); err != nil {
			warnings = append(warnings, "pricing: table not reloaded; keeping the previous one: "+err.Error())
		} else {
			for _, w := range pw {
				warnings = append(warnings, "pricing: "+w)
			}
		}
		for _, w := range warnings {
			slog.Warn(w)
		}
		slog.Info("configuration reloaded", "providers", len(next.Providers), "routes", len(next.Routes))
		return warnings, nil
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			reloadConfig()
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/ui/", ui.New(ui.Options{Store: st, Stats: h.Stats, Config: h.Config, Pricing: h.Pricing, ReloadPricing: reloadPricing, ReloadConfig: reloadConfig}))
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

func newModels(cfg *config.Config) *models.Cache {
	return models.New(cfg, http.DefaultTransport, cfg.Models.CacheTTL.Std(), cfg.Models.Timeout.Std())
}

// restartOnly names the settings changed from old to next that a running
// server keeps: its listener, its log database, and its pin store.
func restartOnly(old, next *config.Config) []string {
	var names []string
	for _, f := range []struct {
		name    string
		changed bool
	}{
		{"server.listen", old.Server.Listen != next.Server.Listen},
		{"log.path", old.Log.Path != next.Log.Path},
		{"log.retention", old.Log.Retention != next.Log.Retention},
		{"affinity.ttl", old.Affinity.TTL != next.Affinity.TTL},
	} {
		if f.changed {
			names = append(names, f.name)
		}
	}
	return names
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
	fs, cfgPath := newFlags("check", "check [-c config.yaml] [--probe]")
	probing := fs.Bool("probe", false, "also send an empty request to every endpoint to check its base_url and key")
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

	if *probing {
		fmt.Println("\nprobe: an empty request to each endpoint, which generates nothing")
		var rows [][]string
		failed := 0
		for _, r := range probe(cfg) {
			result := "ok"
			if r.problem != "" {
				result = r.problem
				failed++
			}
			rows = append(rows, []string{r.endpoint, result})
		}
		newTable(column{name: "ENDPOINT"}, column{name: "RESULT"}).print(os.Stdout, rows, false)
		if failed > 0 {
			fmt.Printf("%d of %d endpoints failed\n", failed, len(rows))
			return 1
		}
	}
	fmt.Println("ok")
	return 0
}
