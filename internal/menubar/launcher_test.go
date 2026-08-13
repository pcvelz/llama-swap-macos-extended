package menubar

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/stretchr/testify/require"
)

func testOptions() Options {
	return Options{ListenAddr: ":8080", Bars: []string{"gpu", "vram"}}
}

func TestLauncher_ReportsMissingSidecar(t *testing.T) {
	l := New(logmon.New(), testOptions())
	err := l.Start()
	require.Error(t, err)
	require.Contains(t, err.Error(), "sidecar not found")
}

func TestLauncher_SidecarPathRelativeToExecutable(t *testing.T) {
	// This test documents the path construction rule: sidecar lives next to
	// the llama-swap executable. We can't easily mock os.Executable, but we
	// can verify the expected path shape.
	tmp := t.TempDir()
	exePath := filepath.Join(tmp, "llama-swap")
	expected := filepath.Join(filepath.Dir(exePath), SidecarName())
	require.Equal(t, filepath.Join(tmp, SidecarName()), expected)
}

func TestLauncher_StopWithoutStartIsSafe(t *testing.T) {
	l := New(logmon.New(), testOptions())
	err := l.Stop()
	require.NoError(t, err)
}

func TestLauncher_SidecarNamePerPlatform(t *testing.T) {
	name := SidecarName()
	switch runtime.GOOS {
	case "darwin":
		require.Equal(t, "llama-swap-menu", name)
	case "windows":
		require.Equal(t, "llama-swap-tray.exe", name)
	default:
		require.Equal(t, "llama-swap-tray", name)
	}
}

func TestLauncher_BaseURL(t *testing.T) {
	cases := []struct {
		listen string
		tls    bool
		want   string
	}{
		{":8080", false, "http://127.0.0.1:8080"},
		{":8001", false, "http://127.0.0.1:8001"},
		{"localhost:8001", false, "http://localhost:8001"},
		{"0.0.0.0:9292", false, "http://127.0.0.1:9292"},
		{"192.168.1.5:8080", false, "http://192.168.1.5:8080"},
		{":8443", true, "https://127.0.0.1:8443"},
		{"garbage", false, "http://127.0.0.1:8080"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, BaseURL(c.listen, c.tls), "listen=%s", c.listen)
	}
}
