//go:build e2e || live

package provider_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxseal/server"
)

// The in-process daemon the e2e and live tests start.

// waxsealE2ELogLevelEnv optionally raises the in-process daemon's log level from
// the default info to debug, which also prints the reduced SABR URLs logged
// around the status-2 confirm path.
const waxsealE2ELogLevelEnv = "WAXSEAL_E2E_LOG_LEVEL"

// testDaemonLogger routes the in-process daemon's logs, including the status-2
// confirm outcome, to "go test -v" output instead of the server's default
// discard logger. Every browser session the daemon launches shares it, so
// warm-up and every later request log here.
func testDaemonLogger(t *testing.T) *slog.Logger {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv(waxsealE2ELogLevelEnv), "debug") {
		level = slog.LevelDebug
	}
	w := &testLogWriter{t: t}
	t.Cleanup(w.close)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// testLogWriter is an io.Writer over t.Log, so each slog record becomes one
// test log line. Calling t.Log after the test returns panics. The daemon
// normally logs while the test waits on Warm or a request; close, registered
// with t.Cleanup, covers a goroutine that logs later: once closed is set, Write
// drops the record, and close waits for any Write already inside t.Log.
type testLogWriter struct {
	t *testing.T

	mu     sync.Mutex
	closed bool
}

func (w *testLogWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	// Hold the lock across t.Log, not only the check, so close cannot return
	// while this call is still on its way into t.Log.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return len(p), nil
	}
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// newInProcessDaemon binds a loopback listener and registers server cleanup. The
// listener comes back open for the caller to hand to serveDaemon after warming;
// its close is a cleanup here because a failed warm-up returns before Serve
// owns it. Holding the port from the start keeps anything else from taking it.
func newInProcessDaemon(t *testing.T, cfg server.Config) (*server.Server, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab free port: %v", err)
	}
	// Registered before the server's cleanup, so it runs after Shutdown; it is a
	// no-op unless Serve never ran.
	t.Cleanup(func() { _ = ln.Close() })

	cfg.Addr = ln.Addr().String()
	if cfg.Logger == nil {
		cfg.Logger = testDaemonLogger(t)
	}
	srv, err := server.NewWithContext(context.Background(), cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	})
	return srv, ln
}

// serveDaemon starts srv on ln and returns its base URL once it answers.
func serveDaemon(t *testing.T, srv *server.Server, ln net.Listener) string {
	t.Helper()
	go func() { _ = srv.Serve(ln) }()
	base := "http://" + ln.Addr().String()
	waitDaemonReady(t, base)
	return base
}

// waitDaemonReady waits for the server goroutine to start serving. Each attempt
// has its own timeout: the listener is already bound, so a request sent before
// Serve starts waits in the backlog instead of being refused, and without one
// the deadline below would never be reached.
func waitDaemonReady(t *testing.T, base string) {
	t.Helper()
	hc := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := hc.Get(base + "/metrics")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon at %s never became ready: %v", base, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
