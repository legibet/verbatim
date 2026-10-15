package protocol

import "github.com/tidwall/gjson"

// maxBodyBytes bounds a non-streamed response body kept for usage parsing;
// a larger body yields no usage.
const maxBodyBytes = 8 << 20

// usageState is what a protocol extracts from the objects of a response body.
type usageState struct {
	usage      Usage
	ok         bool
	id         string
	errorEvent bool
}

// set records u, when it is an object, as the current usage object.
func (st *usageState) set(u gjson.Result, normalize func(gjson.Result) Usage) {
	if !u.IsObject() {
		return
	}
	st.usage = normalize(u)
	st.usage.Raw = []byte(u.Raw)
	st.ok = true
}

// observeFunc folds one JSON object of a response into st. For a stream it is
// called once per event with the SSE event name; for a whole body it is called
// once with an empty event name.
type observeFunc func(st *usageState, event string, obj gjson.Result)

func newUsageParser(stream bool, observe observeFunc) UsageParser {
	if stream {
		return &streamParser{observe: observe}
	}
	return &bodyParser{observe: observe}
}

// streamParser parses each server-sent event as it completes.
type streamParser struct {
	sse     sseSplitter
	observe observeFunc
	st      usageState
}

func (p *streamParser) Write(b []byte) { p.sse.write(b, p.event) }

func (p *streamParser) event(name string, data []byte) {
	if gjson.ValidBytes(data) {
		p.observe(&p.st, name, gjson.ParseBytes(data))
	}
}

func (p *streamParser) Usage() (Usage, bool) { return p.st.usage, p.st.ok }
func (p *streamParser) ResponseID() string   { return p.st.id }
func (p *streamParser) ErrorEvent() bool     { return p.st.errorEvent }

// bodyParser accumulates a whole response body and parses it when asked.
type bodyParser struct {
	buf      []byte
	overflow bool
	observe  observeFunc
	st       *usageState // parsed result; nil until asked and after every Write
}

func (p *bodyParser) Write(b []byte) {
	if p.overflow {
		return
	}
	p.st = nil
	if len(p.buf)+len(b) > maxBodyBytes {
		p.overflow, p.buf = true, nil
		return
	}
	p.buf = append(p.buf, b...)
}

func (p *bodyParser) state() *usageState {
	if p.st == nil {
		p.st = &usageState{}
		if !p.overflow && gjson.ValidBytes(p.buf) {
			p.observe(p.st, "", gjson.ParseBytes(p.buf))
		}
	}
	return p.st
}

func (p *bodyParser) Usage() (Usage, bool) { st := p.state(); return st.usage, st.ok }
func (p *bodyParser) ResponseID() string   { return p.state().id }

// ErrorEvent is always false for a whole body: its status line already told.
func (p *bodyParser) ErrorEvent() bool { return false }

// num returns a JSON number as an int64, or nil when r is not a number.
func num(r gjson.Result) *int64 {
	if r.Type != gjson.Number {
		return nil
	}
	return new(r.Int())
}
