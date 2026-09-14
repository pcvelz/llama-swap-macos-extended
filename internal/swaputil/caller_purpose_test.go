package swaputil

import (
	"net/http"
	"strings"
	"testing"
)

// TestSessionMetadata_CallerPurpose pins the X-Caller-Purpose channel: a
// caller names WHAT a request is for (commit-subject, mm-plan-writer, ...)
// so an in-flight row is not an anonymous curl/python User-Agent. The value
// is free text from any local process, so it is sanitized to a short
// log/UI-safe token rather than trusted verbatim: disallowed runs collapse to
// one '-', edges are trimmed, and the result is capped at CallerPurposeMaxLen.
func TestSessionMetadata_CallerPurpose(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"plain slug", "commit-subject", "commit-subject"},
		{"dots colons slashes kept", "hermes-gate:cq35/v2.1", "hermes-gate:cq35/v2.1"},
		{"spaces collapse", "  mm plan   writer ", "mm-plan-writer"},
		{"quotes cannot forge a log field", "x\" tier=priority evil", "x-tier-priority-evil"},
		{"hostile value", "'; DROP TABLE sessions;--", "DROP-TABLE-sessions"},
		{"capped", strings.Repeat("a", 200), strings.Repeat("a", CallerPurposeMaxLen)},
		{"only junk", "\"';;", ""},
		{"absent", "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
			r.Header.Set("Content-Type", "application/json")
			if c.header != "" {
				r.Header.Set("X-Caller-Purpose", c.header)
			}
			if got := CallerPurposeFromHeader(r); got != c.want {
				t.Errorf("CallerPurposeFromHeader = %q, want %q", got, c.want)
			}
			got, err := extractContext(r)
			if err != nil {
				t.Fatalf("extractContext: %v", err)
			}
			if c.want == "" {
				if v, ok := got.Metadata["purpose"]; ok {
					t.Errorf("purpose = %q, want key absent", v)
				}
				return
			}
			if got.Metadata["purpose"] != c.want {
				t.Errorf("purpose = %q, want %q", got.Metadata["purpose"], c.want)
			}
		})
	}
}

// TestSessionMetadata_CallerPurposeOnGET: the header needs no body, so a
// body-less request carries its purpose too.
func TestSessionMetadata_CallerPurposeOnGET(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/v1/models?model=m", nil)
	r.Header.Set("X-Caller-Purpose", "cm-launch-prewarm")
	got, err := extractContext(r)
	if err != nil {
		t.Fatalf("extractContext: %v", err)
	}
	if got.Metadata["purpose"] != "cm-launch-prewarm" {
		t.Errorf("purpose = %q, want %q", got.Metadata["purpose"], "cm-launch-prewarm")
	}
}
