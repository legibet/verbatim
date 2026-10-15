package route

import (
	"sync"
	"time"
)

// Breakers holds one circuit breaker per provider (design 8.3). It is safe
// for concurrent use.
type Breakers struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time // replaced in tests

	mu sync.Mutex
	m  map[string]*breaker
}

// breaker is closed when openUntil is zero. Otherwise it is open until
// openUntil, and half-open once that has passed; probing marks the one
// request admitted in half-open state.
type breaker struct {
	failures  int
	openUntil time.Time
	probing   bool
}

// State is a provider's breaker state for display.
type State struct {
	State     string    // "closed" | "open" | "half_open"
	Failures  int       // consecutive counted failures
	OpenUntil time.Time // zero unless open
}

// NewBreakers returns breakers that open after threshold consecutive counted
// failures and stay open for cooldown.
func NewBreakers(threshold int, cooldown time.Duration) *Breakers {
	return &Breakers{threshold: threshold, cooldown: cooldown, now: time.Now, m: make(map[string]*breaker)}
}

func (b *Breakers) get(provider string) *breaker {
	br := b.m[provider]
	if br == nil {
		br = &breaker{}
		b.m[provider] = br
	}
	return br
}

// Admit reports whether an attempt to provider may be dispatched now. A
// closed breaker admits everyone; an open one admits no one until the
// cooldown has passed, then admits exactly one probe and refuses everyone
// else until that probe reports Success, Failure, or Abandon. probe tells
// the caller it holds that probe, so only it calls Abandon.
func (b *Breakers) Admit(provider string) (ok, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.get(provider)
	switch {
	case br.openUntil.IsZero():
		return true, false
	case br.probing || b.now().Before(br.openUntil):
		return false, false
	default:
		br.probing = true
		return true, true
	}
}

// Success records a committed response: the breaker closes and the
// consecutive failure counter resets.
func (b *Breakers) Success(provider string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	*b.get(provider) = breaker{}
}

// Failure records a counted failure. A closed breaker opens when the counter
// reaches the threshold. probe is the flag Admit returned for this attempt:
// the probe's failure reopens the breaker for another cooldown, while a
// failure of an attempt admitted earlier only counts and leaves the probe
// in flight alone.
func (b *Breakers) Failure(provider string, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.get(provider)
	br.failures++
	switch {
	case probe:
		br.probing = false
		br.openUntil = b.now().Add(b.cooldown)
	case br.openUntil.IsZero() && br.failures >= b.threshold:
		br.openUntil = b.now().Add(b.cooldown)
	}
}

// Abandon records that a half-open probe ended without a verdict (the client
// cancelled). The breaker stays open with its cooldown already elapsed, so
// the next Admit probes again. It does nothing when no probe is in flight.
func (b *Breakers) Abandon(provider string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.get(provider).probing = false
}

// State returns the current state of provider's breaker.
func (b *Breakers) State(provider string) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state(b.get(provider))
}

// Snapshot returns the state of every provider the breakers have seen.
func (b *Breakers) Snapshot() map[string]State {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]State, len(b.m))
	for name, br := range b.m {
		out[name] = b.state(br)
	}
	return out
}

func (b *Breakers) state(br *breaker) State {
	switch {
	case br.openUntil.IsZero():
		return State{State: "closed", Failures: br.failures}
	case br.probing || !b.now().Before(br.openUntil):
		return State{State: "half_open", Failures: br.failures}
	default:
		return State{State: "open", Failures: br.failures, OpenUntil: br.openUntil}
	}
}
