package swaputil

import "context"

// SlotRebound is called by the router when it binds a granted request to a
// different upstream slot than the one its session lane pinned (see
// internal/router/slotbind.go). The slot-affinity middleware installs it so
// the lane follows the request to the slot that now holds its KV cache. It
// lives here because the router cannot import internal/server.
type SlotRebound func(slot int)

type slotReboundKey struct{}

func WithSlotRebound(ctx context.Context, fn SlotRebound) context.Context {
	return context.WithValue(ctx, slotReboundKey{}, fn)
}

func SlotReboundFromContext(ctx context.Context) (SlotRebound, bool) {
	fn, ok := ctx.Value(slotReboundKey{}).(SlotRebound)
	return fn, ok && fn != nil
}
