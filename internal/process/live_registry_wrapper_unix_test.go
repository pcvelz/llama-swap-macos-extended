//go:build !windows

package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// llama-cm starts every model through a wrapper script
// (llama-child-cq27.sh ${PORT}) that resolves the GGUF and then EXECs
// llama-server with --model <path>. The argv llama-swap started carries no
// model flag, so a registry that reads only that argv hands the memory brake
// an empty file list, and the brake's post-kill eviction drops nothing
// (2026-09-27 12:43: "MEMORY-BRAKE-EVICT files=0", 25.29 -> 25.29 GB, every
// local load held for 2h+). The files must come from the process as it runs
// once it is ready, i.e. after the wrapper's exec.
//
// The stand-in server is python3 serving /health, exec'd by a shell wrapper
// with "--model <path>" as extra argv - the same shape as the real wrapper.
func TestLiveRegistry_ModelFilesFromExecedWrapper(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH; the stand-in server needs it")
	}
	dir := t.TempDir()
	model := filepath.Join(dir, "Qwen-Test-Q5_K_XL.gguf")
	if err := os.WriteFile(model, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	port := getFreePort(t)
	server := `import http.server,sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self,*a): pass
http.server.HTTPServer(("127.0.0.1",int(sys.argv[1])),H).serve_forever()`
	serverPy := filepath.Join(dir, "server.py")
	if err := os.WriteFile(serverPy, []byte(server), 0o644); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "llama-child-test.sh")
	script := fmt.Sprintf("#!/bin/sh\n# resolve the model, then become the server (as llama-child-cq27.sh does)\nexec '%s' '%s' %d --model '%s'\n", py, serverPy, port, model)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           fmt.Sprintf("%s %d", wrapper, port),
		Proxy:         fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint: "/health",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := p.EnsureReady(ctx, 20*time.Second); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	c, _, ok := liveFor(p)
	if !ok {
		t.Fatal("a running child must be in the live registry")
	}
	t.Cleanup(func() { _ = syscall.Kill(-c.Pgid, syscall.SIGKILL) })

	if !slices.Contains(c.Files, model) {
		t.Fatalf("live registry files = %q; want the exec'd server's --model %q (the wrapper argv has none)", c.Files, model)
	}

	// The resolution happens once, at ready - never on the brake's 1 s hot path.
	buf := make([]LiveChild, 0, 16)
	if a := testing.AllocsPerRun(100, func() { buf, _ = SnapshotLive(buf[:0]) }); a != 0 {
		t.Fatalf("SnapshotLive allocated %.1f per call on the brake's hot path", a)
	}
}
