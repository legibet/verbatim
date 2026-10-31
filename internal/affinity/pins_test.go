package affinity

import (
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/legibet/verbatim/internal/protocol"
	"github.com/legibet/verbatim/internal/route"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestPins(ttl time.Duration, max int) (*Pins, *clock) {
	c := &clock{t: time.Unix(1000, 0)}
	p := NewPins(ttl, max)
	p.now = c.now
	return p, c
}

var (
	ta = route.TargetID{Provider: "a", Model: "m"}
	tb = route.TargetID{Provider: "b", Model: "m"}
)

func pk(key string) PinKey { return PinKey{Protocol: protocol.Anthropic, Key: key, Model: "claude"} }

func TestPinsReserve(t *testing.T) {
	p, _ := newTestPins(time.Hour, 10)
	if got, reserved := p.Reserve(pk("s"), ta); got != ta || !reserved {
		t.Fatalf("first Reserve = %v %v", got, reserved)
	}
	if got, reserved := p.Reserve(pk("s"), tb); got != ta || reserved {
		t.Fatalf("second Reserve = %v %v", got, reserved)
	}
	// Pin identity includes protocol and client model.
	other := []PinKey{
		{Protocol: protocol.Chat, Key: "s", Model: "claude"},
		{Protocol: protocol.Anthropic, Key: "s", Model: "haiku"},
	}
	for _, k := range other {
		if _, ok := p.Get(k); ok {
			t.Fatalf("%v shares the pin", k)
		}
	}
}

func TestPinsReserveRace(t *testing.T) {
	for range 100 {
		p, _ := newTestPins(time.Hour, 10)
		var reserved atomic.Int32
		results := make([]route.TargetID, 8)
		var wg sync.WaitGroup
		for i := range results {
			wg.Go(func() {
				t := route.TargetID{Provider: string(rune('a' + i)), Model: "m"}
				got, r := p.Reserve(pk("s"), t)
				if r {
					reserved.Add(1)
				}
				results[i] = got
			})
		}
		wg.Wait()
		if n := reserved.Load(); n != 1 {
			t.Fatalf("%d reservations", n)
		}
		for _, r := range results {
			if r != results[0] {
				t.Fatalf("callers got different targets: %v", results)
			}
		}
	}
}

func TestPinsTTL(t *testing.T) {
	p, c := newTestPins(time.Hour, 10)
	p.Reserve(pk("s"), ta)
	c.t = c.t.Add(59 * time.Minute)
	if _, ok := p.Get(pk("s")); !ok {
		t.Fatal("expired early")
	}
	// Get refreshed the TTL.
	c.t = c.t.Add(59 * time.Minute)
	if _, ok := p.Get(pk("s")); !ok {
		t.Fatal("TTL not refreshed by Get")
	}
	c.t = c.t.Add(time.Hour)
	if _, ok := p.Get(pk("s")); ok {
		t.Fatal("not expired")
	}
	if p.Len() != 0 {
		t.Fatalf("Len = %d", p.Len())
	}
	// An expired session is new again.
	if got, reserved := p.Reserve(pk("s"), tb); got != tb || !reserved {
		t.Fatalf("Reserve after expiry = %v %v", got, reserved)
	}
}

func TestPinsLRUEviction(t *testing.T) {
	p, c := newTestPins(time.Hour, 2)
	p.Reserve(pk("1"), ta)
	c.t = c.t.Add(time.Second)
	p.Reserve(pk("2"), ta)
	c.t = c.t.Add(time.Second)
	p.Get(pk("1")) // 2 is now least recently used
	p.Reserve(pk("3"), tb)
	if _, ok := p.Get(pk("2")); ok {
		t.Fatal("least recently used pin not evicted")
	}
	for _, k := range []string{"1", "3"} {
		if _, ok := p.Get(pk(k)); !ok {
			t.Fatalf("pin %s evicted", k)
		}
	}
	if p.Len() != 2 {
		t.Fatalf("Len = %d", p.Len())
	}
}

func TestPinsCommitAndRelease(t *testing.T) {
	hits := []struct {
		name string
		use  func(*Pins)
	}{
		{"Get", func(p *Pins) { p.Get(pk("s")) }},
		{"Reserve", func(p *Pins) { p.Reserve(pk("s"), tb) }},
	}
	for _, hit := range hits {
		t.Run(hit.name, func(t *testing.T) {
			// A reserves ta, B finds the pin and commits on ta, A fails
			// entirely and releases: the pin has served a response and stays.
			p, _ := newTestPins(time.Hour, 10)
			if _, reserved := p.Reserve(pk("s"), ta); !reserved {
				t.Fatal("A did not reserve")
			}
			hit.use(p)
			p.Commit(pk("s"), ta)
			p.Release(pk("s"), ta)
			if got, ok := p.Get(pk("s")); !ok || got != ta {
				t.Fatalf("Release dropped a committed pin: %v %v", got, ok)
			}

			// A reserves, B hits, both fail: nothing was committed, so A's
			// release drops the pin.
			p, _ = newTestPins(time.Hour, 10)
			p.Reserve(pk("s"), ta)
			hit.use(p)
			p.Release(pk("s"), ta)
			if p.Len() != 0 {
				t.Fatalf("uncommitted reservation kept: Len = %d", p.Len())
			}
		})
	}

	t.Run("failover", func(t *testing.T) {
		// A reserves ta and commits on tb after failing over: the pin moves
		// and is committed, so no later Release drops it.
		p, _ := newTestPins(time.Hour, 10)
		p.Reserve(pk("s"), ta)
		p.Commit(pk("s"), tb)
		p.Release(pk("s"), ta)
		p.Release(pk("s"), tb)
		if got, ok := p.Get(pk("s")); !ok || got != tb {
			t.Fatalf("pin after commit on tb = %v %v", got, ok)
		}
	})

	t.Run("expired", func(t *testing.T) {
		// Commit re-creates a pin that expired while the request ran.
		p, c := newTestPins(time.Hour, 10)
		p.Reserve(pk("s"), ta)
		c.t = c.t.Add(2 * time.Hour)
		p.Commit(pk("s"), ta)
		p.Release(pk("s"), ta)
		if got, ok := p.Get(pk("s")); !ok || got != ta {
			t.Fatalf("pin after commit past expiry = %v %v", got, ok)
		}
	})
}

func TestPinsByEndpoint(t *testing.T) {
	p, c := newTestPins(time.Hour, 10)
	p.Reserve(pk("1"), ta)
	c.t = c.t.Add(30 * time.Minute)
	p.Reserve(pk("2"), ta)
	p.Reserve(pk("3"), tb)
	p.Reserve(PinKey{Protocol: protocol.Chat, Key: "4", Model: "claude"}, ta)
	if got := p.ByEndpoint(); !maps.Equal(got, map[string]int{"a.anthropic": 2, "b.anthropic": 1, "a.chat": 1}) {
		t.Fatalf("ByEndpoint = %v", got)
	}
	c.t = c.t.Add(45 * time.Minute) // pin 1 expires
	if got := p.ByEndpoint(); !maps.Equal(got, map[string]int{"a.anthropic": 1, "b.anthropic": 1, "a.chat": 1}) {
		t.Fatalf("ByEndpoint after expiry = %v", got)
	}
}
