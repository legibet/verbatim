package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"verbatim/internal/protocol"
)

// usageTee feeds a copy of the committed body to the protocol's usage
// parser. A parser panic drops the parser and never reaches the request.
//
// A gzip or deflate body is decompressed for the parser only (design 4.3).
// The stdlib decompressors pull from a reader while the tee is pushed to, so
// a decoder goroutine runs the decompressor and hands its output back; the
// parser is only ever called from the caller's goroutine. write returns once
// the decoder asks for the next chunk, so the parser has then seen everything
// the decompressor released from the body so far, as on the plain path.
type usageTee struct {
	p protocol.UsageParser

	// Set only for a compressed body.
	in   chan []byte   // write → decoder: compressed chunks
	out  chan []byte   // decoder → write: decompressed data, nil when the chunk is used up
	done chan struct{} // closed when the decoder has exited
}

// newUsageTee prepares the tee for a response with the given headers. A
// content encoding other than gzip and deflate is not inspected: the tee gets
// no parser and usage stays null.
func newUsageTee(p protocol.UsageParser, header http.Header) *usageTee {
	t := &usageTee{p: p}
	var open func(io.Reader) (io.Reader, error)
	switch strings.ToLower(strings.TrimSpace(strings.Join(header.Values("Content-Encoding"), ","))) {
	case "", "identity":
		return t
	case "gzip", "x-gzip":
		open = func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }
	case "deflate":
		// HTTP "deflate" is the zlib format (RFC 9110 section 8.4.1.2).
		open = func(r io.Reader) (io.Reader, error) { return zlib.NewReader(r) }
	default:
		t.p = nil
		return t
	}
	t.in, t.out, t.done = make(chan []byte), make(chan []byte), make(chan struct{})
	go t.decode(open)
	return t
}

// decode runs the decompressor until the body ends or turns out to be
// corrupt; the parser keeps whatever it was fed before.
func (t *usageTee) decode(open func(io.Reader) (io.Reader, error)) {
	defer close(t.done)
	r, err := open(&chunkReader{in: t.in, out: t.out})
	if err != nil {
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			t.out <- bytes.Clone(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// chunkReader is the decompressor's input: the chunks pushed by write. When
// one is used up it tells write, which then returns, before waiting for the
// next.
type chunkReader struct {
	in    <-chan []byte
	out   chan<- []byte
	chunk []byte
	owed  bool // write is waiting for the current chunk to be used up
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunk) == 0 {
		if r.owed {
			r.out <- nil
			r.owed = false
		}
		c, ok := <-r.in
		if !ok {
			return 0, io.EOF
		}
		r.chunk, r.owed = c, true
	}
	n := copy(p, r.chunk)
	r.chunk = r.chunk[n:]
	return n, nil
}

// write observes one chunk of the body. b is not retained after it returns.
func (t *usageTee) write(b []byte) {
	switch {
	case t.p == nil:
	case t.in == nil:
		t.parse(b)
	default:
		select {
		case t.in <- b:
			t.receive()
		case <-t.done:
		}
	}
}

// close ends a compressed body: the decompressor sees the end of its input,
// and what it still releases is parsed. Call it once, after the last write
// and before reading the results.
func (t *usageTee) close() {
	if t.in != nil {
		close(t.in)
		t.receive()
	}
}

// receive parses the decoder's output until it needs the next chunk or exits.
func (t *usageTee) receive() {
	for {
		select {
		case d := <-t.out:
			if d == nil {
				return
			}
			t.parse(d)
		case <-t.done:
			return
		}
	}
}

// catch is deferred around every parser call.
func (t *usageTee) catch() {
	if v := recover(); v != nil {
		slog.Warn("usage parser panicked; usage for this response is dropped", "panic", v)
		t.p = nil
	}
}

func (t *usageTee) parse(b []byte) {
	if t.p != nil {
		defer t.catch()
		t.p.Write(b)
	}
}

func (t *usageTee) responseID() string {
	if t.p == nil {
		return ""
	}
	defer t.catch()
	return t.p.ResponseID()
}

func (t *usageTee) errorEvent() bool {
	if t.p == nil {
		return false
	}
	defer t.catch()
	return t.p.ErrorEvent()
}

func (t *usageTee) usage() (protocol.Usage, bool) {
	if t.p == nil {
		return protocol.Usage{}, false
	}
	defer t.catch()
	return t.p.Usage()
}
