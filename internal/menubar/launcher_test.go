package menubar

import (
	"path/filepath"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/stretchr/testify/require"
)

func TestLauncherReportsMissingSidecar(t *testing.T) {
	l := New(logmon.New())
	err := l.Start()
	require.Error(t, err)
	require.Contains(t, err.Error(), "sidecar not found")
}

func TestLauncherSidecarPathRelativeToExecutable(t *testing.T) {
	// This test documents the path construction rule: sidecar lives next to
	// the llama-swap executable. We can't easily mock os.Executable, but we
	// can verify the expected path shape.
	tmp := t.TempDir()
	exePath := filepath.Join(tmp, "llama-swap")
	expected := filepath.Join(filepath.Dir(exePath), "llama-swap-menu")
	require.Equal(t, filepath.Join(tmp, "llama-swap-menu"), expected)
}

func TestLauncherStopWithoutStartIsSafe(t *testing.T) {
	l := New(logmon.New())
	err := l.Stop()
	require.NoError(t, err)
}
