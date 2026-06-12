package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/colespringer/waxseal/internal/browser"
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
		{"missing --video", []string{"player-context"}, 2, "--video"},
		{"URL where id expected", []string{"player-context", "--video", "https://youtu.be/x"}, 2, "not a URL"},
		// newRootCmd initializes Cobra's completion commands before wrapping validators.
		{"too many args to completion", []string{"completion", "bash", "extra"}, 2, "waxseal: "},
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

// TestExecuteMissingBinding verifies the bgutil failure response without launching
// a browser.
func TestExecuteMissingBinding(t *testing.T) {
	code, stdout, stderr := runCLI()
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stdout != "{}\n" {
		t.Errorf("stdout = %q, want %q", stdout, "{}\n")
	}
	if !strings.Contains(stderr, "content-binding (-c) is required") {
		t.Errorf("stderr = %q, want the content-binding message", stderr)
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
	for _, s := range []string{"dQw4w9WgXcQ", "aqz-KE-bpKQ", "", "abc123"} {
		if looksLikeURL(s) {
			t.Errorf("looksLikeURL(%q) = true, want false", s)
		}
	}
}
