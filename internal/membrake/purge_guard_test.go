package membrake

import (
	"os"
	"testing"
)

// A purge that outlives its attempt must not get a sibling: the next attempt
// refuses while one is still running, and runs once none is.
func TestPurgeCommand_RefusesWhileAPurgeIsRunning(t *testing.T) {
	marker := t.TempDir() + "/ran"
	cmd := []string{"/bin/sh", "-c", "touch " + marker}
	if err := purgeCommandWith(cmd, func() bool { return true })(); err == nil {
		t.Fatal("started a purge while another was running")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran although a purge was still running")
	}
	if err := purgeCommandWith(cmd, func() bool { return false })(); err != nil {
		t.Fatalf("no purge running: the command must run, got %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the command did not run")
	}
}
