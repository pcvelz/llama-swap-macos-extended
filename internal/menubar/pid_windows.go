//go:build windows

package menubar

// pidAlive: the reap path never runs on Windows (it enumerates via pgrep/ps,
// absent there), so report not-alive and let waitForExit return immediately.
func pidAlive(int) bool {
	return false
}
