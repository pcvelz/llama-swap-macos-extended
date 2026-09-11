package process

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// The idle TTL is the process-side twin of the scheduler's swap-grace clock
// (scheduler/status_read_idle_test.go): both must start when the last REAL
// request finishes, so an idle resident unloads once its cooldown is over.
// A status read (GET/HEAD /slots, /props, /metrics, /health) is an
// observation, not use. Witnessed on :8001 2026-09-11 (llama-cm
// llama/incidents/2026-09-10-two-cooldowns-for-one-resident-status-polls-queue-swaps.md
// § 2026-09-11 follow-up): a GET /upstream/<model>/health moved lastUse to
// the second of the read, and with the menu-bar helper and llama-status
// polling the resident every ~23 s the TTL never elapsed - zero TTL unloads
// in a whole day.

func statusReadTTLProcess(t *testing.T, unloadAfter int) *ProcessCommand {
	t.Helper()
	skipIfNoSimpleResponder(t)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mock.Close)
	cmd, _ := simpleResponderCmd(t, "-silent")
	return newProcessCommand(t, config.ModelConfig{
		Cmd:                cmd,
		Proxy:              mock.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		UnloadAfter:        unloadAfter,
		UnloadTimeout:      1,
	})
}

func serve(p *ProcessCommand, method, path string, hdr ...string) int {
	r := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, r)
	return rr.Code
}

// The user-visible bug: last slot finished, nothing queued, cooldown over -
// and the model stays loaded because something is watching it.
func TestProcessCommand_TTL_StatusReadsDoNotHoldIdleModel(t *testing.T) {
	p := statusReadTTLProcess(t, 1)
	runErr := runAsync(t, p)
	defer func() {
		if p.State() == StateReady {
			p.Stop(testStopTimeout)
		}
	}()

	if code := serve(p, http.MethodPost, "/v1/chat/completions"); code != http.StatusOK {
		t.Fatalf("real request: status %d, want 200", code)
	}

	// Poll the resident the way the menu-bar helper and llama-status do,
	// far more often than the 1 s TTL, until it unloads or 5 s pass.
	deadline := time.Now().Add(5 * time.Second)
	for p.State() == StateReady && time.Now().Before(deadline) {
		serve(p, http.MethodGet, "/slots")
		serve(p, http.MethodHead, "/health")
		serve(p, http.MethodGet, "/metrics")
		time.Sleep(50 * time.Millisecond)
	}
	// Checked before the polling stops: once it stops, the TTL fires
	// regardless, which is exactly what would hide the bug.
	if p.State() == StateReady {
		t.Fatalf("still loaded after 5 s of status reads on a 1 s TTL: the reads are holding the idle model")
	}
	waitForState(t, p, StateStopped)

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() after TTL stop: %v", err)
		}
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after TTL stop")
	}
}

// lastUse (the idle clock, also the UI's "last used") moves only on use:
// a real turn (POST, including count_tokens, which is part of one) and a
// websocket session. GET/HEAD status reads leave it where it was.
func TestProcessCommand_LastUse_OnlyRealRequestsStamp(t *testing.T) {
	p := statusReadTTLProcess(t, 0)
	runErr := runAsync(t, p)
	defer func() {
		p.Stop(testStopTimeout)
		<-runErr
	}()

	serve(p, http.MethodPost, "/v1/messages")
	after := p.LastUse()
	if after.IsZero() {
		t.Fatal("a POST did not stamp lastUse")
	}

	for _, read := range []struct{ method, path string }{
		{http.MethodGet, "/slots"},
		{http.MethodHead, "/props"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/health"},
	} {
		time.Sleep(5 * time.Millisecond)
		serve(p, read.method, read.path)
		if got := p.LastUse(); !got.Equal(after) {
			t.Errorf("%s %s moved lastUse %s -> %s; a status read is not use",
				read.method, read.path, after.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano))
		}
	}

	time.Sleep(5 * time.Millisecond)
	serve(p, http.MethodPost, "/v1/messages/count_tokens")
	if got := p.LastUse(); !got.After(after) {
		t.Errorf("count_tokens POST did not move lastUse (still %s)", got.Format(time.RFC3339Nano))
	}
	after = p.LastUse()

	time.Sleep(5 * time.Millisecond)
	serve(p, http.MethodGet, "/socket", "Connection", "Upgrade", "Upgrade", "websocket")
	if got := p.LastUse(); !got.After(after) {
		t.Errorf("websocket session did not move lastUse (still %s)", got.Format(time.RFC3339Nano))
	}
}
