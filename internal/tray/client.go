package tray

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Client polls the llama-swap HTTP API and follows its SSE event stream,
// maintaining a State snapshot. OnChange is called (from arbitrary
// goroutines) whenever the display-relevant state changed.
type Client struct {
	BaseURL  string
	Bars     []BarMetric
	OnChange func(State)

	HTTP *http.Client

	// sse has no timeout (the event stream is long-lived); loader allows for
	// slow model loads. Both are reused across reconnects/clicks.
	sse    *http.Client
	loader *http.Client

	mu         sync.Mutex
	state      State
	lastPerfTS string // newest /api/performance sample seen, drives ?after=
}

// NewClient builds a client from the environment contract set by the
// llama-swap launcher (falling back to defaults for standalone runs).
func NewClient() *Client {
	return &Client{
		BaseURL: BaseURLFromEnv(),
		Bars:    BarsFromEnv(),
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		sse:     &http.Client{},
		loader:  &http.Client{Timeout: 300 * time.Second},
	}
}

// Snapshot returns a copy of the current state.
func (c *Client) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// update applies a mutation and notifies OnChange only when the state
// actually changed, so a steady backend doesn't force tray redraws (SetIcon
// on Linux round-trips DBus on every call).
func (c *Client) update(mutate func(*State)) {
	c.mu.Lock()
	before := c.state
	mutate(&c.state)
	snapshot := c.state
	c.mu.Unlock()
	if c.OnChange != nil && !reflect.DeepEqual(before, snapshot) {
		c.OnChange(snapshot)
	}
}

// Run starts the poll loop and the SSE listener; it returns when ctx ends.
func (c *Client) Run(ctx context.Context) {
	go c.eventLoop(ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	c.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollOnce(ctx)
		}
	}
}

func (c *Client) pollOnce(ctx context.Context) {
	perf, perfErr := c.fetchPerformance(ctx)
	completed, metricsErr := c.fetchCompletedCount(ctx)
	c.update(func(s *State) {
		if perfErr == nil {
			s.BackendOnline = true
			// An empty incremental response means no new samples since the
			// last poll — keep the previous readings.
			if len(perf.GpuStats) > 0 || len(perf.SysStats) > 0 {
				s.BarValues = perf.barValues(c.Bars)
			}
		} else {
			s.BackendOnline = false
		}
		if metricsErr == nil {
			s.Completed = completed
		}
	})
}

// fetchPerformance polls incrementally: ?after=<last seen timestamp> keeps
// the response to new samples instead of the endpoint's full ring buffer
// (up to an hour of history) on every 2s tick.
func (c *Client) fetchPerformance(ctx context.Context) (perfResponse, error) {
	var out perfResponse
	path := "/api/performance"
	c.mu.Lock()
	if c.lastPerfTS != "" {
		path += "?after=" + url.QueryEscape(c.lastPerfTS)
	}
	c.mu.Unlock()
	body, err := c.get(ctx, path)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	if ts := out.lastTimestamp(); ts != "" {
		c.mu.Lock()
		c.lastPerfTS = ts
		c.mu.Unlock()
	}
	return out, nil
}

// fetchCompletedCount counts entries in the activity log, mirroring the
// macOS helper's "N completed" readout.
func (c *Client) fetchCompletedCount(ctx context.Context) (int, error) {
	body, err := c.get(ctx, "/api/metrics")
	if err != nil {
		return 0, err
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(body, &entries); err != nil {
		return 0, err
	}
	return len(entries), nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// eventLoop follows GET /api/events (SSE) with a 2s reconnect delay,
// matching the macOS helper's behaviour.
func (c *Client) eventLoop(ctx context.Context) {
	for {
		c.consumeEvents(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Client) consumeEvents(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/events", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.sse.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var eventBuf strings.Builder
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "data:"):
			eventBuf.WriteString(strings.TrimPrefix(line, "data:"))
		case line == "":
			payload := eventBuf.String()
			eventBuf.Reset()
			if payload == "" {
				continue
			}
			c.update(func(s *State) {
				s.decodeEvent([]byte(payload))
			})
		}
	}
}

// LoadModel asks llama-swap to load the model, using the same POST
// /upstream/<id>/ path as the web UI's Load button (GET returns 503 for
// non-resident models by design; see the eager-reload guard). The model ID
// is deliberately NOT path-escaped: the server routes /upstream/{path...}
// as a wildcard and IDs like "author/model" must keep their literal slashes.
func (c *Client) LoadModel(ctx context.Context, modelID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/upstream/"+modelID+"/", bytes.NewReader(nil))
	if err != nil {
		return err
	}
	resp, err := c.loader.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// UnloadAll unloads all running models.
func (c *Client) UnloadAll(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/models/unload", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
