package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/internal/minter"
	"github.com/spf13/cobra"
)

func TestCommandTree(t *testing.T) {
	root := newRootCmd()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, want := range []string{"server", "doctor", "get-pot", "ping"} {
		if !have[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

// TestGenerateRequiresBinding: the root (generate mode) with no -c prints "{}"
// and errors before ever launching a browser.
func TestGenerateRequiresBinding(t *testing.T) {
	root := newRootCmd()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{})
	if err := root.Execute(); err == nil {
		t.Error("expected an error when --content-binding is missing")
	}
	if out.String() != "{}\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "{}\n")
	}
}

func TestBuildLogger(t *testing.T) {
	if buildLogger("debug", &bytes.Buffer{}) == nil {
		t.Error("buildLogger returned nil")
	}
}

// runCLI executes a command and captures its output.
func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = execute(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestExecuteUsageErrors(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{"unknown root flag", []string{"--bogusflag"}, 2, "waxseal: "},
		{"unknown flag on ping", []string{"ping", "--port", "4420"}, 2, "waxseal: "},
		// A stray subcommand reaches the root NoArgs validator because the root has RunE.
		{"unknown subcommand", []string{"bogussubcmd"}, 2, "waxseal: "},
		{"missing video id", []string{"player-context"}, 2, "provide a video ID"},
		{"URL via --video", []string{"player-context", "--video", "https://youtu.be/x"}, 2, "not a URL"},
		{"URL positional", []string{"player-context", "https://youtu.be/x"}, 2, "not a URL"},
		// newRootCmd initializes Cobra's completion commands before wrapping validators.
		{"too many args to completion", []string{"completion", "bash", "extra"}, 2, "waxseal: "},
		// Help and completion reject bad input with exit 2 like the other commands.
		{"unknown help topic", []string{"help", "frobnicate"}, 2, "waxseal: "},
		{"help with a trailing word", []string{"help", "server", "extra"}, 2, "waxseal: "},
		{"unknown completion shell", []string{"completion", "not-a-shell"}, 2, "waxseal: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCLI(tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr=%q)", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

// TestHelpCompletionExitZero keeps valid help and completion invocations on
// exit 0 while invalid inputs use the normal usage-error path.
func TestHelpCompletionExitZero(t *testing.T) {
	for _, args := range [][]string{
		{"help"},               // root help
		{"help", "server"},     // help for a real subcommand
		{"completion"},         // runnable parent prints instructions
		{"completion", "bash"}, // a valid shell generates a script
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			code, _, stderr := runCLI(args...)
			if code != 0 {
				t.Errorf("exit = %d, want 0 (stderr=%q)", code, stderr)
			}
		})
	}
}

// A missing -c argument must preserve the bgutil failure response.
func TestExecuteMissingBinding(t *testing.T) {
	code, stdout, stderr := runCLI()
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if stdout != "{}\n" {
		t.Errorf("stdout = %q, want %q", stdout, "{}\n")
	}
	if !strings.Contains(stderr, "content-binding (-c) is required") {
		t.Errorf("stderr = %q, want the content-binding message", stderr)
	}
}

// Control characters in -c are rejected before the browser starts, and failures
// still return the bgutil empty-object response.
func TestGenerateRejectsControlChars(t *testing.T) {
	code, stdout, stderr := runCLI("-c", "a\x7fb") // a literal DEL byte
	if code != 2 {
		t.Errorf("exit = %d, want 2 (stderr=%q)", code, stderr)
	}
	if stdout != "{}\n" {
		t.Errorf("stdout = %q, want %q", stdout, "{}\n")
	}
	if !strings.Contains(stderr, "control characters") {
		t.Errorf("stderr = %q, want the control-characters message", stderr)
	}
}

func TestGetPotContentBindingTooLong(t *testing.T) {
	over := strings.Repeat("a", browser.MaxContentBindingBytes+1)
	code, stdout, stderr := runCLI("get-pot", "-c", over)
	if code != 2 {
		t.Errorf("exit = %d, want 2 (stderr=%q)", code, stderr)
	}
	if stdout != "{}\n" {
		t.Errorf("stdout = %q, want %q (bgutil contract)", stdout, "{}\n")
	}
	if !strings.Contains(stderr, "too long") {
		t.Errorf("stderr = %q, want it to mention the over-length binding", stderr)
	}
}

func TestBindListener(t *testing.T) {
	// Port 0 asks the operating system to assign an available port.
	ln, err := bindListener("127.0.0.1", 0)
	if err != nil {
		t.Fatalf("bindListener(0) error = %v, want nil", err)
	}
	defer ln.Close()
	if port := ln.Addr().(*net.TCPAddr).Port; port == 0 {
		t.Error("bindListener(0) returned port 0, want an OS-assigned port")
	}

	// Out-of-range ports are usage errors and do not bind a socket.
	for _, port := range []int{-1, 99999999} {
		l, err := bindListener("127.0.0.1", port)
		if l != nil {
			l.Close()
			t.Errorf("bindListener(%d) returned a listener, want nil", port)
		}
		if _, ok := errors.AsType[*usageError](err); !ok {
			t.Errorf("bindListener(%d) error = %v (%T), want *usageError", port, err, err)
		}
	}

	// An unavailable port is a runtime error, not a usage error.
	taken := ln.Addr().(*net.TCPAddr).Port
	if l, err := bindListener("127.0.0.1", taken); err == nil {
		l.Close()
		t.Fatal("bindListener on an in-use port = nil error, want an address-in-use error")
	} else if _, ok := errors.AsType[*usageError](err); ok {
		t.Errorf("in-use bind error is *usageError, want a runtime error (exit 1)")
	}
}

func TestBindListenerBracketedIPv6(t *testing.T) {
	l, err := bindListener("[::1]", 0)
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer l.Close()
	if ip := l.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Errorf("bindListener(\"[::1]\") bound %v, want an IPv6 loopback address", ip)
	}
}

func TestIsExposedHost(t *testing.T) {
	for _, h := range []string{"localhost", "127.0.0.1", "::1", "[::1]"} {
		if isExposedHost(h) {
			t.Errorf("isExposedHost(%q) = true, want false (loopback)", h)
		}
	}
	for _, h := range []string{"0.0.0.0", "::", "[::]", "192.168.1.5", "example.com", ""} {
		if !isExposedHost(h) {
			t.Errorf("isExposedHost(%q) = false, want true (exposed)", h)
		}
	}
}

// TestWarnKeylessExposure checks that the guest-identity warning is emitted only
// for keyless daemons bound to an exposed address.
func TestWarnKeylessExposure(t *testing.T) {
	cases := []struct {
		keyed    bool
		host     string
		wantWarn bool
	}{
		{false, "0.0.0.0", true},    // keyless and exposed
		{false, "::", true},         // keyless and exposed through IPv6 any
		{false, "127.0.0.1", false}, // loopback stays local
		{false, "localhost", false}, // loopback name stays local
		{true, "0.0.0.0", false},    // keys protect exposed hosts
	}
	for _, tt := range cases {
		var buf bytes.Buffer
		warnKeylessExposure(slog.New(slog.NewTextHandler(&buf, nil)), tt.keyed, tt.host)
		warned := strings.Contains(buf.String(), "keyless daemon exposes")
		if warned != tt.wantWarn {
			t.Errorf("warnKeylessExposure(keyed=%v, host=%q): warned=%v, want %v (log=%q)",
				tt.keyed, tt.host, warned, tt.wantWarn, buf.String())
		}
	}
}

func TestServerInvalidPortUsageError(t *testing.T) {
	// Ensure configuration parsing reaches bindListener regardless of the caller's
	// environment.
	t.Setenv("WAXSEAL_STREAMING_MAX_AGE", "")
	t.Setenv("WAXSEAL_REPORT_DEBOUNCE", "")
	code, _, stderr := runCLI("server", "--port", "99999999")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "invalid --port") {
		t.Errorf("stderr = %q, want it to mention the invalid port", stderr)
	}
}

// Invalid tenant-key configurations are usage errors. Error messages must not
// reveal API keys.
func TestServerInvalidTenantKeysUsageError(t *testing.T) {
	t.Setenv("WAXSEAL_STREAMING_MAX_AGE", "")
	t.Setenv("WAXSEAL_REPORT_DEBOUNCE", "")
	for _, tc := range []struct {
		name, keys, wantMsg string
	}{
		{"all empty keys", "alice=,bob=", "empty key"},
		{"dropped pair", "alice=KEYA, bob=", "empty key"},
		{"duplicate key", "alice=KEYA, bob=KEYA", "duplicate API key"},
		{"duplicate label", "alice=KEYA, alice=KEYB", "duplicate tenant label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLI("server", "--tenant-keys", tc.keys)
			if code != 2 {
				t.Errorf("exit = %d, want 2 (stderr=%q)", code, stderr)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantMsg)
			}
			if strings.Contains(stderr, "KEYA") || strings.Contains(stderr, "KEYB") {
				t.Errorf("stderr leaks key material: %q", stderr)
			}
		})
	}
}

// The metrics access flags parse on the server command.
func TestServerMetricsFlagsParse(t *testing.T) {
	c := newServerCmd()
	if err := c.ParseFlags([]string{"--metrics-public", "--metrics-key", "OPSKEY"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pub, _ := c.Flags().GetBool("metrics-public"); !pub {
		t.Error("--metrics-public did not parse to true")
	}
	if mk, _ := c.Flags().GetString("metrics-key"); mk != "OPSKEY" {
		t.Errorf("--metrics-key = %q, want OPSKEY", mk)
	}
}

// A --metrics-key equal to a tenant key is a usage error (exit 2). The message
// names the colliding tenant label and never leaks key material.
func TestServerMetricsKeyCollisionUsageError(t *testing.T) {
	t.Setenv("WAXSEAL_STREAMING_MAX_AGE", "")
	t.Setenv("WAXSEAL_REPORT_DEBOUNCE", "")
	code, _, stderr := runCLI("server", "--tenant-keys", "alice=KEYA,bob=KEYB", "--metrics-key", "KEYA")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "metrics key collides") {
		t.Errorf("stderr = %q, want it to mention the collision", stderr)
	}
	if !strings.Contains(stderr, "alice") {
		t.Errorf("stderr = %q, want it to name the colliding tenant label", stderr)
	}
	if strings.Contains(stderr, "KEYA") || strings.Contains(stderr, "KEYB") {
		t.Errorf("stderr leaks key material: %q", stderr)
	}
}

func TestValidateLandingVideo(t *testing.T) {
	for _, ok := range []string{browser.DefaultVideo, "aqz-KE-bpKQ", "abc123"} {
		if err := validateLandingVideo(ok); err != nil {
			t.Errorf("validateLandingVideo(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"https://youtu.be/x", "@@invalid@@", ""} {
		err := validateLandingVideo(bad)
		if err == nil {
			t.Errorf("validateLandingVideo(%q) = nil, want a usage error", bad)
		}
		if got := exitCodeFor(err); got != 2 {
			t.Errorf("validateLandingVideo(%q) exitCodeFor = %d, want 2", bad, got)
		}
	}
}

// Every command validates its landing video before launching Chromium.
func TestCommandsRejectInvalidLandingVideo(t *testing.T) {
	t.Setenv("WAXSEAL_STREAMING_MAX_AGE", "")
	t.Setenv("WAXSEAL_REPORT_DEBOUNCE", "")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"server", []string{"server", "--video", "@@invalid@@"}},
		{"doctor", []string{"doctor", "--video", "@@invalid@@"}},
		{"generate", []string{"get-pot", "-c", "vd", "--video", "@@invalid@@"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(tc.args...)
			if code != 2 {
				t.Errorf("exit = %d, want 2 (stderr=%q)", code, stderr)
			}
			if !strings.Contains(stderr, "video ID must contain") {
				t.Errorf("stderr = %q, want the invalid-video message", stderr)
			}
			if tc.name == "generate" && stdout != "{}\n" {
				t.Errorf("stdout = %q, want %q (bgutil contract)", stdout, "{}\n")
			}
		})
	}
}

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{&usageError{"bad"}, 2},
		{context.Canceled, 130},
		{browser.ErrUnplayable, 3},
		{&browser.UnplayableError{Status: "LOGIN_REQUIRED"}, 3},
		{errors.New("other"), 1},
	}
	for _, tt := range cases {
		if got := exitCodeFor(tt.err); got != tt.want {
			t.Errorf("exitCodeFor(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func TestRenderError(t *testing.T) {
	var b bytes.Buffer
	renderError(&b, errors.New("waxseal: boom"))
	if got := b.String(); got != "waxseal: boom\n" { // existing prefix is not duplicated
		t.Errorf("renderError = %q, want %q", got, "waxseal: boom\n")
	}
	b.Reset()
	renderError(&b, &usageError{"bad flag"})
	if got := b.String(); got != "waxseal: bad flag\n" {
		t.Errorf("renderError = %q, want %q", got, "waxseal: bad flag\n")
	}
	// Wrapped internal errors may carry the prefix after a stage name.
	b.Reset()
	renderError(&b, fmt.Errorf("player-context: %w", errors.New("waxseal: video unplayable")))
	if got := b.String(); got != "waxseal: player-context: video unplayable\n" {
		t.Errorf("renderError did not collapse the inner prefix: %q", got)
	}
	b.Reset()
	renderError(&b, nil)
	if b.Len() != 0 {
		t.Errorf("renderError(nil) wrote %q", b.String())
	}
}

func TestLooksLikeURL(t *testing.T) {
	for _, s := range []string{"http://youtube.com", "https://youtu.be/x", "ftp://h", "a://b"} {
		if !looksLikeURL(s) {
			t.Errorf("looksLikeURL(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"exampleVid1", "aqz-KE-bpKQ", "", "abc123"} {
		if looksLikeURL(s) {
			t.Errorf("looksLikeURL(%q) = true, want false", s)
		}
	}
}

// resolveSMA binds the flag before resolving it so Changed reflects command-line
// input.
func resolveSMA(t *testing.T, flagArgs ...string) (time.Duration, error) {
	t.Helper()
	var o serverOpts
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringVar(&o.streamingMaxAge, "streaming-max-age", "", "")
	if err := cmd.ParseFlags(flagArgs); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return resolveStreamingMaxAge(cmd, &o, slog.New(slog.DiscardHandler))
}

func TestResolveStreamingMaxAge(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		os.Unsetenv("WAXSEAL_STREAMING_MAX_AGE")
		d, err := resolveSMA(t)
		if err != nil || d != streamingMaxAgeDefault {
			t.Fatalf("default = (%v, %v), want %v", d, err, streamingMaxAgeDefault)
		}
	})
	t.Run("env overrides default", func(t *testing.T) {
		t.Setenv("WAXSEAL_STREAMING_MAX_AGE", "10m")
		d, err := resolveSMA(t)
		if err != nil || d != 10*time.Minute {
			t.Fatalf("env = (%v, %v), want 10m", d, err)
		}
	})
	t.Run("flag overrides env", func(t *testing.T) {
		t.Setenv("WAXSEAL_STREAMING_MAX_AGE", "10m")
		d, err := resolveSMA(t, "--streaming-max-age", "2m")
		if err != nil || d != 2*time.Minute {
			t.Fatalf("flag>env = (%v, %v), want 2m", d, err)
		}
	})
	t.Run("zero disables", func(t *testing.T) {
		os.Unsetenv("WAXSEAL_STREAMING_MAX_AGE")
		if d, err := resolveSMA(t, "--streaming-max-age", "0"); err != nil || d != 0 {
			t.Fatalf("0 = (%v, %v), want (0, nil)", d, err)
		}
	})
	t.Run("empty flag disables", func(t *testing.T) {
		os.Unsetenv("WAXSEAL_STREAMING_MAX_AGE")
		if d, err := resolveSMA(t, "--streaming-max-age", ""); err != nil || d != 0 {
			t.Fatalf("empty = (%v, %v), want (0, nil)", d, err)
		}
	})
	t.Run("floor is accepted", func(t *testing.T) {
		os.Unsetenv("WAXSEAL_STREAMING_MAX_AGE")
		if d, err := resolveSMA(t, "--streaming-max-age", "1m"); err != nil || d != time.Minute {
			t.Fatalf("1m = (%v, %v), want (1m, nil)", d, err)
		}
	})
	t.Run("large value warns but is accepted", func(t *testing.T) {
		os.Unsetenv("WAXSEAL_STREAMING_MAX_AGE")
		if d, err := resolveSMA(t, "--streaming-max-age", "5h"); err != nil || d != 5*time.Hour {
			t.Fatalf("5h = (%v, %v), want (5h, nil)", d, err)
		}
	})
	for _, bad := range []string{"abc", "-5m", "30s", "59s"} {
		t.Run("reject "+bad, func(t *testing.T) {
			os.Unsetenv("WAXSEAL_STREAMING_MAX_AGE")
			d, err := resolveSMA(t, "--streaming-max-age", bad)
			if err == nil {
				t.Fatalf("%q = (%v, nil), want an error", bad, d)
			}
			if got := exitCodeFor(err); got != 2 {
				t.Errorf("%q exitCodeFor = %d, want 2", bad, got)
			}
		})
	}
}

// resolveRD binds the flag before resolving it.
func resolveRD(t *testing.T, flagArgs ...string) (time.Duration, error) {
	t.Helper()
	var o serverOpts
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringVar(&o.reportDebounce, "report-debounce", "", "")
	if err := cmd.ParseFlags(flagArgs); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return resolveReportDebounce(cmd, &o, slog.New(slog.DiscardHandler))
}

func TestResolveReportDebounce(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		os.Unsetenv("WAXSEAL_REPORT_DEBOUNCE")
		d, err := resolveRD(t)
		if err != nil || d != minter.DefaultReportDebounce {
			t.Fatalf("default = (%v, %v), want %v", d, err, minter.DefaultReportDebounce)
		}
	})
	t.Run("empty resolves to default (not disabled)", func(t *testing.T) {
		os.Unsetenv("WAXSEAL_REPORT_DEBOUNCE")
		d, err := resolveRD(t, "--report-debounce", "")
		if err != nil || d != minter.DefaultReportDebounce {
			t.Fatalf("empty = (%v, %v), want default", d, err)
		}
	})
	t.Run("env overrides default", func(t *testing.T) {
		t.Setenv("WAXSEAL_REPORT_DEBOUNCE", "30s")
		d, err := resolveRD(t)
		if err != nil || d != 30*time.Second {
			t.Fatalf("env = (%v, %v), want 30s", d, err)
		}
	})
	t.Run("flag overrides env", func(t *testing.T) {
		t.Setenv("WAXSEAL_REPORT_DEBOUNCE", "30s")
		d, err := resolveRD(t, "--report-debounce", "10s")
		if err != nil || d != 10*time.Second {
			t.Fatalf("flag>env = (%v, %v), want 10s", d, err)
		}
	})
	t.Run("floor is accepted", func(t *testing.T) {
		os.Unsetenv("WAXSEAL_REPORT_DEBOUNCE")
		if d, err := resolveRD(t, "--report-debounce", reportDebounceFloor.String()); err != nil || d != reportDebounceFloor {
			t.Fatalf("floor = (%v, %v), want (%v, nil)", d, err, reportDebounceFloor)
		}
	})
	for _, bad := range []string{"abc", "0", "-5s", "1s", "4s"} {
		t.Run("reject "+bad, func(t *testing.T) {
			os.Unsetenv("WAXSEAL_REPORT_DEBOUNCE")
			d, err := resolveRD(t, "--report-debounce", bad)
			if err == nil {
				t.Fatalf("%q = (%v, nil), want an error below the minimum debounce", bad, d)
			}
			if got := exitCodeFor(err); got != 2 {
				t.Errorf("%q exitCodeFor = %d, want 2", bad, got)
			}
		})
	}
}
