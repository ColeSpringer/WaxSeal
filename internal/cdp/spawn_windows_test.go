//go:build windows

package cdp

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"unsafe"
)

// newTestCmd is an unstarted command attach can decorate. The binary is never
// run, so its name only has to be non-empty.
func newTestCmd() *exec.Cmd { return exec.Command("chrome.exe", "--headless=new") }

// assertSpawnArgv checks what the helper was launched with. The spawn path
// appends two flags after BuildArgs runs, so TestArgvGolden never sees them and
// this is the only check they are there. The helper answering over the handles
// already proved they work; what is left is the shape.
func assertSpawnArgv(t *testing.T, argv []string, _ *Browser) {
	t.Helper()
	flag, ok := helperArgvContains(argv, ioPipesFlagPrefix)
	if !ok {
		t.Fatalf("helper argv = %s, want a %s flag", marshalIndent(argv), ioPipesFlagPrefix)
	}
	values := strings.TrimPrefix(flag, ioPipesFlagPrefix)
	in, out, split := strings.Cut(values, ",")
	if !split || in == "" || out == "" {
		t.Errorf("%s = %q, want two comma-separated handle values", ioPipesFlagPrefix, values)
	}
	if in == out {
		t.Errorf("%s = %q: the command and event ends must be different handles", ioPipesFlagPrefix, values)
	}
	if _, ok := helperArgvContains(argv, "--enable-logging=stderr"); !ok {
		t.Errorf("helper argv = %s, want --enable-logging=stderr; without it a failed handshake has no stderr tail", marshalIndent(argv))
	}
	// Exactly one, so a second spawn through the same cmd could not have appended
	// a stale pair the child might pick up instead.
	n := 0
	for _, a := range argv {
		if strings.HasPrefix(a, ioPipesFlagPrefix) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("helper argv carries %d %s flags, want 1", n, ioPipesFlagPrefix)
	}
}

// A hand-rolled Win32 layout is where a port breaks silently: a wrong size makes
// SetInformationJobObject fail, the guard degrade to a plain kill, and nothing
// else go wrong until a crashed daemon leaves Chromium behind. 144 bytes is
// JOB_OBJECT_EXTENDED_LIMIT_INFORMATION on 64-bit Windows.
func TestJobObjectExtendedLimitInfoSize(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the pinned size is the 64-bit layout")
	}
	if got := unsafe.Sizeof(jobObjectExtendedLimitInfo{}); got != 144 {
		t.Errorf("sizeof(JOB_OBJECT_EXTENDED_LIMIT_INFORMATION) = %d, want 144", got)
	}
}

// attach must name the handles it actually created and mark them inherited, since
// nothing else connects the argv values to the pipes.
func TestProcGuardAttachNamesTheChildHandles(t *testing.T) {
	cmdPipe, err := newPipePair(pipeParentWrites, false)
	if err != nil {
		t.Fatalf("command pipe: %v", err)
	}
	defer cmdPipe.close()
	evtPipe, err := newPipePair(pipeParentReads, false)
	if err != nil {
		t.Fatalf("event pipe: %v", err)
	}
	defer evtPipe.close()

	cmd := newTestCmd()
	g := newProcGuard(nil)
	g.attach(cmd, cmdPipe, evtPipe)
	defer g.release()

	wantIn, wantOut := uint32(cmdPipe.child.Fd()), uint32(evtPipe.child.Fd())
	wantFlag := fmt.Sprintf("%s%d,%d", ioPipesFlagPrefix, wantIn, wantOut)
	if _, ok := helperArgvContains(cmd.Args, wantFlag); !ok {
		t.Errorf("cmd.Args = %s, want %q", marshalIndent(cmd.Args), wantFlag)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("attach set no SysProcAttr")
	}
	got := cmd.SysProcAttr.AdditionalInheritedHandles
	if len(got) != 2 || uint32(got[0]) != wantIn || uint32(got[1]) != wantOut {
		t.Errorf("AdditionalInheritedHandles = %v, want [%d %d]", got, wantIn, wantOut)
	}
}
