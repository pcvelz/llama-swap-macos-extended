//go:build !darwin

package membrake

import "errors"

// evictFile has no implementation off macOS: the brake only runs there.
func evictFile(path string) error {
	return errors.New("membrake: file-cache eviction is only implemented on macOS")
}
