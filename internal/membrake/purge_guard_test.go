package membrake

import (
	"os"
	"runtime"
	"testing"
)

// A purge that outlives its attempt must not get a sibling: the next attempt
// refuses while one is still running, and runs once none is.
func TestPurgeCommand_RefusesWhileAPurgeIsRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test shells out to /bin/sh, which does not exist on Windows")
	}
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

// On Windows the production purge cannot be exercised at all (purge is a
// macOS-only tool), so pin the guard's contract there with a command that
// exists on every platform Go supports: cmd.exe /c exit 0.
func TestPurgeCommand_RefusesWhileAPurgeIsRunning_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only test")
	}
	cmd := []string{"cmd", "/c", "exit 0"}
	if err := purgeCommandWith(cmd, func() bool { return true })(); err == nil {
		t.Fatal("started a purge while another was running")
	}
	if err := purgeCommandWith(cmd, func() bool { return false })(); err != nil {
		t.Fatalf("no purge running: the command must run, got %v", err)
	}
}
