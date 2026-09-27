//go:build linux

package process

import (
	"os"
	"strconv"
	"strings"
)

// processArgv returns the argv pid is running with now, from /proc/<pid>/cmdline.
func processArgv(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"), nil
}
