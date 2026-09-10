package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// TestServer_HandleModel_InflightCarriesSessionID is the end-to-end
// regression for the live symptom: a Claude Code /v1/messages request's
// in-flight entry came back with a nil Metadata map even though the request
// carried a session identity, because extraction only looked at the
// metadata.user_id JSON body field while current claude-cli builds send the
// bare session uuid via the X-Claude-Code-Session-Id header instead (see
// internal/swaputil/http_test.go TestExtractContext_SessionIDFromHeader).
// This exercises the real modelChain middleware order (auth -> profile ->
// selector -> CreateRequestContextMiddleware -> CreateInflightMiddleware ->
// filters -> metrics) so a regression in middleware ordering - not just
// extraction - would also be caught here.
func TestServer_HandleModel_InflightCarriesSessionID(t *testing.T) {
	const sessionID = "cf006070-986b-4150-ac78-308a899c3321"

	local := newStubRouter([]string{"m1"}, "ok")
	var s *Server
	var during swaputil.InFlightRequestsEvent
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		during = s.inflight.Current()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}

	proxylog := logmon.NewWriter(io.Discard)
	s = &Server{
		cfg:         config.Config{Models: map[string]config.ModelConfig{"m1": {}}},
		muxlog:      logmon.NewWriter(io.Discard),
		proxylog:    proxylog,
		upstreamlog: logmon.NewWriter(io.Discard),
		inflight:    newInflightTracker(),
		metrics:     newTestMetricsMonitor(t, proxylog, 10, 0),
		local:       local,
		peer:        newStubRouter(nil, ""),
	}
	s.routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if len(during.Requests) != 1 {
		t.Fatalf("inflight during request = %+v, want 1 request", during)
	}
	entry := during.Requests[0]
	if entry.Metadata == nil {
		t.Fatal("inflight entry Metadata is nil, want session_id set")
	}
	if got := entry.Metadata["session_id"]; got != sessionID {
		t.Errorf("inflight entry Metadata[session_id] = %q, want %q", got, sessionID)
	}
}

// TestInflightTracker_Add_StampsTierOnLiveEntry pins the LIVE in-flight
// entry (not just the completed activity-log entry) also carrying the
// arrival tier at admission time. tierSnapshotLocked already captures this
// same value per-request for the ByTier breakdown (req.tier); this asserts
// it is ALSO visible on entry.Metadata, which is what SSE subscribers and
// the /api/inflight snapshot actually read.
func TestInflightTracker_Add_StampsTierOnLiveEntry(t *testing.T) {
	tracker := newInflightTracker("priority", "background")
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	id := tracker.Add(inflightTierReq(swaputil.Tier{Name: "priority", Rank: 10}), cancel)

	entries := tracker.Current().Requests
	if len(entries) != 1 {
		t.Fatalf("Requests = %+v, want 1 entry", entries)
	}
	entry := entries[0]
	if entry.ID != id {
		t.Fatalf("entry.ID = %q, want %q", entry.ID, id)
	}
	if entry.Metadata == nil {
		t.Fatal("entry.Metadata is nil, want tier set")
	}
	if got := entry.Metadata["tier"]; got != "priority" {
		t.Errorf(`entry.Metadata["tier"] = %q, want "priority"`, got)
	}
}

// TestInflightTracker_SetMetadata_UpdatesLiveEntryAndEmits pins the
// update-by-id path a still-parked request's slot grant stamps through
// (see internal/router/base.go's pw.slotGranted() call site, wired via
// swaputil.InflightMetadataSetter). Unlike Add()'s Metadata (frozen from
// the request context's bag), SetMetadata mutates the tracker's own
// mutable copy directly so a grant recorded mid-request is visible on the
// live entry, not only after the request completes.
func TestInflightTracker_SetMetadata_UpdatesLiveEntryAndEmits(t *testing.T) {
	tracker := newInflightTracker()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	id := tracker.Add(req, cancel)

	tracker.SetMetadata(id, "slot_granted", "1")

	entries := tracker.Current().Requests
	if len(entries) != 1 {
		t.Fatalf("Requests = %+v, want 1 entry", entries)
	}
	entry := entries[0]
	if got := entry.Metadata["slot_granted"]; got != "1" {
		t.Errorf(`entry.Metadata["slot_granted"] = %q, want "1"`, got)
	}
}

// anthropicSSEStream is a representative Anthropic (/v1/messages) SSE body: a
// message_start, a content block, THREE content_block_delta events (the real
// output), TWO keepalive `event: ping` events interleaved, and the closing
// message_delta/message_stop. Real output tokens here = 3; pings must never
// bump the counter, and every byte (pings included) still counts toward
// RespBytes.
const anthropicSSEStream = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: ping\n" +
	"data: {\"type\": \"ping\"}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n" +
	"event: ping\n" +
	"data: {\"type\": \"ping\"}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"!\"}}\n\n" +
	"event: content_block_stop\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

const anthropicSSEStreamDeltas = 3

func addTrackedRequest(t *testing.T, tracker *inflightTracker) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return tracker.Add(req, cancel)
}

func onlyEntry(t *testing.T, tracker *inflightTracker) swaputil.InflightRequestEntry {
	t.Helper()
	entries := tracker.Current().Requests
	if len(entries) != 1 {
		t.Fatalf("Requests = %+v, want 1 entry", entries)
	}
	return entries[0]
}

// TestInflightTracker_AddResponseBytes_CountsRealDeltasNotPings pins the core
// contract: RespTokens increments once per content_block_delta event and NEVER
// for a keepalive ping, while RespBytes still counts every byte (delta and
// ping alike) exactly as before this change.
func TestInflightTracker_AddResponseBytes_CountsRealDeltasNotPings(t *testing.T) {
	tracker := newInflightTracker()
	id := addTrackedRequest(t, tracker)

	tracker.AddResponseBytes(id, []byte(anthropicSSEStream))

	entry := onlyEntry(t, tracker)
	if entry.RespTokens != anthropicSSEStreamDeltas {
		t.Errorf("RespTokens = %d, want %d (content_block_delta events only, pings excluded)", entry.RespTokens, anthropicSSEStreamDeltas)
	}
	if want := int64(len(anthropicSSEStream)); entry.RespBytes != want {
		t.Errorf("RespBytes = %d, want %d (every byte, pings included)", entry.RespBytes, want)
	}
}

// TestInflightTracker_AddResponseBytes_SurvivesFrameSplits feeds the SAME
// stream but sliced at EVERY byte boundary (chunks of size 1, then 3, then 7,
// then 13) so `event: content_block_delta` lines are repeatedly split
// mid-frame across AddResponseBytes calls. The counts must match the
// single-shot result regardless of chunking - proving the per-entry carry
// buffer reassembles straddled frames.
func TestInflightTracker_AddResponseBytes_SurvivesFrameSplits(t *testing.T) {
	for _, chunk := range []int{1, 3, 7, 13, 500} {
		t.Run("chunk="+strconv.Itoa(chunk), func(t *testing.T) {
			tracker := newInflightTracker()
			id := addTrackedRequest(t, tracker)

			body := []byte(anthropicSSEStream)
			for i := 0; i < len(body); i += chunk {
				end := i + chunk
				if end > len(body) {
					end = len(body)
				}
				tracker.AddResponseBytes(id, body[i:end])
			}

			entry := onlyEntry(t, tracker)
			if entry.RespTokens != anthropicSSEStreamDeltas {
				t.Errorf("chunk=%d RespTokens = %d, want %d", chunk, entry.RespTokens, anthropicSSEStreamDeltas)
			}
			if want := int64(len(anthropicSSEStream)); entry.RespBytes != want {
				t.Errorf("chunk=%d RespBytes = %d, want %d", chunk, entry.RespBytes, want)
			}
		})
	}
}

// TestInflightTracker_AddResponseBytes_PingsOnlyStayZero is the fixture case
// (hass-token-blind-2026-09-03): a slot that receives ONLY keepalive pings has
// RespBytes climbing while RespTokens stays flat at 0 - the "0 real tokens vs
// healthy occupancy" signal the box was blind to.
func TestInflightTracker_AddResponseBytes_PingsOnlyStayZero(t *testing.T) {
	tracker := newInflightTracker()
	id := addTrackedRequest(t, tracker)

	const ping = "event: ping\ndata: {\"type\": \"ping\"}\n\n"
	for i := 0; i < 20; i++ {
		tracker.AddResponseBytes(id, []byte(ping))
	}

	entry := onlyEntry(t, tracker)
	if entry.RespTokens != 0 {
		t.Errorf("RespTokens = %d, want 0 (pings only)", entry.RespTokens)
	}
	if entry.RespBytes != int64(20*len(ping)) {
		t.Errorf("RespBytes = %d, want %d", entry.RespBytes, 20*len(ping))
	}
}

// TestScanRealOutputDeltas_DataLineSubstringNotCounted guards the exact-match
// on the event line: the string "content_block_delta" also appears inside the
// event's own `data:` line, so a naive substring scan would double-count. Only
// the `event:` line may bump the counter.
func TestScanRealOutputDeltas_DataLineSubstringNotCounted(t *testing.T) {
	// A data line mentioning content_block_delta but with NO preceding event
	// line of that name must count zero.
	deltas, _ := scanRealOutputDeltas(nil, []byte("data: {\"type\":\"content_block_delta\"}\n\n"))
	if deltas != 0 {
		t.Errorf("deltas = %d, want 0 (data-line substring must not count)", deltas)
	}
	// The space-optional and CR-terminated event forms both count.
	deltas, _ = scanRealOutputDeltas(nil, []byte("event:content_block_delta\r\nevent: content_block_delta\n"))
	if deltas != 2 {
		t.Errorf("deltas = %d, want 2 (event:X and event: X\\r both count)", deltas)
	}
}
