// llama-swap-status prints a one-shot JSON queue snapshot and exits, reusing
// the SAME internal/tray client+state model as the tray/menu helpers. It
// exists so shell consumers (llama-cm's cm-menu header line, tmux status
// bars, watch loops) track the menu bar's view of the world instead of
// re-implementing queue probes that drift and starve (native /slots blocks
// on the inference thread under load; everything here is proxy state that
// answers in microseconds).
//
// Exit contract (mirrors what shell callers already expect from a probe):
//
//	backend reachable: print one JSON line, exit 0
//	backend down/unreachable: print NOTHING, exit 1
//
// so "empty output" keeps meaning "down" for every existing caller.
//
//	llama-swap-status                     # snapshot, defaults from env
//	llama-swap-status -base-url http://127.0.0.1:8001 -timeout 3s
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mostlygeek/llama-swap/internal/tray"
)

type snapshot struct {
	Online  bool           `json:"online"`
	Model   string         `json:"model,omitempty"`
	State   string         `json:"state,omitempty"`
	Waiting int            `json:"waiting"`
	ByTier  map[string]int `json:"byTier,omitempty"`
	Summary string         `json:"summary"`
}

func main() {
	timeout := flag.Duration("timeout", 3*time.Second, "overall deadline for assembling the snapshot")
	baseURL := flag.String("base-url", "", "backend address (default: LLAMA_SWAP_MENU_BASE_URL or http://127.0.0.1:8080)")
	flag.Parse()

	client := tray.NewClient()
	if *baseURL != "" {
		client.BaseURL = *baseURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	go client.Run(ctx)

	// Wait until the state carries BOTH the model roster (modelStatus event
	// or poll) and the queue view (first inflight event), or the deadline
	// cuts the wait short. A healthy llama-swap answers the SSE stream's
	// initial burst within milliseconds, so the deadline only ever fires on
	// a sick backend — where a partial snapshot is still the honest answer.
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		s := client.Snapshot()
		if s.Models != nil && s.SeenInflight {
			break
		}
		select {
		case <-ctx.Done():
			goto done
		case <-tick.C:
		}
	}
done:

	s := client.Snapshot()
	if !s.BackendOnline && s.Models == nil && !s.SeenInflight {
		// Nothing answered at all: silent exit 1, "down" for callers.
		os.Exit(1)
	}

	out := snapshot{
		Online:  s.BackendOnline,
		Waiting: s.Waiting,
		ByTier:  s.WaitingByTier,
		Summary: s.WaitingSummary(),
	}
	for _, m := range s.Models {
		if m.ID == s.ActiveModelID || (out.Model == "" && (m.State == "ready" || m.State == "starting")) {
			out.Model = m.DisplayName()
			out.State = m.State
			break
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "llama-swap-status:", err)
		os.Exit(1)
	}
}
