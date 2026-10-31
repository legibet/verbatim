// Command verbatim is a small, self-hosted LLM API gateway. See
// docs/architecture.md for the design; this file dispatches the CLI commands.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"verbatim/internal/config"
	"verbatim/internal/store"
)

const usageText = `verbatim is a small LLM API gateway.

Usage:
  verbatim [serve] [-c config.yaml] [-v]   run the gateway (default command)
  verbatim check   [-c config.yaml] [--probe]  validate the configuration; --probe also tries each endpoint
  verbatim usage   [--since 7d] [--by model|provider|route|day] [--protocol p] [--json]
  verbatim log     [--tail] [--since 1h] [--status 5xx] [--provider p] [--errors] [--json]
  verbatim log     <request_id>            one request in full
  verbatim pricing sync                    download pricing.sources into pricing.cache
  verbatim pricing show <model>            the price entry for a client model name
  verbatim status                          live endpoint state from the running server

` + configHelp + `
Run 'verbatim <command> -h' for a command's flags.
`

const configHelp = `Every command takes -c <file>. Without it, the configuration is ./verbatim.yaml
if that file exists, else ~/.verbatim/verbatim.yaml.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && slices.Contains([]string{"-h", "-help", "--help"}, args[0]) {
		fmt.Print(usageText)
		return 0
	}
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(args)
	case "check":
		return check(args)
	case "usage":
		return usage(args)
	case "log":
		return logCmd(args)
	case "pricing":
		return pricingCmd(args)
	case "status":
		return status(args)
	case "help":
		fmt.Print(usageText)
		return 0
	}
	fmt.Fprintf(os.Stderr, "verbatim: unknown command %q\n\n%s", cmd, usageText)
	return 2
}

// newFlags returns a flag set for a command with the shared -c flag.
func newFlags(name, synopsis string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfgPath := fs.String("c", "", "configuration file")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: verbatim %s\n\n", synopsis)
		fs.PrintDefaults()
		fmt.Fprintf(fs.Output(), "\n%s", configHelp)
	}
	return fs, cfgPath
}

// parse parses flags and positional arguments in any order, so that
// "verbatim log <id> --json" works. ok is false when the command should exit
// with code: 0 after -h, 2 after a bad flag (the flag package has printed why).
func parse(fs *flag.FlagSet, args []string) (positional []string, code int, ok bool) {
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, 0, false
			}
			return nil, 2, false
		}
		if fs.NArg() == 0 {
			return positional, 0, true
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// configPath resolves the -c flag to the configuration file to load.
func configPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if _, err := os.Stat("verbatim.yaml"); err == nil {
		return "verbatim.yaml"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "verbatim.yaml"
	}
	return filepath.Join(home, ".verbatim", "verbatim.yaml")
}

// openStore opens the log database named by the configuration for reading.
// Retention is disabled: deleting old rows is the server's job.
func openStore(cfgFlag string) (*store.Store, *config.Config, error) {
	cfg, err := config.LoadLenient(configPath(cfgFlag))
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(cfg.Log.Path); err != nil {
		return nil, nil, fmt.Errorf("log database: %w", err)
	}
	st, err := store.Open(cfg.Log.Path, 0)
	return st, cfg, err
}

// parseWhen parses a range bound: a duration back from now in the
// configuration syntax ("90m", "7d"), a local date ("2026-10-01"), or an
// RFC 3339 time. Empty is the zero time, meaning no bound.
func parseWhen(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := config.ParseDuration(s); err == nil {
		return now.Add(-d.Std()), nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a duration (7d, 90m), a date (2006-01-02), or an RFC 3339 time", s)
}

// table prints aligned columns: text left-aligned, numbers right-aligned.
// Widths only grow, so successive batches of a live tail stay aligned.
// text/tabwriter is not used because it aligns every column the same way.
type table struct {
	cols   []column
	widths []int
}

type column struct {
	name  string
	right bool
}

func newTable(cols ...column) *table {
	t := &table{cols: cols, widths: make([]int, len(cols))}
	for i, c := range cols {
		t.widths[i] = utf8.RuneCountInString(c.name)
	}
	return t
}

func (t *table) print(w io.Writer, rows [][]string, header bool) {
	for _, r := range rows {
		for i, cell := range r {
			t.widths[i] = max(t.widths[i], utf8.RuneCountInString(cell))
		}
	}
	if header {
		names := make([]string, len(t.cols))
		for i, c := range t.cols {
			names[i] = c.name
		}
		t.line(w, names)
	}
	for _, r := range rows {
		t.line(w, r)
	}
}

func (t *table) line(w io.Writer, cells []string) {
	var b strings.Builder
	for i, cell := range cells {
		pad := strings.Repeat(" ", t.widths[i]-utf8.RuneCountInString(cell))
		if i > 0 {
			b.WriteString("  ")
		}
		switch {
		case t.cols[i].right:
			b.WriteString(pad + cell)
		case i < len(cells)-1:
			b.WriteString(cell + pad)
		default:
			b.WriteString(cell)
		}
	}
	fmt.Fprintln(w, b.String())
}

// count formats an integer with thousands separators.
func count(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// optCount formats a nullable token count; NULL is "-", never 0.
func optCount(n *int64) string {
	if n == nil {
		return "-"
	}
	return count(*n)
}

func optCost(c *float64) string {
	if c == nil {
		return "-"
	}
	return usd(*c)
}

// usd formats a cost as the web UI does: four decimals below one dollar so
// that per-request costs stay aligned, "<0.0001" for an amount that would
// show as zero, and cents with thousands separators from one dollar up.
func usd(v float64) string {
	switch a := math.Abs(v); {
	case v == 0:
		return "0.0000"
	case a < 0.0001:
		return "<0.0001"
	case a < 1:
		return fmt.Sprintf("%.4f", v)
	}
	cents := int64(math.Round(v * 100))
	return fmt.Sprintf("%s.%02d", count(cents/100), max(cents%100, -(cents%100)))
}

func millis(ms int64) string {
	if ms < 1000 {
		return strconv.FormatInt(ms, 10) + "ms"
	}
	return fmt.Sprintf("%.2fs", float64(ms)/1000)
}

func optString(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}
