// Package route matches client model names to routes, orders a route's
// targets into an attempt list, and keeps the per-endpoint circuit breakers.
package route

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"

	"verbatim/internal/config"
	"verbatim/internal/protocol"
)

// TargetID identifies a target: a provider and the upstream model name sent to it.
type TargetID struct{ Provider, Model string }

// Candidate is a route target resolved for one request.
type Candidate struct {
	TargetID
	Priority, Weight int
}

// Table is the routing table built once from the configuration. It is safe
// for concurrent use.
type Table struct {
	routes  []tableRoute
	balance map[string]string // route pattern -> balance

	// intN draws a uniform integer in [0, n) for weighted selection.
	// Tests replace it with a seeded source.
	intN func(n int) int

	mu       sync.Mutex
	counters map[rrKey]uint64
}

type tableRoute struct {
	pattern string
	targets []tableTarget
}

// tableTarget is a configured target for one protocol it serves.
type tableTarget struct {
	provider, model  string
	priority, weight int
	proto            protocol.ID
}

// rrKey identifies one round-robin counter: per route, protocol, and tier.
type rrKey struct {
	route    string
	proto    protocol.ID
	priority int
}

// New builds the table from a loaded, validated configuration.
func New(cfg *config.Config) *Table {
	t := &Table{
		balance:  make(map[string]string, len(cfg.Routes)),
		intN:     rand.IntN,
		counters: make(map[rrKey]uint64),
	}
	for _, r := range cfg.Routes {
		tr := tableRoute{pattern: r.Model}
		for _, tg := range r.Targets {
			provider, _ := tg.Split()
			for _, proto := range cfg.Protocols(tg) {
				tr.targets = append(tr.targets, tableTarget{provider, tg.Model, tg.Priority, tg.Weight, protocol.ID(proto)})
			}
		}
		t.routes = append(t.routes, tr)
		t.balance[r.Model] = r.Balance
	}
	return t
}

// Match finds the first route whose pattern matches clientModel and that has
// at least one target that serves proto (design 6.3). The
// candidates are that route's targets filtered to proto, in configuration
// order, with Model resolved by upstream.
func (t *Table) Match(proto protocol.ID, clientModel string) (route string, cands []Candidate, ok bool) {
	for _, r := range t.routes {
		if !config.MatchGlob(r.pattern, clientModel) {
			continue
		}
		for _, tg := range r.targets {
			if tg.proto != proto {
				continue
			}
			cands = append(cands, Candidate{
				TargetID: TargetID{Provider: tg.provider, Model: r.upstream(tg.model, clientModel)},
				Priority: tg.priority,
				Weight:   tg.weight,
			})
		}
		if len(cands) > 0 {
			return r.pattern, cands, true
		}
	}
	return "", nil, false
}

// upstream resolves a target's model for clientModel (design 6.3): an absent
// model forwards clientModel, a literal is sent as is, and a '*' is replaced
// with the text the route pattern's '*' matched.
func (r tableRoute) upstream(model, clientModel string) string {
	if model == "" {
		return clientModel
	}
	before, after, ok := strings.Cut(model, "*")
	if !ok {
		return model
	}
	s, _ := config.Capture(r.pattern, clientModel)
	return before + s + after
}

// Order builds the attempt order (design 6.4 step 2): tiers by ascending
// priority; within a tier, weighted draws without replacement with
// probability proportional to weight, and round_robin rotates the tier from
// a counter kept per (route, protocol, tier). cands is not modified.
func (t *Table) Order(route string, proto protocol.ID, cands []Candidate) []Candidate {
	sorted := slices.Clone(cands)
	slices.SortStableFunc(sorted, func(a, b Candidate) int { return cmp.Compare(a.Priority, b.Priority) })

	out := make([]Candidate, 0, len(sorted))
	for len(sorted) > 0 {
		n := 1
		for n < len(sorted) && sorted[n].Priority == sorted[0].Priority {
			n++
		}
		tier := sorted[:n]
		sorted = sorted[n:]
		if t.balance[route] == "round_robin" {
			out = append(out, t.rotate(rrKey{route, proto, tier[0].Priority}, tier)...)
		} else {
			out = append(out, t.weighted(tier)...)
		}
	}
	return out
}

func (t *Table) rotate(k rrKey, tier []Candidate) []Candidate {
	t.mu.Lock()
	c := t.counters[k]
	t.counters[k] = c + 1
	t.mu.Unlock()
	i := int(c % uint64(len(tier)))
	return append(slices.Clone(tier[i:]), tier[:i]...)
}

func (t *Table) weighted(tier []Candidate) []Candidate {
	rest := slices.Clone(tier)
	out := make([]Candidate, 0, len(rest))
	for len(rest) > 0 {
		total := 0
		for _, c := range rest {
			total += c.Weight
		}
		n := t.intN(total)
		i := 0
		for n >= rest[i].Weight {
			n -= rest[i].Weight
			i++
		}
		out = append(out, rest[i])
		rest = slices.Delete(rest, i, i+1)
	}
	return out
}

// Front moves pinned to the front of cands if it is among them (design 6.4
// step 3). It returns cands unchanged otherwise.
func Front(cands []Candidate, pinned TargetID) []Candidate {
	i := slices.IndexFunc(cands, func(c Candidate) bool { return c.TargetID == pinned })
	if i <= 0 {
		return cands
	}
	out := make([]Candidate, 0, len(cands))
	out = append(out, cands[i])
	out = append(out, cands[:i]...)
	return append(out, cands[i+1:]...)
}
