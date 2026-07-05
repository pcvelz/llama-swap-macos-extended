package menubar

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// Launcher manages the llama-swap-menu sidecar process.
type Launcher struct {
	log    *logmon.Monitor
	mu     sync.Mutex
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// New creates a Launcher. log is used for diagnostic messages only.
func New(log *logmon.Monitor) *Launcher {
	return &Launcher{log: log}
}

// Start locates the sidecar next to the current executable and runs it.
func (l *Launcher) Start() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}

	sidecar := filepath.Join(filepath.Dir(exe), "llama-swap-menu")
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
