//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// bindPortHelperEnv marks a re-exec of the test binary as the port-holding
// helper rather than a normal test run.
const bindPortHelperEnv = "LLAMASWAP_TEST_BIND_PORT"

// TestHelperBindPort is not a test. Re-executed with bindPortHelperEnv set, it
// binds the requested port and serves a health endpoint, standing in for both a
// leaked upstream and a legitimate one. The port is read from the environment so
// a caller can choose whether it also appears in argv, which is what the argv
// guard keys on.
//
// Exiting non-zero when the bind fails is the behaviour under test: that is how
// a real upstream reacts to finding its port already held, and it is what drives
// the premature-exit path.
func TestHelperBindPort(t *testing.T) {
	port := os.Getenv(bindPortHelperEnv)
	if port == "" {
		t.Skip("not the helper process")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		os.Exit(1)
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go func() { _ = http.Serve(ln, mux) }()
	time.Sleep(60 * time.Second)
}

// helperCommand builds the shell line that runs the port-binding helper with
// the port visible in argv, for use as a model's cmd.
func helperCommand(t *testing.T, port int) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return fmt.Sprintf("%s -test.run=TestHelperBindPort -test.timeout=120s -test.parallel=%d", self, port)
}

func freeTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func waitPortBound(t *testing.T, port int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// spawnPortHolder starts a helper that binds port. When orphan is true the
// helper is launched behind a shell that exits immediately, so the helper
// reparents to PID 1 exactly as a child of a killed llama-swap does. When
// inArgv is true the port is also passed as a command-line argument, which is
// what the argv guard looks for.
//
// Returns the holder pid, or 0 when it could not be identified.
func spawnPortHolder(t *testing.T, port int, orphan, inArgv bool) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	argv := []string{self, "-test.run=TestHelperBindPort", "-test.timeout=120s"}
	if inArgv {
		argv = append(argv, fmt.Sprintf("-test.parallel=%d", port))
	}
	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		quoted = append(quoted, "'"+a+"'")
	}
	line := strings.Join(quoted, " ")

	var cmd *exec.Cmd
	if orphan {
		// The shell exits straight after backgrounding, so the helper is
		// reparented to PID 1 before it is ever inspected.
		cmd = exec.Command("sh", "-c", line+" >/dev/null 2>&1 & exit 0")
	} else {
		cmd = exec.Command("sh", "-c", line+" >/dev/null 2>&1")
	}
	cmd.Env = append(os.Environ(), bindPortHelperEnv+"="+strconv.Itoa(port))

	if orphan {
		if err := cmd.Run(); err != nil {
			t.Fatalf("spawn orphan holder: %v", err)
		}
	} else {
		if err := cmd.Start(); err != nil {
			t.Fatalf("spawn holder: %v", err)
		}
		t.Cleanup(func() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		})
	}

	if !waitPortBound(t, port, 10*time.Second) {
		t.Skip("port holder did not bind in time")
	}
	pid, err := listenerPID(port)
	if err != nil {
		t.Skipf("cannot identify listener (lsof unavailable?): %v", err)
	}
	t.Cleanup(func() {
		if pidAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return pid
}

func reclaimerFor(t *testing.T, port int) *ProcessCommand {
	t.Helper()
	return newProcessCommand(t, config.ModelConfig{
		Cmd:           "sleep 30",
		Proxy:         fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint: "/health",
	})
}

func TestUpstreamPort(t *testing.T) {
	cases := []struct {
		proxy string
		want  int
		ok    bool
	}{
		{"http://localhost:18006", 18006, true},
		{"http://127.0.0.1:9999/", 9999, true},
		{"  http://localhost:18001  ", 18001, true},
		{"http://localhost", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := upstreamPort(c.proxy)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("upstreamPort(%q) = %d, %v; want %d, nil", c.proxy, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("upstreamPort(%q) = %d, nil; want an error", c.proxy, got)
		}
	}
}

// The argv guard must survive a wrapper script exec'ing the real server, which
// is why it matches the injected port rather than the configured command.
func TestArgvCarriesPort(t *testing.T) {
	cases := []struct {
		argv string
		port int
		want bool
	}{
		{"/bin/llama-server --port 18006 --host 127.0.0.1", 18006, true},
		{"/bin/llama-server --port=18006", 18006, true},
		{"/bin/llama-server --host 127.0.0.1:18006", 18006, true},
		{"/bin/llama-server --port 18006", 18007, false},
		{"/bin/llama-server --port 180060", 18006, false},
		{"/bin/other-daemon --unrelated 1", 18006, false},
		{"", 18006, false},
	}
	for _, c := range cases {
		if got := argvCarriesPort(c.argv, c.port); got != c.want {
			t.Errorf("argvCarriesPort(%q, %d) = %v; want %v", c.argv, c.port, got, c.want)
		}
	}
}

// A holder whose parent is still alive is NOT an orphan. This is the guard that
// keeps a concurrently running isolated test box safe: it shares the child port
// range, but its children have a live parent.
func TestReclaimDeclinesHolderWithLiveParent(t *testing.T) {
	port := freeTestPort(t)
	pid := spawnPortHolder(t, port, false, true)

	if reclaimerFor(t, port).reclaimOrphanedUpstream() {
		t.Fatal("reclaimed a port whose holder has a live parent")
	}
	if !pidAlive(pid) {
		t.Fatalf("holder pid %d was killed despite having a live parent", pid)
	}
}

// An orphan that does not look like this model's upstream is left alone.
func TestReclaimDeclinesOrphanWithUnrelatedArgv(t *testing.T) {
	port := freeTestPort(t)
	pid := spawnPortHolder(t, port, true, false)

	ppid, argv, err := processInfo(pid)
	if err != nil {
		t.Skipf("cannot inspect holder: %v", err)
	}
	if ppid != 1 {
		t.Skipf("holder pid %d has parent %d, not reparented yet", pid, ppid)
	}
	if argvCarriesPort(argv, port) {
		t.Skipf("helper argv unexpectedly carries the port: %q", argv)
	}

	if reclaimerFor(t, port).reclaimOrphanedUpstream() {
		t.Fatal("reclaimed a port held by an orphan with unrelated argv")
	}
	if !pidAlive(pid) {
		t.Fatalf("orphan pid %d was killed despite failing the argv guard", pid)
	}
}

// All three guards satisfied: the orphan is killed and the port comes back.
func TestReclaimRecoversOrphanedPort(t *testing.T) {
	port := freeTestPort(t)
	pid := spawnPortHolder(t, port, true, true)

	ppid, argv, err := processInfo(pid)
	if err != nil {
		t.Skipf("cannot inspect holder: %v", err)
	}
	if ppid != 1 {
		t.Skipf("holder pid %d has parent %d, not reparented yet", pid, ppid)
	}
	if !argvCarriesPort(argv, port) {
		t.Skipf("helper argv does not carry the port: %q", argv)
	}

	if !reclaimerFor(t, port).reclaimOrphanedUpstream() {
		t.Fatalf("did not reclaim orphaned pid %d on port %d (argv %q)", pid, port, argv)
	}
	if pidAlive(pid) {
		t.Errorf("orphan pid %d survived reclamation", pid)
	}
	if !waitPortFree(port, 5*time.Second) {
		t.Error("port still bound after a reported reclaim")
	}
}

// Nothing listening means the premature exit had another cause entirely.
func TestReclaimDeclinesWhenPortIsFree(t *testing.T) {
	port := freeTestPort(t)
	if reclaimerFor(t, port).reclaimOrphanedUpstream() {
		t.Fatal("reported a reclaim on a port nothing was holding")
	}
}

// The whole recovery, end to end: an orphan holds the child port, so the
// upstream this start spawns cannot bind and exits immediately. Without
// reclamation that is terminal and every later start repeats it. With it, the
// port is freed and the single retry brings the model up.
//
// doStart alone is asserted to fail first, so a change that made this start
// succeed for some unrelated reason cannot leave the test passing vacuously.
func TestStartWithOrphanReclaimRecoversBlockedStart(t *testing.T) {
	port := freeTestPort(t)
	orphan := spawnPortHolder(t, port, true, true)

	ppid, argv, err := processInfo(orphan)
	if err != nil {
		t.Skipf("cannot inspect holder: %v", err)
	}
	if ppid != 1 || !argvCarriesPort(argv, port) {
		t.Skipf("holder pid %d not a usable orphan fixture (ppid %d, argv %q)", orphan, ppid, argv)
	}

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           helperCommand(t, port),
		Proxy:         fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint: "/health",
		Env:           []string{bindPortHelperEnv + "=" + strconv.Itoa(port)},
	})

	ctx := context.Background()
	if res := p.doStart(ctx, testStartTimeout); !errors.Is(res.err, ErrUpstreamPrematureExit) {
		t.Fatalf("doStart against a held port = %v; want ErrUpstreamPrematureExit", res.err)
	}
	if !pidAlive(orphan) {
		t.Fatal("plain doStart killed the orphan; reclamation must be the only thing that does")
	}

	res := p.startWithOrphanReclaim(ctx, testStartTimeout)
	if res.err != nil {
		t.Fatalf("startWithOrphanReclaim = %v; want a successful start after reclaiming the port", res.err)
	}
	t.Cleanup(func() {
		if res.cancel != nil {
			p.killProcess(res.cmd, res.cancel, res.cmdDone, 2*time.Second)
		}
	})
	if pidAlive(orphan) {
		t.Errorf("orphan pid %d survived the reclaim", orphan)
	}
}
