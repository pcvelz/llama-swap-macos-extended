package swaputil

import "context"

// InflightMetadataSetter lets code deep in the router (which cannot import
// internal/server's inflightTracker without an import cycle - server imports
// router, not the reverse) stamp a key/value pair onto the LIVE in-flight
// entry for the CURRENT request, e.g. slot_granted the moment the scheduler
// hands out a serving slot (internal/router/base.go, the pw.slotGranted()
// call site). Bound to one request's tracker id by the inflight middleware
// (internal/server/inflight.go CreateInflightMiddleware /
// CreateUpstreamInflightMiddleware) via WithInflightMetadataSetter, and
// backed by the tracker's own SetMetadata(id, key, value) update-by-id path -
// unlike the Metadata a live entry is seeded with at Add() time (frozen from
// the request context's bag), this mutates the tracker's live copy directly
// so a grant recorded mid-request is visible before the request completes.
type InflightMetadataSetter func(key, value string)

type inflightMetadataSetterContextKey struct{}

// WithInflightMetadataSetter tags ctx with setter.
func WithInflightMetadataSetter(ctx context.Context, setter InflightMetadataSetter) context.Context {
	return context.WithValue(ctx, inflightMetadataSetterContextKey{}, setter)
}

// InflightMetadataSetterFromContext returns the setter tagged onto ctx by
// WithInflightMetadataSetter, if any. Absent for requests that never passed
// through the inflight middleware (e.g. bare test harnesses or requests on a
// path CreateInflightMiddleware/CreateUpstreamInflightMiddleware skip) -
// callers must treat that as "this request cannot stamp live metadata", not
// an error.
func InflightMetadataSetterFromContext(ctx context.Context) (InflightMetadataSetter, bool) {
	setter, ok := ctx.Value(inflightMetadataSetterContextKey{}).(InflightMetadataSetter)
	return setter, ok
}
