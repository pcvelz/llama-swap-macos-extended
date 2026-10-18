package scheduler

// SlotWarmReporter is implemented by schedulers that can publish per-session
// slot-grace warm-until timestamps. Called from HTTP/SSE handlers outside the
// run loop.
type SlotWarmReporter interface {
	// SlotWarmUntil returns per-session slot-grace warm-until timestamps,
	// keyed by session ID and rendered as RFC3339 strings. Omitted when the
	// model has no slot-grace configured or the session is not warm.
	SlotWarmUntil() map[string]string
}
