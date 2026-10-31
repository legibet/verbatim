package affinity

import (
	"container/list"
	"sync"
	"time"

	"github.com/legibet/verbatim/internal/config"
	"github.com/legibet/verbatim/internal/protocol"
	"github.com/legibet/verbatim/internal/route"
)

// PinKey identifies a pin (docs/affinity.md): the same session key under
// another protocol or for another client model is an independent pin.
type PinKey struct {
	Protocol protocol.ID
	Key      string // from Rules.Key
	Model    string // client model name
}

// Pins is the in-memory pin store: LRU-bounded, with a TTL refreshed on every
// use. It is safe for concurrent use.
type Pins struct {
	ttl time.Duration
	max int
	now func() time.Time // replaced in tests

	mu  sync.Mutex
	m   map[PinKey]*list.Element
	lru *list.List // of *pin; front is most recently used
}

type pin struct {
	key       PinKey
	target    route.TargetID
	expires   time.Time
	committed bool // a response has been committed under this pin
}

// NewPins returns a pin store holding at most maxEntries pins, each expiring
// ttl after its last use.
func NewPins(ttl time.Duration, maxEntries int) *Pins {
	return &Pins{ttl: ttl, max: maxEntries, now: time.Now, m: make(map[PinKey]*list.Element), lru: list.New()}
}

// prune drops expired pins. Every use moves a pin to the front and sets its
// expiry to now+ttl, so the list is also ordered by expiry and expired pins
// are always at the back.
func (p *Pins) prune(now time.Time) {
	for e := p.lru.Back(); e != nil && !now.Before(e.Value.(*pin).expires); e = p.lru.Back() {
		p.remove(e)
	}
}

func (p *Pins) remove(e *list.Element) {
	delete(p.m, e.Value.(*pin).key)
	p.lru.Remove(e)
}

// lookup returns the live pin for k, refreshed, or nil.
func (p *Pins) lookup(k PinKey, now time.Time) *pin {
	p.prune(now)
	e := p.m[k]
	if e == nil {
		return nil
	}
	pn := e.Value.(*pin)
	pn.expires = now.Add(p.ttl)
	p.lru.MoveToFront(e)
	return pn
}

// insert adds a pin for a key that has none, evicting the least recently
// used pin when the store is full.
func (p *Pins) insert(k PinKey, t route.TargetID, now time.Time) *pin {
	for p.lru.Len() > 0 && p.lru.Len() >= p.max {
		p.remove(p.lru.Back())
	}
	pn := &pin{key: k, target: t, expires: now.Add(p.ttl)}
	p.m[k] = p.lru.PushFront(pn)
	return pn
}

// Get returns the pinned target for k and refreshes its TTL.
func (p *Pins) Get(k PinKey) (route.TargetID, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pn := p.lookup(k, p.now()); pn != nil {
		return pn.target, true
	}
	return route.TargetID{}, false
}

// Reserve returns the existing pin for k if there is one (refreshing it);
// otherwise it pins k to t and returns t. reserved reports whether this call
// created the pin. Concurrent callers for the same key all get the same
// target, and exactly one of them gets reserved=true.
func (p *Pins) Reserve(k PinKey, t route.TargetID) (pinned route.TargetID, reserved bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if pn := p.lookup(k, now); pn != nil {
		return pn.target, false
	}
	p.insert(k, t, now)
	return t, true
}

// Commit records that a response from t was committed under k: the pin
// moves to t if it pointed elsewhere, is created again if it has expired
// meanwhile, and from now on survives Release (docs/affinity.md).
func (p *Pins) Commit(k PinKey, t route.TargetID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	pn := p.lookup(k, now)
	if pn == nil {
		pn = p.insert(k, t, now)
	}
	pn.target, pn.committed = t, true
}

// Release undoes a reservation after the request that made it failed
// entirely. It deletes the pin for k only if the pin still points at t and
// no response has been committed under it: another request may have moved
// the pin, or hit it and committed on t while the reserving request was
// still failing.
func (p *Pins) Release(k PinKey, t route.TargetID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.m[k]; e != nil {
		if pn := e.Value.(*pin); pn.target == t && !pn.committed {
			p.remove(e)
		}
	}
}

// Len returns the number of live pins.
func (p *Pins) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(p.now())
	return p.lru.Len()
}

// ByEndpoint counts live pins per endpoint, by config.EndpointName: the
// pinned provider under the pin's protocol.
func (p *Pins) ByEndpoint() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(p.now())
	out := make(map[string]int)
	for e := p.lru.Front(); e != nil; e = e.Next() {
		pn := e.Value.(*pin)
		out[config.EndpointName(pn.target.Provider, string(pn.key.Protocol))]++
	}
	return out
}
