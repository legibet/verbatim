package protocol

import "github.com/tidwall/gjson"

// maxBodyBytes bounds a non-streamed response body kept for usage parsing;
// a larger body yields no usage. Long strings are dropped before the bound
// applies (stringElider), so only a body with very many fields reaches it.
const maxBodyBytes = 8 << 20

// maxStringBytes is the longest JSON string content the usage parsers keep.
// Ids, types and model names are far shorter; what is longer (base64 images,
// generated text) is never needed to read usage.
const maxStringBytes = 4 << 10

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

// stringElider drops the content of JSON strings longer than maxStringBytes
// as bytes pass through, leaving "" in their place, so that a response
// carrying images parses in bounded memory. A raw newline cannot occur in a
// JSON string, so a newline ends a string: this keeps the filter correct on
// the lines of a server-sent event stream, around the JSON in data fields.
type stringElider struct {
	in      bool   // inside a string
	escaped bool   // the previous byte was an unescaped backslash
	long    bool   // the current string exceeded maxStringBytes
	content []byte // the current string's content, while short enough
}

// filter appends the filtered form of src to dst.
func (e *stringElider) filter(dst, src []byte) []byte {
	for _, c := range src {
		if !e.in {
			dst = append(dst, c)
			if c == '"' {
				e.in, e.escaped, e.long, e.content = true, false, false, e.content[:0]
			}
			continue
		}
		if c == '\n' || c == '"' && !e.escaped {
			if !e.long {
				dst = append(dst, e.content...)
			}
			dst = append(dst, c)
			e.in = false
			continue
		}
		e.escaped = c == '\\' && !e.escaped
		if e.long {
			continue
		}
		if len(e.content) == maxStringBytes {
			e.long, e.content = true, e.content[:0]
			continue
		}
		e.content = append(e.content, c)
	}
	return dst
}

// streamParser parses each server-sent event as it completes.
type streamParser struct {
	elider  stringElider
	buf     []byte // the filtered form of the latest write
	sse     sseSplitter
	observe observeFunc
	st      usageState
}

func (p *streamParser) Write(b []byte) {
	p.buf = p.elider.filter(p.buf[:0], b)
	p.sse.write(p.buf, p.event)
}

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
	elider   stringElider
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
	if p.buf = p.elider.filter(p.buf, b); len(p.buf) > maxBodyBytes {
		p.overflow, p.buf = true, nil
	}
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
