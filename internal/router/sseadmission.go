package router

import "bytes"

// Admission frames.
//
// A preempted request can be replayed while the client has read nothing that
// belongs to the message itself. llama-server satisfies that on its own: it
// sends SSE comments until its first token. An engine that speaks the Anthropic
// stream natively sends real events the moment it admits a request, long before
// the prompt is processed:
//
//	event: ping
//	event: message_start     (the message id and the input usage)
//	event: ping
//	...                      (minutes of prefill)
//	event: content_block_start
//
// Nothing before the first content_block_start is content. A client that has
// read only those frames holds the start of an empty message, and the replayed
// attempt can continue that message as long as its own message_start is not
// delivered a second time: Anthropic stream parsers reject two message_start
// events in one stream, and the second message id would name a message the
// client never started.
//
// admissionScan reads the upstream bytes of one attempt frame by frame to do
// two things:
//
//   - report the first frame that is not admission traffic, i.e. anything but a
//     comment, a ping or a message_start (the client now holds content, so a
//     cut ends the stream instead of being replayed);
//   - on a replayed attempt, withhold and discard that attempt's own
//     message_start frame, so the client sees one message_start per stream.
//
// It is byte-wise and keeps its state between calls, so the answer does not
// depend on how the proxy chunked the stream. It looks at the first line of a
// frame only (the event: line); an SSE frame whose first line is anything else
// counts as content, which is the safe side: content means "never replay".

// admLineCap bounds the event line the scanner keeps. The lines it cares about
// are "event: message_start" (20 bytes) and "event: ping"; a longer first line
// cannot be either.
const admLineCap = 64

type admState uint8

const (
	admFrameStart admState = iota // at the first byte of a line that may open a frame
	admComment                    // inside a comment line
	admEventLine                  // collecting the event: line that opens a frame
	admPassFrame                  // rest of a ping or message_start frame being forwarded
	admDropFrame                  // rest of a message_start frame being discarded
	admDone                       // a content frame has been seen; the scan is over
)

// admissionResult reports what one feed call saw.
type admissionResult struct {
	content  bool // a frame beyond admission traffic began
	msgStart bool // a message_start frame was let through
	dropped  bool // a message_start frame was discarded
}

type admissionScan struct {
	state admState
	// filter makes the scan withhold the first line of every frame until it is
	// known whether it opens a message_start, which is then discarded. Off, the
	// scan only observes and the bytes it is given are forwarded untouched.
	filter  bool
	n       int // bytes of the event line held in line
	lineLen int // non-CR bytes of the current line, to spot the blank line that ends a frame
	line    [admLineCap]byte
}

// reset starts the scan of a new upstream response. filter is true for a replay
// attempt whose client already holds a message_start.
func (s *admissionScan) reset(filter bool) {
	*s = admissionScan{filter: filter}
}

// feed consumes the next chunk of upstream bytes and returns the bytes to
// forward to the client. Without filtering that is p itself.
func (s *admissionScan) feed(p []byte) ([]byte, admissionResult) {
	var res admissionResult
	if s.state == admDone || len(p) == 0 {
		return p, res
	}
	var out []byte
	if s.filter {
		out = make([]byte, 0, len(p)+admLineCap)
	}
	for i := 0; i < len(p); i++ {
		b := p[i]
		switch s.state {
		case admFrameStart:
			switch b {
			case '\n', '\r':
				if s.filter {
					out = append(out, b)
				}
			case ':':
				s.state = admComment
				if s.filter {
					out = append(out, b)
				}
			case 'e':
				s.state = admEventLine
				s.line[0] = b
				s.n = 1
			default:
				return s.finish(p, out, i, res)
			}
		case admComment:
			if s.filter {
				out = append(out, b)
			}
			if b == '\n' {
				s.state = admFrameStart
			}
		case admEventLine:
			if b != '\n' {
				if s.n == admLineCap {
					return s.finish(p, out, i, res)
				}
				s.line[s.n] = b
				s.n++
				continue
			}
			switch eventName(s.line[:s.n]) {
			case "message_start":
				if s.filter {
					res.dropped = true
					s.state = admDropFrame
				} else {
					res.msgStart = true
					s.state = admPassFrame
				}
			case "ping":
				if s.filter {
					out = append(out, s.line[:s.n]...)
					out = append(out, b)
				}
				s.state = admPassFrame
			default:
				return s.finish(p, out, i, res)
			}
			s.n = 0
			s.lineLen = 0
		case admPassFrame, admDropFrame:
			if s.state == admPassFrame && s.filter {
				out = append(out, b)
			}
			if b == '\n' {
				if s.lineLen == 0 {
					s.state = admFrameStart
				} else {
					s.lineLen = 0
				}
			} else if b != '\r' {
				s.lineLen++
			}
		}
	}
	if !s.filter {
		return p, res
	}
	return out, res
}

// finish ends the scan at p[i], the byte that proved the stream carries
// content. Whatever was withheld goes out first, then the rest of the chunk
// exactly as it came.
func (s *admissionScan) finish(p, out []byte, i int, res admissionResult) ([]byte, admissionResult) {
	res.content = true
	held := s.state == admEventLine
	s.state = admDone
	if !s.filter {
		return p, res
	}
	if held {
		out = append(out, s.line[:s.n]...)
	}
	return append(out, p[i:]...), res
}

// eventName returns the value of an "event:" line ("" when the line is some
// other field).
func eventName(line []byte) string {
	line = bytes.TrimRight(line, "\r")
	const prefix = "event:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return ""
	}
	return string(bytes.TrimSpace(line[len(prefix):]))
}
