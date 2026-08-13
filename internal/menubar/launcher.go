package menubar

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// Environment variables passed to the sidecar so it can find the llama-swap
// instance that launched it and render the configured bars. Both helpers
// (macos-menu Swift app and cmd/llama-swap-tray) read these.
const (
	EnvBaseURL = "LLAMA_SWAP_MENU_BASE_URL" // e.g. http://127.0.0.1:8080
	EnvBars    = "LLAMA_SWAP_MENU_BARS"     // e.g. gpu,vram
)

// Options configures the sidecar launch.
type Options struct {
	// ListenAddr is llama-swap's own listen address (e.g. ":8080",
	// "localhost:8001"). It is converted to a loopback base URL for the sidecar.
	ListenAddr string
	// TLS selects the https scheme for the sidecar's base URL.
	TLS bool
	// Bars are the metric keys the sidecar should render, in order.
	Bars []string
}

// Launcher manages the menu-bar / system-tray sidecar process.
type Launcher struct {
	log    *logmon.Monitor
	opts   Options
	mu     sync.Mutex
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// New creates a Launcher. log is used for diagnostic messages only.
func New(log *logmon.Monitor, opts Options) *Launcher {
	return &Launcher{log: log, opts: opts}
}

// Supported reports whether the current platform has a sidecar helper.
func Supported() bool {
	switch runtime.GOOS {
	case "darwin", "windows", "linux":
		return true
	default:
		return false
	}
}

// SidecarName returns the platform's helper binary name. macOS uses the
// native Swift menu-bar app; Windows and Linux use the Go system-tray app.
func SidecarName() string {
	switch runtime.GOOS {
	case "darwin":
		return "llama-swap-menu"
	case "windows":
		return "llama-swap-tray.exe"
	default:
		return "llama-swap-tray"
	}
}

// BaseURL converts a listen address into a loopback URL the sidecar can call.
// Wildcard and empty hosts bind all interfaces, so loopback always reaches
// them; a specific host (e.g. a LAN IP) is kept as-is.
func BaseURL(listenAddr string, tls bool) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host, port = "", "8080"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, port))
}

// reapOrphanedSidecars terminates helper processes whose parent is gone.
//
// The sidecar is spawned before llama-swap binds its listener, and not every
// exit path can stop it: a SIGKILLed proxy runs no shutdown code at all. macOS
// additionally has no PDEATHSIG, so an orphan keeps polling and keeps drawing
// its menu-bar icon indefinitely. Clearing orphans at launch bounds the icon
// count at one without anyone killing a process by hand, and covers helpers
// from builds that predate the helper's own parent-death watchdog.
//
// Only ppid==1 processes are touched: a helper with a living parent belongs to
// another llama-swap instance and is not ours to kill.
func reapOrphanedSidecars(log *logmon.Monitor) {
	name := SidecarName()
	out, err := exec.Command("pgrep", "-x", name).Output()
	if err != nil {
		// Non-zero exit means no matches; a missing pgrep means we simply
		// cannot enumerate. Neither is worth failing the launch over.
		return
	}

	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		ppidOut, err := exec.Command("ps", "-o", "ppid=", "-p", field).Output()
		if err != nil || strings.TrimSpace(string(ppidOut)) != "1" {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}

		_ = proc.Signal(os.Interrupt)

		// Escalate if it does not go: the orphans worth reaping are precisely
		// the ones from older builds, which cannot be assumed to handle a
		// signal at all. Leaving one alive would defeat the whole reap.
		if !waitForExit(pid, 2*time.Second) {
			_ = proc.Kill()
		}
		log.Infof("reaped orphaned %s (pid %d)", name, pid)
	}
}

// waitForExit reports whether pid is gone within the timeout. Signal 0 probes
// liveness without delivering anything.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// Start locates the sidecar next to the current executable and runs it.
func (l *Launcher) Start() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}

	// Before adding an icon, remove any that no longer have an owner.
	reapOrphanedSidecars(l.log)

	sidecar := filepath.Join(filepath.Dir(exe), SidecarName())
	if _, err := os.Stat(sidecar); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("sidecar not found at %s", sidecar)
		}
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	l.mu.Lock()
	l.cancel = cancel
	l.cmd = exec.CommandContext(ctx, sidecar)
	l.cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=%s", EnvBaseURL, BaseURL(l.opts.ListenAddr, l.opts.TLS)),
		fmt.Sprintf("%s=%s", EnvBars, strings.Join(l.opts.Bars, ",")),
	)
	l.cmd.Stdout = os.Stdout
	l.cmd.Stderr = os.Stderr
	l.mu.Unlock()

	if err := l.cmd.Start(); err != nil {
		l.Stop()
		return fmt.Errorf("failed to start menu-bar helper: %w", err)
	}

	go func() {
		if err := l.cmd.Wait(); err != nil && ctx.Err() == nil {
			l.log.Warnf("menu-bar helper exited: %v", err)
		}
	}()

	return nil
}

// Stop terminates the sidecar gracefully.
func (l *Launcher) Stop() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.cancel != nil {
		l.cancel()
	}

	if l.cmd != nil && l.cmd.Process != nil {
		return l.cmd.Process.Signal(os.Interrupt)
	}

	return nil
}
