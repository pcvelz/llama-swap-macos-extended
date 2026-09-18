//go:build !(darwin && cgo)

package membrake

import "errors"

// NewNativeSampler has no implementation off macOS (or without cgo): the brake
// is calibrated on the macOS pager and stays off elsewhere.
func NewNativeSampler() (Sampler, error) {
	return nil, errors.New("membrake: no native sampler on this platform (needs darwin + cgo)")
}
