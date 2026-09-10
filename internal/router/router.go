package router

import (
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

var (
	ErrNoRouterFound     = swaputil.ErrNoRouterFound
	ErrNoPeerModelFound  = swaputil.ErrNoPeerModelFound
	ErrNoLocalModelFound = swaputil.ErrNoLocalModelFound
)

type Router interface {
	// Shutdown blocks until the router has shutdown returning nil
	// when the router has shutdown successfully.
	//
	// timeout controls how long to wait for inflight requests to finish. After
	// the timeout all inflight requests will be cancelled.
	Shutdown(timeout time.Duration) error

	// ServeHTTP implements the http.Handler and requests coming in will
	// trigger any model swapping and routing logic.
	ServeHTTP(http.ResponseWriter, *http.Request)

	// Handles reports whether this router can serve requests for the given model.
	Handles(model string) bool
}

// LocalRouter is a Router backed by local processes whose state can be
// inspected and which can be individually stopped. Peer routers, which only
// forward to remote hosts, do not implement it.
type LocalRouter interface {
	Router

	// RunningModels returns the current state of every process that is not
	// stopped or shut down, keyed by model ID.
	RunningModels() map[string]process.ProcessState

	// Unload stops the named models, or every running model when none are
	// named. It blocks until each targeted process has stopped. A timeout <= 0
	// gives each process its configured unloadTimeout to stop gracefully:
	// models sharing a timeout stop in parallel, smaller timeouts before
	// larger ones. A positive timeout overrides the configured values for
	// every target.
	Unload(timeout time.Duration, models ...string)

	// ProcessLogger returns the log monitor for the named model's process.
	// modelID must be a real (non-alias) config key. Returns false when the
	// model is not known to this router.
	ProcessLogger(modelID string) (*logmon.Monitor, bool)

	// ProcessLastUse returns the last-use time for the named model's process.
	ProcessLastUse(modelID string) (time.Time, bool)

	// Capacity returns per-model serving-slot occupancy: how many requests
	// hold a slot, the ceiling, and how many are parked. Nil when the
	// scheduler does not report capacity. Safe to call from any goroutine.
	//
	// This is the honest counterpart to the tracked in-flight count, which
	// counts parked and running requests alike and so reads high while
	// nothing is being served.
	Capacity() []swaputil.ModelCapacity

	// Pin marks a model as permanently pinned so the TTL goroutine will not
	// idle-evict it. Equivalent to PinWithTTL(modelID, 0).
	Pin(modelID string)

	// PinWithTTL pins a model with a lease deadline (ttl <= 0 pins
	// permanently, matching Pin). Returns the stored deadline (zero for a
	// permanent pin). Re-pinning refreshes an existing lease's deadline.
	PinWithTTL(modelID string, ttl time.Duration) time.Time

	// Unpin removes a model's pin, re-enabling idle eviction.
	Unpin(modelID string)

	// IsPinned reports whether the model is currently pinned.
	IsPinned(modelID string) bool

	// PinExpiry reports the pin state and lease deadline for modelID. The
	// deadline is the zero time.Time for a permanent pin.
	PinExpiry(modelID string) (deadline time.Time, pinned bool)

	// Cooldown returns the current swap-grace cooldown: the resident model
	// idle inside its grace while queued requests for another model wait.
	// Nil when the scheduler does not report cooldowns or nothing is held.
	// Safe to call from any goroutine.
	Cooldown() *swaputil.Cooldown

	// FinishCooldown manually ends the current cooldown, letting the queued
	// swap proceed at the next scheduling pass instead of waiting out the
	// resident's remaining grace. A no-op if nothing is held. Safe to call
	// from any goroutine.
	FinishCooldown()
}
