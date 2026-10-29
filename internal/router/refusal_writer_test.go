package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The pieces of the refusal hold (refusalhold.go), one at a time. The scenario
// they serve is in refusal_hold_test.go.

// What the reverse proxy does with an upstream answer: copy its headers into the
// client-facing map, write the status, then the body.
func writeUpstreamAnswer(w http.ResponseWriter, code int, headers map[string]string, body string) {
	for k, v := range headers {
		w.Header().Add(k, v)
	}
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestRefusalWriter_WithholdsARetryableRefusal(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, 529} {
		rec := httptest.NewRecorder()
		rec.Header().Set("X-Request-Id", "abc") // set by a layer before the attempt
		rw := newRefusalWriter(rec)

		writeUpstreamAnswer(rw, code, map[string]string{
			"Content-Type":   "application/json",
			"Content-Length": "11",
			"Retry-After":    "3",
		}, "busy-answer")

		if !rw.refused() {
			t.Fatalf("%d: not reported as refused", code)
		}
		if rw.retryAfter != 3*time.Second || rw.status != code {
			t.Fatalf("%d: retryAfter=%v status=%d, want 3s and the status", code, rw.retryAfter, rw.status)
		}
		if rec.Body.Len() != 0 || rec.Flushed {
			t.Fatalf("%d: the refusal reached the client: body=%q flushed=%v", code, rec.Body.String(), rec.Flushed)
		}
		// The headers the refusal brought are gone, the ones that were there stay:
		// the answer that finally succeeds must not inherit a Content-Length that
		// belongs to the refusal.
		if got := rec.Header(); len(got) != 1 || got.Get("X-Request-Id") != "abc" {
			t.Fatalf("%d: the client-facing headers are %v, want only the one set before the attempt", code, got)
		}
	}
}

func TestRefusalWriter_PassesEverythingElseThrough(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		rec := httptest.NewRecorder()
		rw := newRefusalWriter(rec)

		writeUpstreamAnswer(rw, code, map[string]string{"Content-Type": "text/plain"}, "answer")

		if rw.refused() {
			t.Fatalf("%d: reported as refused", code)
		}
		if rec.Code != code || rec.Body.String() != "answer" || rec.Header().Get("Content-Type") != "text/plain" {
			t.Fatalf("%d: code=%d body=%q headers=%v, want it all through untouched", code, rec.Code, rec.Body.String(), rec.Header())
		}
	}
}

// An interim status (a 103, say) is not the attempt's answer.
func TestRefusalWriter_InterimStatusDoesNotDecide(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newRefusalWriter(rec)
	rw.WriteHeader(http.StatusEarlyHints)
	if rw.refused() || rw.decided {
		t.Fatal("an interim status decided the attempt")
	}
	rw.WriteHeader(http.StatusServiceUnavailable)
	if !rw.refused() {
		t.Fatal("the refusal after the interim status was not withheld")
	}
}

// Once a body byte went out as an implicit 200 the attempt has an answer, and a
// later status line cannot take it back.
func TestRefusalWriter_AnImplicitOKIsAnAnswer(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newRefusalWriter(rec)
	_, _ = rw.Write([]byte("data"))
	rw.WriteHeader(http.StatusServiceUnavailable)
	if rw.refused() {
		t.Fatal("a refusal was claimed after the attempt had already written its answer")
	}
}

func TestRefusalWriter_ReadFromDiscardsARefusalBody(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newRefusalWriter(rec)
	rw.WriteHeader(http.StatusServiceUnavailable)
	n, err := rw.ReadFrom(strings.NewReader("busy-answer"))
	if err != nil || n != int64(len("busy-answer")) {
		t.Fatalf("ReadFrom = %d, %v, want the body consumed", n, err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("the refusal body reached the client through ReadFrom: %q", rec.Body.String())
	}
}

func TestParseRetryAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":                              0,
		"1":                             time.Second,
		" 7 ":                           7 * time.Second,
		"0":                             0,
		"-3":                            0,
		"soon":                          0,
		"Wed, 21 Oct 2026 07:28:00 GMT": 0, // an HTTP date: use the default cadence
	} {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRefusalWait(t *testing.T) {
	for _, c := range []struct{ in, want time.Duration }{
		{0, refusalRetryDefault},
		{-time.Second, refusalRetryDefault},
		{time.Millisecond, refusalRetryMin},
		{refusalRetryMin - 1, refusalRetryMin},
		{refusalRetryMin, refusalRetryMin},
		{refusalRetryMax / 2, refusalRetryMax / 2},
		{refusalRetryMax, refusalRetryMax},
		{refusalRetryMax + 1, refusalRetryMax},
		{time.Hour, refusalRetryMax},
	} {
		if got := refusalWait(c.in); got != c.want {
			t.Errorf("refusalWait(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCutStamps(t *testing.T) {
	var c cutStamps
	if _, ok := c.since("alpha"); ok {
		t.Fatal("a model nobody cut reads as cut")
	}
	c.note("alpha")
	d, ok := c.since("alpha")
	if !ok || d < 0 || d > time.Second {
		t.Fatalf("since = %v, %v, want a moment ago", d, ok)
	}
	if _, ok := c.since("beta"); ok {
		t.Fatal("a cut on one model was read as a cut on another")
	}
}

func TestRefusalHoldArmed(t *testing.T) {
	post := func(rewindable bool) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
		if rewindable {
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("{}")), nil }
		}
		return r
	}
	get := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	t.Run("nothing was cut: not armed", func(t *testing.T) {
		b := &baseRouter{}
		if b.refusalHoldArmed("alpha", post(true), time.Time{}) {
			t.Fatal("armed with no cut on record")
		}
	})
	t.Run("a recent cut on the model: armed", func(t *testing.T) {
		b := &baseRouter{}
		b.cuts.note("alpha")
		if !b.refusalHoldArmed("alpha", post(true), time.Time{}) {
			t.Fatal("not armed right after a cut")
		}
		if !b.refusalHoldArmed("alpha", get, time.Time{}) {
			t.Fatal("a request with no body needs no GetBody and must be armed")
		}
	})
	t.Run("the cut was on another model: not armed", func(t *testing.T) {
		b := &baseRouter{}
		b.cuts.note("beta")
		if b.refusalHoldArmed("alpha", post(true), time.Time{}) {
			t.Fatal("armed by a cut on another model")
		}
	})
	t.Run("a body that cannot be sent again: not armed", func(t *testing.T) {
		b := &baseRouter{}
		b.cuts.note("alpha")
		if b.refusalHoldArmed("alpha", post(false), time.Time{}) {
			t.Fatal("armed for a request whose body is spent")
		}
	})
	t.Run("the cut is older than the window: not armed", func(t *testing.T) {
		old := cutLingerWindow
		cutLingerWindow = 20 * time.Millisecond
		t.Cleanup(func() { cutLingerWindow = old })
		b := &baseRouter{}
		b.cuts.note("alpha")
		time.Sleep(40 * time.Millisecond)
		if b.refusalHoldArmed("alpha", post(true), time.Time{}) {
			t.Fatal("armed by a cut older than cutLingerWindow")
		}
	})
	t.Run("inside a hold the budget decides", func(t *testing.T) {
		b := &baseRouter{} // no cut on record: the hold already running is what arms it
		if !b.refusalHoldArmed("alpha", post(true), time.Now()) {
			t.Fatal("a hold with its whole budget left was disarmed")
		}
		if b.refusalHoldArmed("alpha", post(true), time.Now().Add(-refusalRetryBudget-time.Second)) {
			t.Fatal("a hold past its budget stayed armed")
		}
	})
}

// The unit-level form of the failure: the engine committed the stream's status
// line in the first attempt, the replay's answer is a refusal. Its body must not
// become a line of the stream.
func TestPingWriter_RefusalOnACommittedStreamBecomesAnSSEError(t *testing.T) {
	pw, rec := newAdmissionPW(t)
	pw.WriteHeader(http.StatusOK) // attempt 1: the engine's own status line
	mustWrite(t, pw, msgStartFrame("msg_A")+frPing)

	pw.WriteHeader(http.StatusServiceUnavailable) // attempt 2, the replay: refused
	mustWrite(t, pw, splashRefusalBody)

	want := msgStartFrame("msg_A") + frPing +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"llama-swap: upstream failed with status 503 during load/prefill\"}}\n\n"
	if got := rec.bodyString(); got != want {
		t.Fatalf("stream:\n got %q\nwant %q", got, want)
	}
	if rec.statusCode() != http.StatusOK {
		t.Fatalf("the client's status is %d, want the 200 it already read", rec.statusCode())
	}
	if bad, ok := sseParsable(rec.bodyString()); !ok {
		t.Fatalf("a line that is not an SSE field reached the client: %q", bad)
	}
}

// A replay that is answered normally is unaffected by the mapping.
func TestPingWriter_OKOnACommittedStreamIsStillSwallowed(t *testing.T) {
	pw, rec := newAdmissionPW(t)
	pw.WriteHeader(http.StatusOK)
	mustWrite(t, pw, frPing)
	pw.WriteHeader(http.StatusOK) // the replay's own status line
	mustWrite(t, pw, frPing+blockStartFrame)

	if got, want := rec.bodyString(), frPing+frPing+blockStartFrame; got != want {
		t.Fatalf("stream:\n got %q\nwant %q", got, want)
	}
}
