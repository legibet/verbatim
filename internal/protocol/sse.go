package protocol

import "bytes"

// maxEventBytes bounds one server-sent event; a larger event is skipped.
const maxEventBytes = 1 << 20

// sseSplitter cuts a server-sent event stream into events, keeping only the
// event being read in memory. Lines end in "\n" or "\r\n"; a blank line ends
// an event. Input may be split at any byte.
type sseSplitter struct {
	line     []byte // the current, unterminated line
	event    string
	data     []byte
	hasData  bool
	size     int  // bytes of the current event so far
	oversize bool // the current event exceeded maxEventBytes and is skipped
	done     bool // "data: [DONE]" was seen; nothing after it is read
}

// write feeds p and calls emit with the event name and the data (data lines
// joined with "\n") of every event p completes.
func (s *sseSplitter) write(p []byte, emit func(event string, data []byte)) {
	for len(p) > 0 && !s.done {
		line, rest, complete := bytes.Cut(p, []byte{'\n'})
		s.appendLine(line)
		if !complete {
			return
		}
		p = rest
		s.endLine(emit)
	}
}

func (s *sseSplitter) appendLine(b []byte) {
	s.size += len(b)
	if s.size > maxEventBytes && !s.oversize {
		s.oversize = true
		s.data, s.hasData = nil, false
		s.line = s.line[:min(len(s.line), 2)]
	}
	if s.oversize {
		// Only whether the line is blank still matters, and its first two
		// bytes tell ("" or "\r").
		b = b[:min(len(b), max(0, 2-len(s.line)))]
	}
	s.line = append(s.line, b...)
}

func (s *sseSplitter) endLine(emit func(event string, data []byte)) {
	line := bytes.TrimSuffix(s.line, []byte{'\r'})
	switch {
	case len(line) == 0:
		s.dispatch(emit)
	case !s.oversize:
		name, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(name) {
		case "event":
			s.event = string(value)
		case "data":
			if s.hasData {
				s.data = append(s.data, '\n')
			}
			s.data = append(s.data, value...)
			s.hasData = true
		}
	}
	s.line = s.line[:0]
}

func (s *sseSplitter) dispatch(emit func(event string, data []byte)) {
	if s.hasData && !s.oversize {
		if string(s.data) == "[DONE]" {
			s.done = true
		} else {
			emit(s.event, s.data)
		}
	}
	s.event = ""
	s.data, s.hasData = s.data[:0], false
	s.size, s.oversize = 0, false
}
