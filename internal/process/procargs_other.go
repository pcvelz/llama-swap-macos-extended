//go:build !darwin && !linux

package process

import "errors"

// processArgv is not implemented here; the registry keeps the start argv.
func processArgv(pid int) ([]string, error) {
	return nil, errors.New("process argv lookup not supported on this platform")
}
