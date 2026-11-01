package route

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestBreakers() (*Breakers, *clock) {
	c := &clock{t: time.Unix(1000, 0)}
	b := NewBreakers(3, 30*time.Second)
	b.now = c.now
	return b, c
}

func TestBreakerOpensAtThreshold(t *testing.T) {
	b, c := newTestBreakers()
	b.Failure("p", false)
	b.Failure("p", false)
	if !admit(b, "p") || b.State("p").State != "closed" {
		t.Fatal("opened before threshold")
	}
	b.Failure("p", false)
	st := b.State("p")
	if st.State != "open" || st.Failures != 3 || !st.OpenUntil.Equal(c.t.Add(30*time.Second)) {
		t.Fatalf("state %+v", st)
	}
	if admit(b, "p") {
		t.Fatal("admitted while open")
	}
	if !admit(b, "other") {
		t.Fatal("breakers are not per endpoint")
	}
}

func TestBreakerSuccessResetsCounter(t *testing.T) {
	b, _ := newTestBreakers()
	b.Failure("p", false)
	b.Failure("p", false)
	b.Success("p")
	b.Failure("p", false)
	b.Failure("p", false)
	if st := b.State("p"); st.State != "closed" || st.Failures != 2 {
		t.Fatalf("state %+v", st)
	}
}

func TestBreakerHalfOpen(t *testing.T) {
	b, c := newTestBreakers()
	for range 3 {
		b.Failure("p", false)
	}
	c.t = c.t.Add(30 * time.Second)
	if st := b.State("p"); st.State != "half_open" {
		t.Fatalf("after cooldown: %+v", st)
	}
	if !b.WouldAdmit("p") {
		t.Fatal("WouldAdmit refused the free probe")
	}
	if !admit(b, "p") {
		t.Fatal("probe not admitted after cooldown")
	}
	if admit(b, "p") || b.WouldAdmit("p") {
		t.Fatal("second caller admitted while probing")
	}

	// A failed probe reopens for another cooldown.
	b.Failure("p", true)
	if st := b.State("p"); st.State != "open" || !st.OpenUntil.Equal(c.t.Add(30*time.Second)) {
		t.Fatalf("after failed probe: %+v", st)
	}
	if admit(b, "p") || b.WouldAdmit("p") {
		t.Fatal("admitted right after failed probe")
	}

	// A successful probe closes.
	c.t = c.t.Add(30 * time.Second)
	if !admit(b, "p") {
		t.Fatal("second probe not admitted")
	}
	b.Success("p")
	if st := b.State("p"); st.State != "closed" || st.Failures != 0 || !st.OpenUntil.IsZero() {
		t.Fatalf("after successful probe: %+v", st)
	}
	for range 2 {
		if !admit(b, "p") || !b.WouldAdmit("p") {
			t.Fatal("closed breaker refused")
		}
	}
}

func TestBreakerAbandon(t *testing.T) {
	b, c := newTestBreakers()
	for range 3 {
		b.Failure("p", false)
	}
	c.t = c.t.Add(time.Minute)
	if !admit(b, "p") {
		t.Fatal("probe not admitted")
	}
	b.Abandon("p")
	if !admit(b, "p") {
		t.Fatal("no new probe after abandon")
	}
	if admit(b, "p") {
		t.Fatal("two probes after abandon")
	}
}

func TestBreakerFailureWhileOpenDoesNotExtend(t *testing.T) {
	b, c := newTestBreakers()
	for range 3 {
		b.Failure("p", false)
	}
	until := b.State("p").OpenUntil
	c.t = c.t.Add(10 * time.Second)
	b.Failure("p", false) // a straggler admitted before the breaker opened
	if st := b.State("p"); !st.OpenUntil.Equal(until) || st.Failures != 4 {
		t.Fatalf("state %+v", st)
	}
}

func TestBreakerHalfOpenAdmitsExactlyOneConcurrently(t *testing.T) {
	b, c := newTestBreakers()
	for range 3 {
		b.Failure("p", false)
	}
	c.t = c.t.Add(time.Minute)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if admit(b, "p") {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if n := admitted.Load(); n != 1 {
		t.Fatalf("%d probes admitted", n)
	}
}

func TestBreakerSnapshot(t *testing.T) {
	b, _ := newTestBreakers()
	admit(b, "a")
	for range 3 {
		b.Failure("b", false)
	}
	s := b.Snapshot()
	if len(s) != 2 || s["a"].State != "closed" || s["b"].State != "open" {
		t.Fatalf("snapshot %+v", s)
	}
}

// admit is Admit without the probe flag, for tests that only care about admission.
func admit(b *Breakers, provider string) bool {
	ok, _ := b.Admit(provider)
	return ok
}

func TestAdmitReportsProbe(t *testing.T) {
	b := NewBreakers(1, time.Second)
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	if _, probe := b.Admit("p"); probe {
		t.Fatal("closed breaker reported a probe")
	}
	b.Failure("p", false)
	now = now.Add(2 * time.Second)
	if ok, probe := b.Admit("p"); !ok || !probe {
		t.Fatalf("after cooldown: ok=%v probe=%v, want true true", ok, probe)
	}
	if ok, _ := b.Admit("p"); ok {
		t.Fatal("second caller admitted during probe")
	}
}

func TestBreakerStaleFailureLeavesProbeAlone(t *testing.T) {
	b := NewBreakers(1, time.Second)
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	b.Failure("p", false) // opens
	now = now.Add(2 * time.Second)
	if ok, probe := b.Admit("p"); !ok || !probe {
		t.Fatal("expected the probe to be admitted")
	}
	// An attempt admitted while the breaker was still closed fails late: it
	// must not end the probe or restart the cooldown.
	b.Failure("p", false)
	if st := b.State("p"); st.State != "half_open" {
		t.Fatalf("state = %q, want half_open", st.State)
	}
	if ok, _ := b.Admit("p"); ok {
		t.Fatal("a second probe was admitted while the first is in flight")
	}
	b.Success("p")
	if st := b.State("p"); st.State != "closed" {
		t.Fatalf("state after probe success = %q", st.State)
	}
}
