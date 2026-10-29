package router

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// Replay of a stream that holds only admission traffic.
//
// An engine that speaks the Anthropic stream natively sends ping and
// message_start when it admits a request, minutes before its first token (see
// sseadmission.go). A client that has read only those frames holds an empty
// message, so a preemption in that window is hidden by a transparent replay: the
// replayed attempt's own message_start is dropped, the cancel's terminal error
// event never reaches the client, and the stream goes on. Once the first content
// frame has reached the client the stream is a live one, a priority arrival
// keeps its right to abort it, and the client is told so with the terminal error
// event.

func msgStartFrame(id string) string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"" + id + "\",\"usage\":{\"input_tokens\":12}}}\n\n"
}

func deltaFrame(text string) string {
	return "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n"
}

const (
	blockStartFrame = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	blockStopFrame  = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	msgDeltaFrame   = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"
	msgStopFrame    = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	// proxyAbortFrame is what the process proxy writes into a stream whose
	// upstream request was cancelled after the headers went out: it recovers the
	// http.ErrAbortHandler panic of httputil.ReverseProxy and ends an SSE
	// response with a terminal error event (internal/process, handlerFn). A
	// booted request produces it every time, so a replay has to keep it away
	// from the client.
	proxyAbortFrame = "\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"llama-swap: upstream disconnected mid-stream\"}}\n\n"
)

// dropFrame removes one occurrence of frame from s.
func dropFrame(s, frame string) string { return strings.Replace(s, frame, "", 1) }

// ---- the pingWriter ----------------------------------------------------------

func newAdmissionPW(t *testing.T) (*pingWriter, *syncRecorder) {
	t.Helper()
	rec := newSyncRecorder()
	pw := newPingWriter(logmon.NewWriter(io.Discard), "alpha", rec, true, nil)
	t.Cleanup(func() { pw.stop(); pw.waitLoop() })
	return pw, rec
}

func TestPingWriter_AdmissionFramesAreReplaySafe(t *testing.T) {
	pw, _ := newAdmissionPW(t)
	pw.WriteHeader(http.StatusOK)

	mustWrite(t, pw, frComment+frPing+msgStartFrame("msg_A")+frPing)
	if !pw.replaySafe() {
		t.Fatal("ping, message_start and ping are admission traffic: a cut must be replayable")
	}
	if pw.preFirstToken() {
		t.Fatal("a message_start is still a token for the prefill-stall verdict (llama-server sends it with the first one)")
	}

	mustWrite(t, pw, blockStartFrame)
	if pw.replaySafe() {
		t.Fatal("the first content block reached the client: a cut must not be replayed")
	}
}

// A cut that lands inside a frame would leave the client half a frame, and the
// replayed attempt's bytes would finish it with something else.
func TestPingWriter_MidFrameIsNotReplaySafe(t *testing.T) {
	pw, _ := newAdmissionPW(t)
	pw.WriteHeader(http.StatusOK)

	mustWrite(t, pw, frPing+"event: ping\ndata: {\"ty")
	if pw.replaySafe() {
		t.Fatal("the client holds half a frame: a replay would corrupt it")
	}
	mustWrite(t, pw, "pe\": \"ping\"}\n\n")
	if !pw.replaySafe() {
		t.Fatal("the frame is complete again: the stream is back on a boundary")
	}
}

func mustWrite(t *testing.T, w io.Writer, s string) {
	t.Helper()
	n, err := w.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write(%d bytes) = %d, %v: every byte must be reported consumed, withheld or not", len(s), n, err)
	}
}

// The client holds msg_A from the first attempt. Whatever the proxy's chunking
// of the replayed attempt, the client must read exactly the first attempt's
// frames followed by the replayed attempt's, minus its message_start.
func TestPingWriter_ReplayedAttemptContinuesTheMessage(t *testing.T) {
	attempt1 := frPing + msgStartFrame("msg_A") + frPing
	attempt2 := frPing + msgStartFrame("msg_B") + frPing + blockStartFrame + deltaFrame("hello") + blockStopFrame + msgDeltaFrame + msgStopFrame
	want := attempt1 + dropFrame(attempt2, msgStartFrame("msg_B"))

	for _, chunks := range chunkings(attempt2) {
		pw, rec := newAdmissionPW(t)
		pw.WriteHeader(http.StatusOK)
		mustWrite(t, pw, attempt1)

		pw.WriteHeader(http.StatusOK) // the replayed attempt's response
		for _, c := range chunks {
			mustWrite(t, pw, c)
		}
		if got := rec.bodyString(); got != want {
			t.Fatalf("chunks %q:\n got %q\nwant %q", chunks, got, want)
		}
		if n := strings.Count(rec.bodyString(), "event: message_start"); n != 1 {
			t.Fatalf("chunks %q: the client read %d message_start events, want 1", chunks, n)
		}
	}
}

// An attempt that is cut while a frame is half read must leave nothing behind.
// What the scan withheld (a first line it could not classify yet, a
// message_start it was dropping) is never sent, and the replay starts on a fresh
// frame. A half frame that WAS forwarded (a ping or a comment cannot be told
// from the rest of itself) leaves the stream off an event boundary, which is
// what keeps base.go from replaying it.
func TestPingWriter_HalfReadFramesNeverReachAReplay(t *testing.T) {
	for name, partial := range map[string]string{
		"held first line":   "event: mess",
		"half a message":    "event: message_start\ndata: {\"id\":",
		"half a ping":       "event: ping\ndata: {",
		"half a comment":    ": keepa",
		"held content line": "event: content_block",
	} {
		t.Run(name, func(t *testing.T) {
			pw, rec := newAdmissionPW(t)
			pw.WriteHeader(http.StatusOK)
			mustWrite(t, pw, frPing+msgStartFrame("msg_A")+frPing)
			before := rec.bodyString()

			pw.WriteHeader(http.StatusOK) // attempt 2, cut mid-frame
			mustWrite(t, pw, partial)
			sentOfPartial := strings.TrimPrefix(rec.bodyString(), before)
			// The client is only ever sent what the scan could classify; the
			// replay check is what keeps a half frame that WAS forwarded from
			// being completed by someone else's bytes.
			if sentOfPartial != "" && pw.replaySafe() {
				t.Fatalf("a half frame (%q) reached the client yet the stream reads as replay-safe", sentOfPartial)
			}
			if !pw.replaySafe() {
				return // it was forwarded in part: base.go will not replay it, nothing more to prove
			}

			pw.WriteHeader(http.StatusOK) // attempt 3, the replay
			mustWrite(t, pw, frPing+msgStartFrame("msg_C")+blockStartFrame)
			got := strings.TrimPrefix(rec.bodyString(), before)
			if got != frPing+blockStartFrame {
				t.Fatalf("the replay did not start on a clean frame: %q", got)
			}
		})
	}
}

// ---- the preemptResponseWriter -------------------------------------------------

func TestPreemptResponseWriter_DiscardsTheCancelOfAReplayableAttempt(t *testing.T) {
	cases := []struct {
		name        string
		frames      string
		replayable  bool // replayWanted non-nil, as for an eligible attempt
		wantAbort   bool // the terminal event reaches the client
		preemptedAt bool
	}{
		{name: "admission traffic only", frames: frPing + msgStartFrame("msg_A") + frPing, replayable: true, preemptedAt: true, wantAbort: false},
		{name: "content already sent", frames: frPing + msgStartFrame("msg_A") + blockStartFrame + deltaFrame("par"), replayable: true, preemptedAt: true, wantAbort: true},
		{name: "replay not eligible", frames: frPing + msgStartFrame("msg_A") + frPing, replayable: false, preemptedAt: true, wantAbort: true},
		{name: "not preempted", frames: frPing + msgStartFrame("msg_A") + frPing, replayable: true, preemptedAt: false, wantAbort: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pw, rec := newAdmissionPW(t)
			preempted := new(atomic.Bool)
			var replayWanted *atomic.Bool
			if c.replayable {
				replayWanted = new(atomic.Bool)
			}
			w := newPreemptResponseWriter(pw, preempted, replayWanted)
			w.WriteHeader(http.StatusOK)
			mustWrite(t, w, c.frames)
			if c.preemptedAt {
				preempted.Store(true)
			}
			mustWrite(t, w, proxyAbortFrame) // the proxy's recovered abort, after the cancel

			got := rec.bodyString()
			if sent := strings.Contains(got, "upstream disconnected mid-stream"); sent != c.wantAbort {
				t.Fatalf("terminal event reached the client = %v, want %v\nbody: %q", sent, c.wantAbort, got)
			}
			if !c.wantAbort && got != c.frames {
				t.Fatalf("the client's bytes changed:\n got %q\nwant %q", got, c.frames)
			}
		})
	}
}

// Without a gate (a writer that cannot tell, i.e. not the Anthropic stream
// path) nothing new is discarded: a booted request's writes reach the client as
// they did before the gate existed.
func TestPreemptResponseWriter_WithoutAGateForwardsTheCancelsWrites(t *testing.T) {
	rec := newSyncRecorder()
	preempted := new(atomic.Bool)
	w := newPreemptResponseWriter(rec, preempted, new(atomic.Bool))
	w.WriteHeader(http.StatusOK)
	mustWrite(t, w, frPing)
	preempted.Store(true)
	mustWrite(t, w, proxyAbortFrame)
	if got, want := rec.bodyString(), frPing+proxyAbortFrame; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// ---- through ServeHTTP -------------------------------------------------------

type replayScenario struct {
	b          *baseRouter
	proc       *fakeProcess
	logs       *syncBuf
	victim     *syncRecorder
	rival      *syncRecorder
	victimRuns atomic.Int32
	rivalRuns  atomic.Int32
	// afterContent: the first attempt streams its first content block before it
	// waits to be booted.
	afterContent bool
}

func newReplayScenario(t *testing.T, afterContent bool) *replayScenario {
	t.Helper()
	shortenPingCadence(t, time.Hour) // only upstream bytes count
	s := &replayScenario{
		proc:         newFakeProcess("alpha"),
		logs:         &syncBuf{},
		victim:       newSyncRecorder(),
		rival:        newSyncRecorder(),
		afterContent: afterContent,
	}
	s.proc.markReady()
	s.proc.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		switch r.Header.Get("X-Role") {
		case "victim":
			attempt := s.victimRuns.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// The engine admits the request: ping, message_start, ping, then
			// minutes of prefill. Each attempt is a new message, so each has
			// its own id.
			fmt.Fprint(w, frPing+msgStartFrame(map[int32]string{1: "msg_A", 2: "msg_B"}[attempt])+frPing)
			flush()
			if attempt == 1 {
				if s.afterContent {
					fmt.Fprint(w, blockStartFrame+deltaFrame("par"))
					flush()
				}
				// Wait to be booted, then do what the proxy does to a
				// cancelled stream.
				<-r.Context().Done()
				fmt.Fprint(w, proxyAbortFrame)
				flush()
				return true
			}
			fmt.Fprint(w, blockStartFrame+deltaFrame("hello")+blockStopFrame+msgDeltaFrame+msgStopFrame)
			flush()
			return true
		case "rival":
			s.rivalRuns.Add(1)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "rival-ok")
			return true
		}
		return false
	}
	sendLoading := true
	mc := config.ModelConfig{ConcurrencyLimit: 1, SendLoadingState: &sendLoading}
	s.b = newInFlightTestBase(t, s.proc, mc, config.PrefillStallConfig{}, s.logs)
	return s
}

// run starts the victim, waits until the client has read the victim's stream up
// to until, then sends the priority rival and waits for both to finish.
func (s *replayScenario) run(t *testing.T, until string) {
	t.Helper()
	victimDone, rivalDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(victimDone)
		s.b.ServeHTTP(s.victim, buildParkReq("alpha", "victim", replayTierDefault,
			"/v1/messages", `{"model":"alpha","stream":true}`))
	}()
	waitForBody(t, s.victim, until)
	go func() {
		defer close(rivalDone)
		s.b.ServeHTTP(s.rival, buildParkReq("alpha", "rival", tierPriorityRank10,
			"/v1/chat/completions", `{"model":"alpha"}`))
	}()
	awaitDone(t, "victim", victimDone, s.logs)
	awaitDone(t, "rival", rivalDone, s.logs)
	if s.rival.statusCode() != http.StatusOK || s.rival.bodyString() != "rival-ok" {
		t.Fatalf("rival status=%d body=%q, want 200 rival-ok", s.rival.statusCode(), s.rival.bodyString())
	}
}

func waitForBody(t *testing.T, rec *syncRecorder, suffix string) {
	t.Helper()
	waitFor(t, "the client to read "+fmt.Sprintf("%.40q", suffix), func() bool {
		return strings.HasSuffix(rec.bodyString(), suffix)
	})
}

// The unit test the replay path is held to: a fake upstream that emits ping,
// message_start, ping and then content, booted by a priority arrival before the
// first content delta (replayed, the client never sees the cut) and after it
// (aborted, as ruled, with the terminal error event).
func TestBaseRouter_AdmissionFrames_ReplayedBeforeContentAbortedAfter(t *testing.T) {
	attempt1 := frPing + msgStartFrame("msg_A") + frPing
	attempt2 := frPing + msgStartFrame("msg_B") + frPing + blockStartFrame + deltaFrame("hello") + blockStopFrame + msgDeltaFrame + msgStopFrame

	t.Run("booted before the first content delta: replayed", func(t *testing.T) {
		s := newReplayScenario(t, false)
		s.run(t, attempt1)

		// One message the client can read start to end: the first attempt's
		// admission frames, then the replayed attempt without its message_start.
		want := attempt1 + dropFrame(attempt2, msgStartFrame("msg_B"))
		if got := s.victim.bodyString(); got != want {
			t.Fatalf("victim stream:\n got %q\nwant %q", got, want)
		}
		if got := s.victimRuns.Load(); got != 2 {
			t.Fatalf("the model was entered %d times for the victim, want 2 (the cut attempt and its replay)", got)
		}
		if !strings.Contains(s.logs.String(), "preempt-replay: tier=default model=alpha attempt=1") {
			t.Fatalf("no replay line in the log:\n%s", s.logs.String())
		}
		if strings.Contains(s.logs.String(), "suppressed") {
			t.Fatalf("the replay was suppressed:\n%s", s.logs.String())
		}
	})

	t.Run("booted after the first content delta: aborted", func(t *testing.T) {
		s := newReplayScenario(t, true)
		s.run(t, attempt1+blockStartFrame+deltaFrame("par"))

		// The priority arrival keeps its right to abort a live stream: the
		// victim is not replayed and its client is told the stream ended.
		want := attempt1 + blockStartFrame + deltaFrame("par") + proxyAbortFrame
		if got := s.victim.bodyString(); got != want {
			t.Fatalf("victim stream:\n got %q\nwant %q", got, want)
		}
		if got := s.victimRuns.Load(); got != 1 {
			t.Fatalf("the model was entered %d times for the victim, want exactly 1: a stream that carried content is never replayed", got)
		}
		if strings.Contains(s.logs.String(), "preempt-replay: tier=default model=alpha attempt=1 held") {
			t.Fatalf("a replay line was logged for a stream that was aborted:\n%s", s.logs.String())
		}
	})
}
