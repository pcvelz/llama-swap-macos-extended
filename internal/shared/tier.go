package shared

import "context"

// Tier identifies which entry point (listener) a request arrived through. The
// main `-listen` port is always the implicit "default" tier (Rank 0); extra
// tiers are declared under the top-level `tiers:` config block and each gets
// its own additional http.Server sharing the same handler (see llama-swap.go).
// A tier is NOT a separate queue - it is a pre-sort key every request carries
// into the one shared FIFO queue. See docs/intent/llama-swap-tiers.md
// (llama-cm) for the full design.
type Tier struct {
	// Name is the tier's config key, or "default" for the implicit tier.
	Name string
	// Rank orders the shared queue: higher ranks are serviced first, and a
	// queued request is never granted while a strictly higher-rank request is
	// still queued (the "rank barrier"). The implicit default tier is Rank 0.
	Rank int
	// Preempts, when true, lets an arrival on this tier boot ANY running
	// lower-rank request, including non-preemptible ones.
	Preempts bool
	// Preemptible, when true, lets a running request on this tier be booted
	// by any higher-rank arrival, even one without Preempts set.
	Preemptible bool
}

// DefaultTier is the tier assigned to every request that did not arrive
// through a configured tier listener - i.e. everything on the main port when
// no `tiers:` block is configured, and every request routed to it after
// tiers ARE configured. Rank 0, no preemption flags: byte-identical queue
// behavior to a config with no tiers.
var DefaultTier = Tier{Name: "default"}

type tierContextKey struct{}

// WithTier tags ctx with the tier a request arrived through. Set once, at the
// listener (llama-swap.go), before the request ever reaches routing/scheduling
// so every downstream layer (ReqContextData, scheduler.HandlerReq, the
// inflight counter) can read the same tag.
func WithTier(ctx context.Context, tier Tier) context.Context {
	return context.WithValue(ctx, tierContextKey{}, tier)
}

// TierFromContext returns the tier tagged onto ctx by WithTier, or DefaultTier
// if none was set (e.g. a request that arrived on the main listener before
// any tier wrapping was added, or in tests that build a bare context).
func TierFromContext(ctx context.Context) Tier {
	if t, ok := ctx.Value(tierContextKey{}).(Tier); ok {
		return t
	}
	return DefaultTier
}
