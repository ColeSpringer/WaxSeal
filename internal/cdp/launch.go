package cdp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"time"
)

// defaultLaunchTimeout bounds the Browser.getVersion handshake.
const defaultLaunchTimeout = 60 * time.Second

// waitDelay bounds how long cmd.Wait blocks on the stderr-copy goroutine after the
// process has exited, so a lingering child holding fd 2 cannot block the reaper.
// It doubles as waitExited's budget: it is the longest a reap can legitimately
// take once the process itself is gone.
const waitDelay = 5 * time.Second

// SpawnOptions configures Spawn.
type SpawnOptions struct {
	LaunchTimeout time.Duration // version-handshake budget; 0 uses defaultLaunchTimeout
	StderrMax     int           // crash-diagnostics ring size; 0 uses defaultStderrMax
	Logger        *slog.Logger  // nil discards
}

// baseArgs are the Chromium flags WaxSeal launches with; --enable-automation
// is not among them. Several affect fingerprinting and process cleanup, so
// TestArgvGolden pins the whole BuildArgs argv to testdata/argv_baseline.txt,
// with remote-debugging-port swapped for remote-debugging-pipe.
var baseArgs = []string{
	"--remote-debugging-pipe",
	"--no-sandbox",
	"--disable-dev-shm-usage",
	"--disable-gpu",
	"--mute-audio",
	"--disable-blink-features=AutomationControlled",
	"--no-first-run",
	"--no-startup-window",
	"--disable-features=site-per-process,TranslateUI",
	"--disable-background-networking",
	"--disable-background-timer-throttling",
	"--disable-backgrounding-occluded-windows",
	"--disable-breakpad",
	"--disable-client-side-phishing-detection",
	"--disable-component-extensions-with-background-pages",
	"--disable-default-apps",
	"--disable-hang-monitor",
	"--disable-ipc-flooding-protection",
	"--disable-popup-blocking",
	"--disable-prompt-on-repost",
	"--disable-renderer-backgrounding",
	"--disable-sync",
	"--disable-site-isolation-trials",
	"--enable-features=NetworkService,NetworkServiceInProcess",
	"--force-color-profile=srgb",
	"--metrics-recording-only",
	"--use-mock-keychain",
}

// BuildArgs returns the Chromium argv for a profile directory, sorted so the
// order of baseArgs does not matter. headful drops --headless=new.
func BuildArgs(profileDir string, headful bool) []string {
	args := make([]string, 0, len(baseArgs)+2)
	args = append(args, baseArgs...)
	args = append(args, "--user-data-dir="+profileDir)
	if !headful {
		args = append(args, "--headless=new")
	}
	sort.Strings(args)
	return args
}

// Spawn launches bin with args over a remote-debugging pipe and completes the
// Browser.getVersion handshake. On any failure it kills the process group and
// closes the pipes, so failed starts do not leave the launched process running.
// The caller owns Browser.Close.
func Spawn(ctx context.Context, bin string, args []string, opts SpawnOptions) (*Browser, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.LaunchTimeout <= 0 {
		opts.LaunchTimeout = defaultLaunchTimeout
	}
	stderrMax := opts.StderrMax
	if stderrMax <= 0 {
		stderrMax = defaultStderrMax
	}

	// The command pipe carries commands to Chromium; the event pipe carries
	// responses and events back. Building the pairs and handing them to
	// Chromium is platform code (newPlatformPipePair, procGuard.attach).
	cmdPipe, err := newPipePair(pipeParentWrites, false)
	if err != nil {
		return nil, fmt.Errorf("cdp: command pipe: %w", err)
	}
	evtPipe, err := newPipePair(pipeParentReads, false)
	if err != nil {
		cmdPipe.close()
		return nil, fmt.Errorf("cdp: event pipe: %w", err)
	}

	cmd := exec.Command(bin, args...)
	stderr := &ringBuffer{max: stderrMax}
	cmd.Stderr = stderr
	// A ringBuffer is not an *os.File, so Wait joins a stderr copy goroutine;
	// see waitDelay.
	cmd.WaitDelay = waitDelay
	guard := newProcGuard(opts.Logger)
	guard.attach(cmd, cmdPipe, evtPipe)

	if err := cmd.Start(); err != nil {
		cmdPipe.close()
		evtPipe.close()
		guard.release()
		return nil, fmt.Errorf("cdp: start chromium: %w", err)
	}
	guard.started(cmd)
	// Chromium has inherited the child ends, so drop the parent's copies (see
	// pipePair and closeChild).
	cmdPipe.closeChild()
	evtPipe.closeChild()

	c := newConn(cmd, cmdPipe.parent, evtPipe.parent, opts.Logger)
	c.guard = guard
	go c.readLoop()
	go func() {
		// Reap the process and signal exit even if the read loop has not yet seen
		// EOF (e.g. the process was group-killed). Record the reap before tearing
		// down so killProcess never signals a PID the OS may have recycled.
		_ = cmd.Wait()
		c.procExited.Store(true)
		guard.release()
		close(c.exited)
		c.teardown(fmt.Errorf("%w: process exited", ErrConnClosed))
	}()

	b := &Browser{conn: c, ctx: context.Background()}
	hctx, cancel := context.WithTimeout(ctx, opts.LaunchTimeout)
	defer cancel()
	if _, err := b.Context(hctx).Version(); err != nil {
		c.forceClose(fmt.Errorf("version handshake: %w", err))
		// The caller removes the profile next, so wait for the reap (see
		// waitExited). The wait ignores ctx: an expired caller deadline is one
		// way the handshake fails, and it must not turn the wait into a no-op.
		if !c.waitExited() {
			opts.Logger.Warn("cdp: chromium was not reaped after a failed handshake; the profile may not remove cleanly",
				"budget", waitDelay, "pid", c.pid())
		}
		// The read loop (EOF), the reaper, and a write into a closed pipe race
		// to record a death, so name it by its exit, not whichever wording won.
		// procExited alone cannot tell a death from the forceClose kill above,
		// so only a lost connection counts; a protocol error, timeout, or
		// cancellation keeps its own wording.
		if c.procExited.Load() && errors.Is(err, ErrConnClosed) &&
			!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			err = fmt.Errorf("chromium exited during the version handshake (%s): %w", c.exitStatus(), err)
		}
		return nil, fmt.Errorf("cdp: launch handshake: %w (stderr tail: %q)", err, tail(stderr.String(), 600))
	}
	return b, nil
}

// tail returns the last n bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
