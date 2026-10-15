package main

import (
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"", time.Time{}},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"2026-10-01", time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)},
		{"2026-10-01T08:00:00Z", time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)},
	} {
		got, err := parseWhen(tc.in, now)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("%q: got %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if _, err := parseWhen("yesterday", now); err == nil {
		t.Error("yesterday: want an error")
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	limit := fs.Int("limit", 50, "")
	pos, _, ok := parse(fs, []string{"--limit", "5", "abc", "--json"})
	if !ok || !slices.Equal(pos, []string{"abc"}) || !*asJSON || *limit != 5 {
		t.Errorf("pos %v ok %v json %v limit %d", pos, ok, *asJSON, *limit)
	}
	if _, code, ok := parse(fs, []string{"-h"}); ok || code != 0 {
		t.Errorf("-h: ok %v code %d", ok, code)
	}
	if _, code, ok := parse(fs, []string{"--nope"}); ok || code != 2 {
		t.Errorf("bad flag: ok %v code %d", ok, code)
	}
}

func TestCount(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -45678: "-45,678"} {
		if got := count(in); got != want {
			t.Errorf("count(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTable(t *testing.T) {
	var b strings.Builder
	tb := newTable(column{name: "NAME"}, column{"N", true}, column{name: "NOTE"})
	tb.print(&b, [][]string{{"a", "1", "x"}, {"longer", "1,000", "y"}}, true)
	tb.print(&b, [][]string{{"b", "2", "z"}}, false) // a later batch keeps the widths
	want := "" +
		"NAME        N  NOTE\n" +
		"a           1  x\n" +
		"longer  1,000  y\n" +
		"b           2  z\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}
