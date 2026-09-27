//go:build !darwin

package membrake

import "errors"

// evictFile has no implementation off macOS: the brake only runs there.
func evictFile(path string) error {
	return errors.New("membrake: file-cache eviction is only implemented on macOS")
}

// residentBytes has no implementation off macOS; the gate then opens only on
// drainBelowGB.
func residentBytes(path string) (int64, error) {
	return 0, errors.New("membrake: file-cache residency is only implemented on macOS")
}
