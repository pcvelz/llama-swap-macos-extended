//go:build !windows

package process

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// findPortHolder identifies the process listening on 127.0.0.1:port.
//
// lsof and ps are shelled out to because the kernel interfaces that map a
// listening socket to a pid are not portable across the Unixes this builds for,
// and Go's standard library exposes no equivalent. Both are base system tools on
// macOS and Linux. A missing tool, or no listener, is reported as an error so
// the caller declines to reclaim rather than guessing.
func findPortHolder(port int) (portHolder, error) {
	pid, err := listenerPID(port)
	if err != nil {
		return portHolder{}, err
	}
	ppid, argv, err := processInfo(pid)
	if err != nil {
		return portHolder{}, err
	}
	return portHolder{pid: pid, ppid: ppid, argv: argv}, nil
}

// listenerPID returns the pid of the single process LISTENing on port. More than
// one listener is treated as unidentifiable rather than picking one, so an
// ambiguous situation never leads to killing the wrong process.
func listenerPID(port int) (int, error) {
	out, err := exec.Command("lsof", "-ti", fmt.Sprintf("tcp:%d", port), "-sTCP:LISTEN").Output()
	if err != nil {
		return 0, fmt.Errorf("lsof found no listener on port %d: %w", port, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 1 {
		return 0, fmt.Errorf("expected exactly one listener on port %d, got %d", port, len(fields))
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("unparsable pid %q from lsof: %w", fields[0], err)
	}
	return pid, nil
}

// processInfo returns the parent pid and full argv of pid.
func processInfo(pid int) (int, string, error) {
	out, err := exec.Command("ps", "-o", "ppid=,command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, "", fmt.Errorf("ps failed for pid %d: %w", pid, err)
	}
	line := strings.TrimSpace(string(out))
	ppidStr, argv, found := strings.Cut(line, " ")
	if !found {
		return 0, "", fmt.Errorf("unparsable ps output for pid %d: %q", pid, line)
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(ppidStr))
	if err != nil {
		return 0, "", fmt.Errorf("unparsable ppid %q for pid %d: %w", ppidStr, pid, err)
	}
	return ppid, strings.TrimSpace(argv), nil
}

// killPID force-terminates a single pid. The target is an orphan whose parent is
// already gone, so there is no process group to signal via a negative pid the way
// a live upstream is torn down: its group may since have been reused.
func killPID(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal pid %d", pid)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
