package tray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestClient builds a Client pointed at ts with a bounded timeout, close
// enough to production defaults for wire-level assertions without risking a
// slow test on failure.
func newTestClient(ts *httptest.Server) *Client {
	return &Client{
		BaseURL: ts.URL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// TestFetchCompletedCount_ReadsAggregateStatsTotal pins the client to the
// real store.ActivityStats field set (internal/store/store.go) - total_requests
// is the aggregate count; the histogram/other fields must not derail decoding.
// This is the wire-level counterpart to the compile-time-only fix: a wrong
// path or a shape mismatch here fails silently (badge shows 0 forever), so a
// passing compile is not sufficient evidence.
func TestFetchCompletedCount_ReadsAggregateStatsTotal(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total_requests":137,"total_input_tokens":50000,"total_output_tokens":80000,` +
			`"total_cache_tokens":12000,"prompt_histogram":null,"gen_histogram":null}`))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	count, err := c.fetchCompletedCount(context.Background())
	require.NoError(t, err)
	require.Equal(t, 137, count)
	require.Equal(t, "/api/metrics/stats", gotPath, "must poll the new aggregate-stats path")
}

// TestFetchCompletedCount_DoesNotCountPaginatedActivityPage is the negative
// case: if the client (or a future regression) were pointed at the paginated
// /api/metrics/activity shape instead, naively counting the "data" array or
// reading its pagination "total" field would report a page size or a stale
// total instead of the real aggregate. Decoding that shape where the client
// expects the stats object must yield 0, never 25 (page length) or 612 (the
// pagination "total" field) - the exact silent-wrong-number failure this
// endpoint split can reintroduce.
func TestFetchCompletedCount_DoesNotCountPaginatedActivityPage(t *testing.T) {
	rows := make([]string, 25)
	for i := range rows {
		rows[i] = `{"id":` + strconv.Itoa(i) + `}`
	}
	body := `{"data":[` + strings.Join(rows, ",") + `],"page":1,"limit":25,"total":612,"total_pages":25}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer ts.Close()

	c := newTestClient(ts)
	count, err := c.fetchCompletedCount(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, count, "must not mistake a paginated page/array for a request total")
}

// TestFetchCompletedCount_ErrorsOnNonJSONResponse proves a missing/broken
// endpoint (e.g. the removed legacy GET /api/metrics, or a path that 404s)
// surfaces as an error rather than silently decoding to a plausible-looking
// zero that a human would never notice.
func TestFetchCompletedCount_ErrorsOnNonJSONResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := newTestClient(ts)
	_, err := c.fetchCompletedCount(context.Background())
	require.Error(t, err)
}

// TestPollOnce_MetricsErrorPreservesPreviousCompletedCount exercises the
// actual production path (Client.pollOnce): once a good total_requests value
// has been read, a later transport/decode failure on /api/metrics/stats must
// not reset the badge to 0 - that is exactly the silent-zero failure mode
// this whole area is fragile to.
func TestPollOnce_MetricsErrorPreservesPreviousCompletedCount(t *testing.T) {
	var failMetrics atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/performance":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"sys_stats":[],"gpu_stats":[]}`))
		case "/api/metrics/stats":
			if failMetrics.Load() {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"total_requests":10}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer ts.Close()

	c := newTestClient(ts)
	c.pollOnce(context.Background())
	require.Equal(t, 10, c.Snapshot().Completed)

	failMetrics.Store(true)
	c.pollOnce(context.Background())
	require.Equal(t, 10, c.Snapshot().Completed,
		"a metrics error must preserve the last known count, not silently zero it")
}
