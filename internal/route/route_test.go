package route

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/legibet/verbatim/internal/config"
	"github.com/legibet/verbatim/internal/protocol"
)

func testConfig() *config.Config {
	eps := func(protos ...string) config.Provider {
		p := config.Provider{Endpoints: map[string]config.Endpoint{}}
		for _, proto := range protos {
			p.Endpoints[proto] = config.Endpoint{}
		}
		return p
	}
	return &config.Config{
		Providers: map[string]config.Provider{
			"ms":    eps("anthropic", "chat"),
			"relay": eps("chat"),
			"or":    eps("anthropic", "chat"),
			"oa":    eps("responses", "chat"),
			"local": eps("chat"),
			"rr-a":  eps("chat"),
			"rr-b":  eps("chat"),
			"rr-c":  eps("chat"),
			"rr-d":  eps("chat"),
		},
		Routes: []config.Route{
			{Model: "kimi-k3", Balance: "weighted", Targets: []config.Target{
				{Provider: "ms", Priority: 1, Weight: 3},
				{Provider: "relay", Model: "kimi-k3-0905", Priority: 1, Weight: 1},
				{Provider: "or.anthropic", Model: "moonshotai/kimi-k3", Priority: 2, Weight: 1},
			}},
			{Model: "gpt-*", Balance: "weighted", Targets: []config.Target{
				{Provider: "oa.responses", Priority: 1, Weight: 1},
			}},
			{Model: "both", Balance: "weighted", Targets: []config.Target{
				{Provider: "oa", Priority: 1, Weight: 1},
			}},
			{Model: "rr", Balance: "round_robin", Targets: []config.Target{
				{Provider: "rr-a", Priority: 1, Weight: 5},
				{Provider: "rr-b", Priority: 1, Weight: 1},
				{Provider: "rr-c", Priority: 1, Weight: 1},
				{Provider: "rr-d", Priority: 2, Weight: 1},
			}},
			{Model: "claude-*", Balance: "weighted", Targets: []config.Target{
				{Provider: "or.anthropic", Model: "anthropic/claude-*", Priority: 1, Weight: 1},
				{Provider: "local", Model: "*:q4", Priority: 1, Weight: 1},
			}},
			{Model: "*", Balance: "weighted", Targets: []config.Target{
				{Provider: "or.anthropic", Priority: 1, Weight: 1},
				{Provider: "local", Priority: 1, Weight: 1},
			}},
		},
	}
}

func ids(cands []Candidate) []TargetID {
	out := make([]TargetID, len(cands))
	for i, c := range cands {
		out[i] = c.TargetID
	}
	return out
}

func TestMatch(t *testing.T) {
	tab := New(testConfig())
	tests := []struct {
		name  string
		proto protocol.ID
		model string
		route string
		want  []TargetID
	}{
		{"exact, filtered to anthropic, model rewrite", protocol.Anthropic, "kimi-k3", "kimi-k3",
			[]TargetID{{"ms", "kimi-k3"}, {"or", "moonshotai/kimi-k3"}}},
		{"exact, filtered to chat; an endpoint target serves only its protocol", protocol.Chat, "kimi-k3", "kimi-k3",
			[]TargetID{{"ms", "kimi-k3"}, {"relay", "kimi-k3-0905"}}},
		{"pattern with slash-free name", protocol.Responses, "gpt-6", "gpt-*",
			[]TargetID{{"oa", "gpt-6"}}},
		{"name matches gpt-* but no chat target: falls through to catch-all", protocol.Chat, "gpt-6", "*",
			[]TargetID{{"local", "gpt-6"}}},
		{"provider target serves responses", protocol.Responses, "both", "both", []TargetID{{"oa", "both"}}},
		{"provider target serves chat", protocol.Chat, "both", "both", []TargetID{{"oa", "both"}}},
		{"provider without the protocol: falls through", protocol.Anthropic, "both", "*", []TargetID{{"or", "both"}}},
		{"catch-all matches slashes", protocol.Anthropic, "anthropic/claude-x", "*",
			[]TargetID{{"or", "anthropic/claude-x"}}},
		{"target model star takes what the route star matched", protocol.Anthropic, "claude-opus-5-5", "claude-*",
			[]TargetID{{"or", "anthropic/claude-opus-5-5"}}},
		{"star with a suffix", protocol.Chat, "claude-opus", "claude-*", []TargetID{{"local", "opus:q4"}}},
		{"route star matched nothing", protocol.Anthropic, "claude-", "claude-*", []TargetID{{"or", "anthropic/claude-"}}},
		{"exact route has no responses target: falls through, catch-all has none either", protocol.Responses, "kimi-k3", "", nil},
		{"case-sensitive", protocol.Responses, "GPT-6", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, cands, ok := tab.Match(tt.proto, tt.model)
			if ok != (tt.want != nil) || route != tt.route || !slices.Equal(ids(cands), tt.want) {
				t.Fatalf("Match = %q %v %v, want %q %v", route, ids(cands), ok, tt.route, tt.want)
			}
		})
	}
}

func TestMatchKeepsPriorityAndWeight(t *testing.T) {
	_, cands, _ := New(testConfig()).Match(protocol.Anthropic, "kimi-k3")
	if cands[1].Priority != 2 || cands[1].Weight != 1 || cands[0].Priority != 1 {
		t.Fatalf("cands = %+v", cands)
	}
}

func TestOrderRoundRobin(t *testing.T) {
	tab := New(testConfig())
	_, cands, _ := tab.Match(protocol.Chat, "rr")
	want := [][]string{
		{"rr-a", "rr-b", "rr-c", "rr-d"},
		{"rr-b", "rr-c", "rr-a", "rr-d"},
		{"rr-c", "rr-a", "rr-b", "rr-d"},
		{"rr-a", "rr-b", "rr-c", "rr-d"},
	}
	for i, w := range want {
		var got []string
		for _, c := range tab.Order("rr", protocol.Chat, cands) {
			got = append(got, c.Provider)
		}
		if !slices.Equal(got, w) {
			t.Fatalf("call %d: order %v, want %v", i, got, w)
		}
	}
	// The counter is per protocol: another protocol starts from zero.
	other := []Candidate{{TargetID: TargetID{"x", "m"}, Priority: 1}, {TargetID: TargetID{"y", "m"}, Priority: 1}}
	if got := tab.Order("rr", protocol.Anthropic, other); got[0].Provider != "x" {
		t.Fatalf("anthropic counter not independent: %v", ids(got))
	}
}

func TestOrderTiersAndInputUnchanged(t *testing.T) {
	tab := New(testConfig())
	cands := []Candidate{
		{TargetID: TargetID{"p3", "m"}, Priority: 3, Weight: 1},
		{TargetID: TargetID{"p1", "m"}, Priority: 1, Weight: 1},
		{TargetID: TargetID{"p2", "m"}, Priority: 2, Weight: 1},
	}
	orig := slices.Clone(cands)
	got := tab.Order("kimi-k3", protocol.Chat, cands)
	if p := []string{got[0].Provider, got[1].Provider, got[2].Provider}; !slices.Equal(p, []string{"p1", "p2", "p3"}) {
		t.Fatalf("order %v", p)
	}
	if !slices.Equal(cands, orig) {
		t.Fatal("Order modified its input")
	}
}

func TestOrderWeighted(t *testing.T) {
	tab := New(testConfig())
	tab.intN = rand.New(rand.NewPCG(1, 2)).IntN
	cands := []Candidate{
		{TargetID: TargetID{"a", "m"}, Priority: 1, Weight: 1},
		{TargetID: TargetID{"b", "m"}, Priority: 1, Weight: 3},
		{TargetID: TargetID{"c", "m"}, Priority: 1, Weight: 1},
		{TargetID: TargetID{"z", "m"}, Priority: 2, Weight: 100},
	}
	const n = 20000
	first := map[string]int{}
	for range n {
		got := tab.Order("kimi-k3", protocol.Chat, cands)
		if len(got) != 4 || got[3].Provider != "z" {
			t.Fatalf("lower tier not last or candidate lost: %v", ids(got))
		}
		seen := map[string]bool{}
		for _, c := range got[:3] {
			seen[c.Provider] = true
		}
		if len(seen) != 3 {
			t.Fatalf("draw with replacement: %v", ids(got))
		}
		first[got[0].Provider]++
	}
	for p, want := range map[string]float64{"a": 0.2, "b": 0.6, "c": 0.2} {
		if got := float64(first[p]) / n; got < want-0.02 || got > want+0.02 {
			t.Errorf("%s first with frequency %.3f, want about %.1f", p, got, want)
		}
	}

	// Determinism with a seeded source.
	run := func() [][]TargetID {
		tab.intN = rand.New(rand.NewPCG(7, 7)).IntN
		var out [][]TargetID
		for range 10 {
			out = append(out, ids(tab.Order("kimi-k3", protocol.Chat, cands)))
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if !slices.Equal(a[i], b[i]) {
			t.Fatalf("seeded runs differ at %d: %v vs %v", i, a[i], b[i])
		}
	}
}

func TestFront(t *testing.T) {
	c := func(p string) Candidate { return Candidate{TargetID: TargetID{p, "m"}} }
	cands := []Candidate{c("a"), c("b"), c("c")}
	tests := []struct {
		pinned TargetID
		want   []string
	}{
		{TargetID{"c", "m"}, []string{"c", "a", "b"}},
		{TargetID{"b", "m"}, []string{"b", "a", "c"}},
		{TargetID{"a", "m"}, []string{"a", "b", "c"}},
		{TargetID{"c", "other"}, []string{"a", "b", "c"}},
		{TargetID{}, []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		var got []string
		for _, x := range Front(cands, tt.pinned) {
			got = append(got, x.Provider)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("Front(%v) = %v, want %v", tt.pinned, got, tt.want)
		}
	}
	if cands[0].Provider != "a" || cands[2].Provider != "c" {
		t.Fatal("Front modified its input")
	}
}
