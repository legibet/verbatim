package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"verbatim/internal/config"
	"verbatim/internal/pricing"
)

const pricingUsage = `Usage:
  verbatim pricing sync [-c config.yaml]           download pricing.sources into pricing.cache
  verbatim pricing show [-c config.yaml] <model>   the price entry for a client model name
`

func pricingCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, pricingUsage)
		return 2
	}
	switch args[0] {
	case "sync":
		return pricingSync(args[1:])
	case "show":
		return pricingShow(args[1:])
	case "-h", "-help", "--help", "help":
		fmt.Print(pricingUsage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "verbatim pricing: unknown command %q\n\n%s", args[0], pricingUsage)
	return 2
}

func pricingSync(args []string) int {
	fs, cfgPath := newFlags("pricing sync", "pricing sync [-c config.yaml]")
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
	p := cfg.Pricing
	if len(p.Sources) == 0 {
		fmt.Fprintln(os.Stderr, "pricing.sources is empty; nothing to sync")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := pricing.Sync(ctx, p.Sources, p.Cache); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Each source is loaded alone for its own count; the merged table can be
	// smaller because a later source wins for ids both define.
	for _, src := range p.Sources {
		t, _, err := pricing.Load(p.Cache, []string{src}, "", nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("%6d models  %s\n", len(t.Models()), src)
	}
	t, warnings, err := pricing.Load(p.Cache, p.Sources, p.Overrides, p.Aliases)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, w := range warnings {
		fmt.Printf("warning: %s\n", w)
	}
	fmt.Printf("%6d models in the price table (%s)\n", len(t.Models()), p.Cache)
	return 0
}

func pricingShow(args []string) int {
	fs, cfgPath := newFlags("pricing show", "pricing show [-c config.yaml] <model>")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	model := pos[0]
	cfg, err := config.LoadLenient(configPath(*cfgPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	p := cfg.Pricing
	t, _, err := pricing.Load(p.Cache, p.Sources, p.Overrides, p.Aliases)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// The client name alone: the upstream name is per request.
	key, price, ok := t.Lookup(model, "")
	if !ok {
		fmt.Printf("no price for %q\n", model)
		return 1
	}

	usd := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	orInput := func(v *float64) string {
		if v == nil {
			return usd(price.Input) + " (input price)"
		}
		return usd(*v)
	}
	line := func(label, value string) { fmt.Printf("  %-15s %s\n", label, value) }
	fmt.Printf("%s\n", model)
	if key != model {
		line("priced as", key+" (pricing.aliases)")
	}
	line("source", price.Source)
	fmt.Println("USD per 1M tokens:")
	line("input", usd(price.Input))
	if price.Output != nil {
		line("output", usd(*price.Output))
	} else {
		line("output", "none")
	}
	if price.ImageInput != nil {
		line("image input", usd(*price.ImageInput))
	}
	if price.ImageOutput != nil {
		line("image output", usd(*price.ImageOutput))
	}
	if price.CacheRead != nil {
		line("cache read", usd(*price.CacheRead))
	} else {
		line("cache read", "none (requests with cache reads are not priced)")
	}
	line("cache write", orInput(price.CacheWrite))
	if m := price.CacheWrite1hMultiple; m != nil {
		line("1h cache write", fmt.Sprintf("%s (input x %s)", usd(price.Input**m), usd(*m)))
	} else {
		line("1h cache write", "same as cache write")
	}
	if price.Reasoning != nil {
		line("reasoning", usd(*price.Reasoning)+" (listed only; reasoning is priced as output)")
	}
	for _, tier := range price.Tiers {
		fmt.Printf("context above %d tokens:\n", tier.Size)
		for _, f := range []struct {
			name string
			v    *float64
		}{{"input", tier.Input}, {"output", tier.Output}, {"cache read", tier.CacheRead}, {"cache write", tier.CacheWrite}} {
			if f.v != nil {
				line(f.name, usd(*f.v))
			} else {
				line(f.name, "base price")
			}
		}
	}
	return 0
}
