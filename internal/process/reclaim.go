package process

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrUpstreamPrematureExit marks a start that failed because the upstream
// command exited before it could serve a health check. It is a sentinel so the
// start path can tell this specific failure apart from every other start error
// and attempt orphan reclamation, which is only ever valid for this one.
var ErrUpstreamPrematureExit = errors.New("upstream command exited prematurely")

// reclaimSettleTimeout bounds how long the reclaim polls for a killed holder to
// release the port before giving up.
//
// WHY poll rather than kill and retry immediately: a SIGKILLed upstream holding
// tens of gigabytes does not release its listening socket the moment the signal
// lands, because the kernel reclaims its pages first. Retrying the start on a
// fixed delay would turn a deterministic failure into an intermittent one, which
// is harder to diagnose than the outage it replaces. The retry happens only once
// the port is observed free, and a holder that never releases it surfaces the
// original error instead.
const reclaimSettleTimeout = 20 * time.Second

// portHolder describes the process currently listening on an upstream port.
type portHolder struct {
	pid  int
	ppid int
	argv string
}

// startWithOrphanReclaim runs doStart and, when the upstream exited immediately
// because a leaked process from a previous llama-swap still holds this model's
// child port, reclaims that port and retries the start exactly once.
//
// The retry is deliberately single-shot: a kill that does not take, or a holder
// that is replaced by another, must surface today's error rather than spin.
func (p *ProcessCommand) startWithOrphanReclaim(startCtx context.Context, healthCheckTimeout time.Duration) startResult {
	res := p.doStart(startCtx, healthCheckTimeout)
	if !errors.Is(res.err, ErrUpstreamPrematureExit) {
		return res
	}
	if !p.reclaimOrphanedUpstream() {
		return res
	}
	return p.doStart(startCtx, healthCheckTimeout)
}

// reclaimOrphanedUpstream frees this model's configured child port when it is
// held by a leaked upstream from a dead llama-swap, and reports whether the port
// is now free to retry against.
//
// Killing another process is only safe under all three guards together:
//
//  1. the port is this model's OWN configured child port, so it belongs to
//     llama-swap by configuration rather than by discovery;
//  2. the holder is reparented to PID 1, so no live supervisor owns it. This is
//     the guard that protects a concurrently RUNNING isolated test box: it
//     shares the same child port range, but its children have a live parent and
//     therefore never match;
//  3. the holder's argv still carries the port llama-swap injected, so it looks
//     like an upstream this model would have spawned.
//
// Any guard failing means the port is held by something else, and the start
// falls through to the original error untouched.
//
// A leftover from a CRASHED test box does match, because it is a parentless
// upstream on this model's port and is indistinguishable from any other leak.
// That is INTENDED rather than an oversight: such a process is abandoned by
// definition, it is occupying a port this config owns, and leaving it in place
// would keep the outage this whole path exists to end. The reclaim is scoped by
// the configured port, never by process name, so it cannot become the
// blanket kill-by-name pattern this stack has removed elsewhere.
func (p *ProcessCommand) reclaimOrphanedUpstream() bool {
	port, err := upstreamPort(p.config.Proxy)
	if err != nil {
		return false
	}

	holder, err := findPortHolder(port)
	if err != nil {
		// Nothing is listening, so the premature exit had a different cause and
		// there is nothing to reclaim. Reported at debug: this is the ordinary
		// outcome for every start failure that is not an orphaned port.
		p.proxyLogger.Debugf("<%s> orphan-reclaim: no listener on port %d: %v", p.id, port, err)
		return false
	}

	if holder.ppid != 1 {
		p.proxyLogger.Warnf(
			"<%s> orphan-reclaim: DECLINED, port %d held by pid %d with live parent %d; not an orphan, leaving it alone",
			p.id, port, holder.pid, holder.ppid)
		return false
	}

	if !argvCarriesPort(holder.argv, port) {
		p.proxyLogger.Warnf(
			"<%s> orphan-reclaim: DECLINED, port %d held by orphan pid %d whose command does not look like this model's upstream: %s",
			p.id, port, holder.pid, holder.argv)
		return false
	}

	p.proxyLogger.Warnf(
		"<%s> orphan-reclaim: port %d held by orphaned upstream pid %d (parent died without stopping it); killing it to recover: %s",
		p.id, port, holder.pid, holder.argv)

	if err := killPID(holder.pid); err != nil {
		p.proxyLogger.Warnf("<%s> orphan-reclaim: FAILED to kill pid %d: %v", p.id, holder.pid, err)
		return false
	}

	if !waitPortFree(port, reclaimSettleTimeout) {
		p.proxyLogger.Warnf(
			"<%s> orphan-reclaim: FAILED, port %d still bound %v after killing pid %d",
			p.id, port, reclaimSettleTimeout, holder.pid)
		return false
	}

	p.proxyLogger.Warnf(
		"<%s> orphan-reclaim: RECOVERED port %d from orphaned pid %d; retrying start once",
		p.id, port, holder.pid)
	return true
}

// upstreamPort extracts the TCP port from a model's proxy URL.
func upstreamPort(proxy string) (int, error) {
	u, err := url.Parse(strings.TrimSpace(proxy))
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 {
		return 0, fmt.Errorf("no usable port in proxy %q", proxy)
	}
	return port, nil
}

// argvCarriesPort reports whether argv mentions port as a standalone token.
//
// It matches the port rather than the configured executable on purpose: a model
// command is commonly a wrapper script that execs the real server, and exec
// replaces argv, so comparing against the configured command would never match a
// running upstream. What survives the exec is the port llama-swap substituted
// into the command via ${PORT}.
func argvCarriesPort(argv string, port int) bool {
	want := strconv.Itoa(port)
	for _, field := range strings.FieldsFunc(argv, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '=' || r == ':'
	}) {
		if field == want {
			return true
		}
	}
	return false
}

// waitPortFree polls until nothing is listening on port, or timeout elapses.
func waitPortFree(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			ln.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}
