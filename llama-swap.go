package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/menubar"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/shared"
	"github.com/mostlygeek/llama-swap/internal/watcher"
)

var (
	version = "0"
	commit  = "abcd1234"
	date    = "unknown"
)

const shutdownTimeout = 30 * time.Second

// logTimeFormats maps the cfg.LogTimeFormat value to a Go time layout. An
// unset or unrecognised value yields "" — no timestamp prefix.
var logTimeFormats = map[string]string{
	"ansic":       time.ANSIC,
	"unixdate":    time.UnixDate,
	"rubydate":    time.RubyDate,
	"rfc822":      time.RFC822,
	"rfc822z":     time.RFC822Z,
	"rfc850":      time.RFC850,
	"rfc1123":     time.RFC1123,
	"rfc1123z":    time.RFC1123Z,
	"rfc3339":     time.RFC3339,
	"rfc3339nano": time.RFC3339Nano,
	"kitchen":     time.Kitchen,
	"stamp":       time.Stamp,
	"stampmilli":  time.StampMilli,
	"stampmicro":  time.StampMicro,
	"stampnano":   time.StampNano,
}

func main() {
	flagConfig := flag.String("config", "", "path to config file (required)")
	flagListen := flag.String("listen", "", "listen address (default :8080 or :8443 for TLS)")
	flagCertFile := flag.String("tls-cert-file", "", "TLS certificate file")
	flagKeyFile := flag.String("tls-key-file", "", "TLS key file")
	flagVersion := flag.Bool("version", false, "show version and exit")
	flagWatchConfig := flag.Bool("watch-config", false, "reload config on file change")
	flagMenuBar := flag.Bool("menu-bar", false, "enable the menu-bar/system-tray helper")
	flag.Parse()

	if *flagVersion {
		fmt.Printf("version: %s (%s), built at %s\n", version, commit, date)
		os.Exit(0)
	}

	if *flagConfig == "" {
		slog.Error("-config is required")
		os.Exit(1)
	}

	useTLS := *flagCertFile != "" || *flagKeyFile != ""
	if (*flagCertFile != "" && *flagKeyFile == "") || (*flagCertFile == "" && *flagKeyFile != "") {
		slog.Error("both -tls-cert-file and -tls-key-file must be provided for TLS")
		os.Exit(1)
	}

	listenAddr := *flagListen
	if listenAddr == "" {
		if useTLS {
			listenAddr = ":8443"
		} else {
			listenAddr = ":8080"
		}
	}

	configPath := *flagConfig
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("failed to load config", "path", configPath, "error", err)
		os.Exit(1)
	}

	// Flag overrides config when both are present.
	if *flagMenuBar {
		cfg.MenuBar.Enabled = true
	}

	// Loggers are wired per cfg.LogToStdout: proxy/upstream feed muxLog, which
	// owns the combined history served by /logs. They outlive config reloads,
	// so a LogToStdout change requires a restart to take effect.
	muxLog, proxyLog, upstreamLog := server.NewLoggers(cfg.LogToStdout)

	if len(cfg.Profiles) > 0 {
		proxyLog.Warn("Profile functionality has been removed in favor of Groups. See the README for more information.")
	}

	applyLogSettings := func(cfg config.Config) {
		level := logmon.LevelInfo
		switch strings.ToLower(strings.TrimSpace(cfg.LogLevel)) {
		case "debug":
			level = logmon.LevelDebug
		case "warn":
			level = logmon.LevelWarn
		case "error":
			level = logmon.LevelError
		}
		timeFormat := logTimeFormats[strings.ToLower(strings.TrimSpace(cfg.LogTimeFormat))]
		for _, lg := range []*logmon.Monitor{proxyLog, upstreamLog} {
			lg.SetLogLevel(level)
			lg.SetLogTimeFormat(timeFormat)
		}
	}

	applyLogSettings(cfg)
	proxyLog.Debugf("PID: %d", os.Getpid())

	// On Windows, bind the process tree to a Job Object so every upstream
	// process is reaped when llama-swap exits — even on a forced kill. No-op
	// elsewhere. Non-fatal: a failure just falls back to per-process teardown.
	if err := process.SetupTreeCleanup(); err != nil {
		proxyLog.Warnf("failed to set up process tree cleanup: %v", err)
	}

	// perfMon outlives config reloads; its config is updated in place.
	var perfMon *perf.Monitor
	if !cfg.Performance.Disabled {
		perfMon, err = perf.New(cfg.Performance, proxyLog)
		if err != nil {
			slog.Error("failed to create performance monitor", "error", err)
			os.Exit(1)
		}
		perfMon.Start()
	} else {
		proxyLog.Info("performance monitoring is disabled")
	}

	buildInfo := server.BuildInfo{Version: version, Commit: commit, Date: date}

	initialSrv, err := server.New(cfg, muxLog, proxyLog, upstreamLog, perfMon, buildInfo)
	if err != nil {
		slog.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	// activeSrv is swapped atomically during hot reload.
	var activeMu sync.RWMutex
	activeSrv := initialSrv

	// The helper receives its settings via env vars at launch, so a changed
	// `menu_bar` section needs a helper relaunch; applyMenuBar reconciles the
	// running sidecar with the (re)loaded config and no-ops when unchanged.
	var menuMu sync.Mutex
	var menuBarLauncher *menubar.Launcher
	var menuBarCfg config.MenuBarConfig

	applyMenuBar := func(mc config.MenuBarConfig) {
		menuMu.Lock()
		defer menuMu.Unlock()
		if reflect.DeepEqual(mc, menuBarCfg) {
			return
		}
		if menuBarLauncher != nil {
			if err := menuBarLauncher.Stop(); err != nil {
				proxyLog.Warnf("menu-bar helper shutdown error: %v", err)
			}
			menuBarLauncher = nil
		}
		if mc.Enabled && menubar.Supported() {
			menuBarLauncher = menubar.New(proxyLog, menubar.Options{
				ListenAddr: listenAddr,
				TLS:        useTLS,
				Bars:       mc.Bars,
			})
			if err := menuBarLauncher.Start(); err != nil {
				proxyLog.Warnf("menu-bar helper failed to start: %v", err)
			}
		}
		menuBarCfg = mc
	}

	applyMenuBar(cfg.MenuBar)

	// The sidecar is already running by this point, but the listener is not yet
	// bound. Any exit between here and shutdown must stop it explicitly or it
	// outlives the proxy as an orphan that keeps drawing a menu-bar icon -
	// os.Exit runs neither deferred calls nor the signal handler below.
	stopMenuBar := func() {
		menuMu.Lock()
		defer menuMu.Unlock()
		if menuBarLauncher != nil {
			if err := menuBarLauncher.Stop(); err != nil {
				proxyLog.Warnf("menu-bar helper shutdown error: %v", err)
			}
		}
	}

	// Opt-in debug pprof listener, gated by LLAMA_SWAP_PPROF_PORT. Absent/empty
	// leaves this fully off with zero behavior change. Deliberately NOT
	// http.DefaultServeMux (that would also expose pprof on the main proxy
	// listener via the blank net/http/pprof import side effect) - a dedicated
	// mux bound to 127.0.0.1 keeps it off the network and off the proxy's
	// request path.
	if pprofPort := os.Getenv("LLAMA_SWAP_PPROF_PORT"); pprofPort != "" {
		pprofMux := http.NewServeMux()
		pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
		pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		pprofMux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
		pprofMux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		pprofMux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
		pprofMux.Handle("/debug/pprof/block", pprof.Handler("block"))
		pprofMux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))

		pprofAddr := "127.0.0.1:" + pprofPort
		go func() {
			proxyLog.Infof("debug pprof listener up on http://%s/debug/pprof/", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, pprofMux); err != nil {
				proxyLog.Warnf("debug pprof listener stopped: %v", err)
			}
		}()
	}

	mainHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeMu.RLock()
		srv := activeSrv
		activeMu.RUnlock()
		srv.ServeHTTP(w, r)
	})

	httpServer := &http.Server{
		Addr:    listenAddr,
		Handler: mainHandler,
	}

	// Extra tier listeners (docs/intent/llama-swap-tiers.md, llama-cm): each
	// configured tier gets its own http.Server sharing mainHandler, wrapped so
	// every request that arrives through it is tagged with the tier before it
	// ever reaches routing/scheduling. The main -listen port is always the
	// implicit default tier and needs no wrapping. Absent `tiers:` => this
	// loop is a no-op => byte-identical single-listener behavior.
	//
	// Fixed at startup, not reconciled on config reload/-watch-config: adding,
	// removing, or moving a tier port requires restarting llama-swap. Given
	// this fork's target here — a small number of long-lived local entry
	// points — that is an acceptable v1 scope limit, not a correctness gap.
	var tierServers []*http.Server
	tierNames := make([]string, 0, len(cfg.Tiers))
	for name := range cfg.Tiers {
		tierNames = append(tierNames, name)
	}
	sort.Strings(tierNames)
	for _, name := range tierNames {
		tc := cfg.Tiers[name]
		tier := shared.Tier{Name: name, Rank: tc.Rank, Preempts: tc.Preempts, Preemptible: tc.Preemptible}
		tierHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mainHandler.ServeHTTP(w, r.WithContext(shared.WithTier(r.Context(), tier)))
		})
		srv := &http.Server{Addr: tc.Listen, Handler: tierHandler}
		tierServers = append(tierServers, srv)

		go func(name string, srv *http.Server) {
			proxyLog.Infof("llama-swap tier %q listening on http://%s (rank %d)", name, srv.Addr, tier.Rank)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("tier http server error", "tier", name, "error", err)
				stopMenuBar()
				os.Exit(1)
			}
		}(name, srv)
	}

	// reload guards against overlapping reloads triggered by concurrent signals
	// or file-watcher callbacks.
	var reloading bool
	var reloadMu sync.Mutex

	reload := func() {
		reloadMu.Lock()
		if reloading {
			reloadMu.Unlock()
			return
		}
		reloading = true
		reloadMu.Unlock()
		defer func() {
			reloadMu.Lock()
			reloading = false
			reloadMu.Unlock()
		}()

		proxyLog.Info("reloading configuration")

		newCfg, err := config.LoadConfig(configPath)
		if err != nil {
			proxyLog.Warnf("failed to reload config: %v", err)
			return
		}

		if len(newCfg.Profiles) > 0 {
			proxyLog.Warn("Profile functionality has been removed in favor of Groups. See the README for more information.")
		}

		if perfMon != nil {
			perfMon.UpdateConfig(newCfg.Performance)
		}

		newSrv, err := server.New(newCfg, muxLog, proxyLog, upstreamLog, perfMon, buildInfo)
		if err != nil {
			proxyLog.Warnf("failed to build new server during reload: %v", err)
			return
		}

		activeMu.Lock()
		old := activeSrv
		activeSrv = newSrv
		activeMu.Unlock()

		applyLogSettings(newCfg)
		applyMenuBar(newCfg.MenuBar)

		if err := old.Shutdown(shutdownTimeout); err != nil {
			proxyLog.Warnf("error shutting down old server during reload: %v", err)
		}

		// Notify UI after a short delay so it can refresh model state.
		time.AfterFunc(3*time.Second, func() {
			event.Emit(shared.ConfigFileChangedEvent{State: shared.ReloadingStateEnd})
		})

		proxyLog.Info("configuration reloaded")
	}

	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	defer watcherCancel()

	if *flagWatchConfig {
		absConfigPath, err := filepath.Abs(configPath)
		if err != nil {
			slog.Error("watch-config: failed to resolve config path", "error", err)
			os.Exit(1)
		}
		proxyLog.Info("watching configuration for changes (poll-based, 2s interval)")
		go func() {
			(&configwatcher.Watcher{
				Path:     absConfigPath,
				Interval: configwatcher.DefaultInterval,
				OnChange: reload,
			}).Run(watcherCtx)
		}()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		var startErr error
		if useTLS {
			proxyLog.Infof("llama-swap listening with TLS on https://%s", listenAddr)
			startErr = httpServer.ListenAndServeTLS(*flagCertFile, *flagKeyFile)
		} else {
			proxyLog.Infof("llama-swap listening on http://%s", listenAddr)
			startErr = httpServer.ListenAndServe()
		}
		if startErr != nil && !errors.Is(startErr, http.ErrServerClosed) {
			slog.Error("http server error", "error", startErr)
			// A redundant llama-swap start fails here with EADDRINUSE; without
			// this the icon it just spawned would be stranded permanently.
			stopMenuBar()
			os.Exit(1)
		}
	}()

	if !shared.IsLoopbackAddr(listenAddr) {
		_, port, _ := net.SplitHostPort(listenAddr)
		proxyLog.Infof("llama-swap is reachable by all hosts on the network, use -listen localhost:%s to restrict to loopback only", port)
	}

	exitChan := make(chan struct{})

	go func() {
		for {
			sig := <-sigChan
			switch sig {
			case syscall.SIGHUP:
				proxyLog.Info("received SIGHUP, reloading config")
				go reload()
			case syscall.SIGINT, syscall.SIGTERM:
				proxyLog.Infof("received signal %v, shutting down", sig)
				watcherCancel()

				// Backstop against a stalled shutdown: force the process to
				// exit once the whole graceful sequence has had its full budget.
				// On Windows the Job Object reaps upstream processes on exit, so
				// a forced exit still cleans up rather than orphaning children.
				go func() {
					time.Sleep(shutdownTimeout + 5*time.Second)
					proxyLog.Warnf("graceful shutdown exceeded %v, forcing exit", shutdownTimeout)
					os.Exit(1)
				}()

				activeMu.RLock()
				srv := activeSrv
				activeMu.RUnlock()

				// Close long-lived SSE streams first so httpServer.Shutdown can
				// drain without blocking on them for the full timeout.
				srv.CloseStreams()

				// Both phases share a single deadline so total shutdown is
				// bounded by shutdownTimeout rather than 2x it.
				deadline := time.Now().Add(shutdownTimeout)
				shutdownCtx, cancel := context.WithDeadline(context.Background(), deadline)
				defer cancel()
				if err := httpServer.Shutdown(shutdownCtx); err != nil {
					proxyLog.Warnf("http server shutdown error: %v", err)
				}

				// Tier listeners share the same handler/deadline as the main
				// listener; shut them all down in parallel so N extra tiers
				// don't multiply the shutdown budget.
				var tierWG sync.WaitGroup
				for _, tsrv := range tierServers {
					tierWG.Add(1)
					go func(tsrv *http.Server) {
						defer tierWG.Done()
						if err := tsrv.Shutdown(shutdownCtx); err != nil {
							proxyLog.Warnf("tier http server shutdown error (%s): %v", tsrv.Addr, err)
						}
					}(tsrv)
				}
				tierWG.Wait()

				// Clamp the remaining budget to a small positive value: a
				// non-positive timeout makes the router fall back to its own
				// healthCheckTimeout, which would defeat the shared deadline.
				remaining := time.Until(deadline)
				if remaining <= 0 {
					remaining = time.Millisecond
				}
				if err := srv.Shutdown(remaining); err != nil {
					proxyLog.Warnf("router shutdown error: %v", err)
				}

				if perfMon != nil {
					perfMon.Stop()
				}

				stopMenuBar()

				close(exitChan)
				return
			}
		}
	}()

	<-exitChan
	proxyLog.Info("shutdown complete")
}
