package router

import (
	"bytes"
	"math/rand"
	"testing"
)

// The admission scan (sseadmission.go) must give the same answer however the
// proxy happens to chunk the upstream stream, so every test below runs the same
// stream whole, split at every offset, and one byte at a time.

const (
	frComment = ": keepalive\n\n"
	frPing    = "event: ping\ndata: {\"type\": \"ping\"}\n\n"
	frStartA  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_A\",\"usage\":{\"input_tokens\":10}}}\n\n"
	frStartB  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_B\",\"usage\":{\"input_tokens\":10}}}\n\n"
	frBlock   = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n"
	frDelta   = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"text\":\"hello\"}}\n\n"
)

// scanAll feeds chunks to s and returns everything it said to forward, with the
// flags OR-ed over the whole stream.
func scanAll(s *admissionScan, chunks ...string) (string, admissionResult) {
	var out bytes.Buffer
	var all admissionResult
	for _, c := range chunks {
		b, res := s.feed([]byte(c))
		out.Write(b)
		all.content = all.content || res.content
		all.msgStart = all.msgStart || res.msgStart
		all.dropped = all.dropped || res.dropped
	}
	return out.String(), all
}

// chunkings returns the stream whole, split in two at every offset, and one
// byte at a time.
func chunkings(stream string) [][]string {
	out := [][]string{{stream}}
	for i := 1; i < len(stream); i++ {
		out = append(out, []string{stream[:i], stream[i:]})
	}
	bytewise := make([]string, len(stream))
	for i := range stream {
		bytewise[i] = stream[i : i+1]
	}
	return append(out, bytewise)
}

func TestAdmissionScan_Classifies(t *testing.T) {
	longLine := "event: " + string(bytes.Repeat([]byte("x"), 80)) + "\ndata: {}\n\n"
	cases := []struct {
		name     string
		stream   string
		content  bool
		msgStart bool
	}{
		{name: "comment keepalive", stream: ":\n\n" + frComment},
		{name: "ping", stream: frPing},
		{name: "pings and comments", stream: frComment + frPing + frPing},
		{name: "message_start", stream: frStartA, msgStart: true},
		{name: "ping message_start ping", stream: frPing + frStartA + frPing, msgStart: true},
		{name: "stray blank lines first", stream: "\n\n" + frPing},
		{name: "message_start then content", stream: frStartA + frBlock, content: true, msgStart: true},
		{name: "content alone", stream: frBlock, content: true},
		{name: "delta", stream: frPing + frStartA + frPing + frBlock + frDelta, content: true, msgStart: true},
		{name: "error event", stream: "event: error\ndata: {}\n\n", content: true},
		{name: "data only frame", stream: "data: {\"x\":1}\n\n", content: true},
		{name: "json body without newline", stream: "{\"type\":\"error\"}", content: true},
		{name: "event line longer than the cap", stream: longLine, content: true},
		{name: "crlf ping", stream: "event: ping\r\ndata: {}\r\n\r\n"},
		{name: "crlf message_start then content", stream: "event: message_start\r\ndata: {}\r\n\r\nevent: content_block_start\r\ndata: {}\r\n\r\n", content: true, msgStart: true},
		{name: "event name without a space", stream: "event:ping\ndata: {}\n\n"},
		{name: "unknown field first", stream: "id: 7\nevent: ping\ndata: {}\n\n", content: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, chunks := range chunkings(c.stream) {
				var s admissionScan
				s.reset(false)
				out, res := scanAll(&s, chunks...)
				if res.content != c.content || res.msgStart != c.msgStart || res.dropped {
					t.Fatalf("chunks %q: got content=%v msgStart=%v dropped=%v, want content=%v msgStart=%v",
						chunks, res.content, res.msgStart, res.dropped, c.content, c.msgStart)
				}
				if out != c.stream {
					t.Fatalf("chunks %q: an observing scan changed the bytes:\n got %q\nwant %q", chunks, out, c.stream)
				}
			}
		})
	}
}

// Without filtering the chunk is handed back as it came, not copied.
func TestAdmissionScan_ObservingReturnsTheCallersSlice(t *testing.T) {
	var s admissionScan
	s.reset(false)
	p := []byte(frPing + frStartA)
	out, _ := s.feed(p)
	if len(out) != len(p) || &out[0] != &p[0] {
		t.Fatal("an observing scan must return the chunk it was given")
	}
}

func TestAdmissionScan_FilterDropsTheReplayedMessageStart(t *testing.T) {
	// What the replayed attempt's upstream sends, and what the client, which
	// already holds msg_A, must be sent of it.
	stream := frPing + frStartB + frPing + frBlock + frDelta
	want := frPing + frPing + frBlock + frDelta
	for _, chunks := range chunkings(stream) {
		var s admissionScan
		s.reset(true)
		out, res := scanAll(&s, chunks...)
		if out != want {
			t.Fatalf("chunks %q:\n got %q\nwant %q", chunks, out, want)
		}
		if !res.dropped || !res.content || res.msgStart {
			t.Fatalf("chunks %q: dropped=%v content=%v msgStart=%v, want dropped, content, not let through", chunks, res.dropped, res.content, res.msgStart)
		}
	}
}

// A filtering scan is the identity on every stream that has no message_start.
func TestAdmissionScan_FilterLeavesOtherStreamsAlone(t *testing.T) {
	for name, stream := range map[string]string{
		"keepalives":      ":\n\n" + frComment + frPing,
		"content first":   frBlock + frDelta,
		"ping then delta": frPing + frPing + frBlock + frDelta,
		"json error body": "{\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}",
		"crlf":            "event: ping\r\ndata: {}\r\n\r\nevent: content_block_start\r\ndata: {}\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			for _, chunks := range chunkings(stream) {
				var s admissionScan
				s.reset(true)
				out, res := scanAll(&s, chunks...)
				if out != stream || res.dropped {
					t.Fatalf("chunks %q: a filtering scan changed a stream without message_start:\n got %q\nwant %q", chunks, out, stream)
				}
			}
		})
	}
}

// The scan withholds only the first line of a frame that might be a
// message_start, and only until it knows.
func TestAdmissionScan_FilterWithholdsOnlyWhatItCannotYetClassify(t *testing.T) {
	var s admissionScan
	s.reset(true)

	if out, _ := s.feed([]byte("event: message_st")); len(out) != 0 {
		t.Fatalf("a half-read event line was forwarded: %q", out)
	}
	if out, res := s.feed([]byte("art\ndata: {\"id\":\"msg_B\"}\n\n")); len(out) != 0 || !res.dropped {
		t.Fatalf("the message_start was not dropped: out=%q dropped=%v", out, res.dropped)
	}
	if out, _ := s.feed([]byte(frPing)); string(out) != frPing {
		t.Fatalf("the frame after a dropped message_start was not forwarded whole: %q", out)
	}

	// A ping is released as soon as its first line is complete, with the held
	// bytes in front of it.
	s.reset(true)
	if out, _ := s.feed([]byte("event: pi")); len(out) != 0 {
		t.Fatalf("a half-read event line was forwarded: %q", out)
	}
	if out, _ := s.feed([]byte("ng\ndata: {}\n\n")); string(out) != "event: ping\ndata: {}\n\n" {
		t.Fatalf("held bytes were not released in front of the rest: %q", out)
	}

	// A content frame releases what was held and ends the scan.
	s.reset(true)
	if out, _ := s.feed([]byte("event: content_block_start")); len(out) != 0 {
		t.Fatalf("a half-read event line was forwarded: %q", out)
	}
	out, res := s.feed([]byte("\ndata: {}\n\n"))
	if string(out) != "event: content_block_start\ndata: {}\n\n" || !res.content {
		t.Fatalf("content frame: out=%q content=%v", out, res.content)
	}
	// Once past admission nothing is looked at again, not even a message_start.
	if out, res := s.feed([]byte(frStartB)); string(out) != frStartB || res.dropped {
		t.Fatalf("after content the scan must stay out of the way: out=%q dropped=%v", out, res.dropped)
	}
}

// A new attempt starts clean: whatever the cut attempt left half-read, held or
// half-dropped is gone.
func TestAdmissionScan_ResetForgetsTheCutAttempt(t *testing.T) {
	for name, partial := range map[string]string{
		"held first line":      "event: mess",
		"half a dropped frame": "event: message_start\ndata: {\"id\":",
		"half a ping":          "event: ping\ndata: {",
		"half a comment":       ": keepa",
	} {
		t.Run(name, func(t *testing.T) {
			var s admissionScan
			s.reset(true)
			s.feed([]byte(partial))
			s.reset(true)
			if out, res := s.feed([]byte(frPing + frStartB + frPing)); string(out) != frPing+frPing || !res.dropped {
				t.Fatalf("scan carried state over a reset: out=%q dropped=%v", out, res.dropped)
			}
		})
	}
}

// The same answer for arbitrary chunking: random cuts of a long stream, many
// times, filtering and not.
func TestAdmissionScan_RandomChunkingGivesTheSameAnswer(t *testing.T) {
	stream := frComment + frPing + frStartB + frPing + frComment + frBlock + frDelta + frDelta
	rng := rand.New(rand.NewSource(7))
	for _, filter := range []bool{false, true} {
		var ref string
		for i := 0; i < 400; i++ {
			var chunks []string
			for rest := stream; rest != ""; {
				n := 1 + rng.Intn(len(rest))
				if rng.Intn(3) == 0 {
					n = 1 + rng.Intn(6)
				}
				if n > len(rest) {
					n = len(rest)
				}
				chunks = append(chunks, rest[:n])
				rest = rest[n:]
			}
			var s admissionScan
			s.reset(filter)
			out, res := scanAll(&s, chunks...)
			if !res.content {
				t.Fatalf("filter=%v chunks %q: content frame not seen", filter, chunks)
			}
			if i == 0 {
				ref = out
				continue
			}
			if out != ref {
				t.Fatalf("filter=%v chunks %q: output depends on the chunking:\n got %q\nwant %q", filter, chunks, out, ref)
			}
		}
		if !filter && ref != stream {
			t.Fatalf("observing scan changed the stream: %q", ref)
		}
	}
}
