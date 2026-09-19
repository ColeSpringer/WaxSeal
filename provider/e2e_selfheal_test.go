//go:build e2e && unix

package provider_test

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/server"
)

// recoveryBudget leaves enough time for relaunch, attestation, and establishment.
// The server applies its own three-minute request timeout.
const recoveryBudget = 4 * time.Minute

// TestBrowserProcessSelfHealHTTP verifies recovery after the pooled Chromium
// process is killed. It targets the pool's process ID so unrelated browser
// processes are not affected.
func TestBrowserProcessSelfHealHTTP(t *testing.T) {
	if ext := os.Getenv("WAXSEAL_URL"); ext != "" {
		t.Skip("browser recovery test requires an in-process daemon")
	}
	srv, addr, ln := newInProcessDaemon(t, server.Config{})
	warmCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	if err := srv.Warm(warmCtx, ""); err != nil {
		cancel()
		t.Fatalf("warm: %v", err)
	}
	cancel()
	go func() { _ = srv.Serve(ln) }()
	base := "http://" + addr
	waitDaemonReady(t, base)

	c := client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY")))

	// Prime a token that should remain cached after the browser exits.
	primeCtx, cancelP := context.WithTimeout(context.Background(), 60*time.Second)
	tok1, err := c.POToken(primeCtx, bbbVideoID, "player")
	cancelP()
	if err != nil {
		t.Fatalf("prime token: %v", err)
	}

	genBefore := readEscalationMetrics(t, base).Generation
	pid := srv.BrowserPID()
	if pid <= 0 {
		t.Fatalf("BrowserPID() = %d, want a live PID", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL %d: %v", pid, err)
	}
	t.Logf("killed pooled Chromium PID %d", pid)

	// Cached tokens do not depend on the live browser process.
	cacheCtx, cancelC := context.WithTimeout(context.Background(), 30*time.Second)
	tok2, err := c.POToken(cacheCtx, bbbVideoID, "player")
	cancelC()
	if err != nil {
		t.Fatalf("cached get_pot after kill: %v", err)
	}
	if tok2.Value != tok1.Value {
		t.Errorf("cached token changed after the browser died; the cache did not survive")
	}

	// A browser-backed endpoint triggers relaunch and attestation.
	pcCtx, cancelPC := context.WithTimeout(context.Background(), recoveryBudget)
	pc, err := c.PlayerContext(pcCtx, bbbVideoID)
	cancelPC()
	if err != nil {
		t.Fatalf("player-context did not recover after the browser died: %v", err)
	}
	if pc.PlayabilityStatus != "OK" {
		t.Errorf("recovered player-context playability_status = %q, want OK", pc.PlayabilityStatus)
	}

	// Recovery creates a new session generation.
	if genAfter := readEscalationMetrics(t, base).Generation; genAfter <= genBefore {
		t.Errorf("generation did not increment after browser recovery: before=%d after=%d", genBefore, genAfter)
	}

	// The replacement session can also be exported.
	sessCtx, cancelS := context.WithTimeout(context.Background(), recoveryBudget)
	sess, err := c.Session(sessCtx)
	cancelS()
	if err != nil {
		t.Fatalf("session did not recover after the browser died: %v", err)
	}
	if sess.VisitorData == "" {
		t.Errorf("recovered session has empty visitor_data")
	}
}
