//go:build windows

package cdp

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

// The Windows transport is two named pipes standing in for what os.Pipe gives
// every other platform, so everything the rest of the package assumes about a
// pipe has to be checked here: that bytes flow the right way, that the parent end
// honours deadlines and cancel-on-close, that the child end reports EOF, and that
// exactly the child end is inheritable.

// Both directions must carry bytes the way their names say.
func TestPipePairRoundTrip(t *testing.T) {
	t.Run("parent writes", func(t *testing.T) {
		p, err := newPipePair(pipeParentWrites, true)
		if err != nil {
			t.Fatalf("newPipePair: %v", err)
		}
		defer p.close()
		if _, err := p.parent.Write([]byte("to-child\x00")); err != nil {
			t.Fatalf("parent write: %v", err)
		}
		buf := make([]byte, 9)
		if _, err := io.ReadFull(p.child, buf); err != nil {
			t.Fatalf("child read: %v", err)
		}
		if string(buf) != "to-child\x00" {
			t.Errorf("child read %q", buf)
		}
	})

	t.Run("parent reads", func(t *testing.T) {
		p, err := newPipePair(pipeParentReads, true)
		if err != nil {
			t.Fatalf("newPipePair: %v", err)
		}
		defer p.close()
		if _, err := p.child.Write([]byte("to-parent\x00")); err != nil {
			t.Fatalf("child write: %v", err)
		}
		buf := make([]byte, 10)
		if _, err := io.ReadFull(p.parent, buf); err != nil {
			t.Fatalf("parent read: %v", err)
		}
		if string(buf) != "to-parent\x00" {
			t.Errorf("parent read %q", buf)
		}
	})
}

// A deadline that expires must return os.ErrDeadlineExceeded and leave the pipe
// usable: this is the mechanism behind the transport's 10 s write-stall guard, so
// a one-shot deadline that poisons the pipe would be worse than none.
func TestPipeParentDeadlineThenNormalRead(t *testing.T) {
	p, err := newPipePair(pipeParentReads, true)
	if err != nil {
		t.Fatalf("newPipePair: %v", err)
	}
	defer p.close()

	if err := p.parent.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := p.parent.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read past the deadline = %v, want os.ErrDeadlineExceeded", err)
	}

	if err := p.parent.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}
	if _, err := p.child.Write([]byte("x")); err != nil {
		t.Fatalf("child write: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(p.parent, buf); err != nil {
		t.Fatalf("read after a deadline expiry: %v", err)
	}
	if buf[0] != 'x' {
		t.Errorf("read %q, want x", buf)
	}
}

// Closing the parent end must wake a goroutine already parked in Read. The read
// loop is torn down exactly this way, so without it teardown would leave a
// goroutine waiting for bytes that will never come.
func TestPipeParentCloseUnblocksRead(t *testing.T) {
	p, err := newPipePair(pipeParentReads, true)
	if err != nil {
		t.Fatalf("newPipePair: %v", err)
	}
	defer p.close()

	errc := make(chan error, 1)
	go func() {
		_, rerr := p.parent.Read(make([]byte, 1))
		errc <- rerr
	}()
	// Give the goroutine time to park in the read before closing under it.
	time.Sleep(100 * time.Millisecond)
	_ = p.parent.Close()

	select {
	case rerr := <-errc:
		if !errors.Is(rerr, os.ErrClosed) {
			t.Errorf("parked read after Close = %v, want os.ErrClosed", rerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wake a parked read; the parent end is not pollable")
	}
}

// When the child goes away the parent must see EOF. That is the transport's whole
// death signal: readLoop tears the connection down on it.
func TestPipeChildCloseGivesParentEOF(t *testing.T) {
	p, err := newPipePair(pipeParentReads, true)
	if err != nil {
		t.Fatalf("newPipePair: %v", err)
	}
	defer p.close()

	p.closeChild()
	if _, rerr := p.parent.Read(make([]byte, 1)); !errors.Is(rerr, io.EOF) {
		t.Errorf("parent read after the child closed = %v, want io.EOF", rerr)
	}
}

// Chromium checks GetFileType before adopting a handle, so a child end that is
// not FILE_TYPE_PIPE would be rejected outright.
func TestPipeChildIsAPipe(t *testing.T) {
	p, err := newPipePair(pipeParentWrites, false)
	if err != nil {
		t.Fatalf("newPipePair: %v", err)
	}
	defer p.close()

	const fileTypePipe = 0x0003
	got, err := syscall.GetFileType(syscall.Handle(p.child.Fd()))
	if err != nil {
		t.Fatalf("GetFileType: %v", err)
	}
	if got != fileTypePipe {
		t.Errorf("child GetFileType = %d, want FILE_TYPE_PIPE (%d)", got, fileTypePipe)
	}
}

// Exactly the child end is inheritable. An inheritable parent end would leak the
// other side of the transport into Chromium, which would then hold the pipe open
// and never see EOF when this process died.
func TestPipeInheritFlags(t *testing.T) {
	p, err := newPipePair(pipeParentWrites, false)
	if err != nil {
		t.Fatalf("newPipePair: %v", err)
	}
	defer p.close()

	childInherit, err := handleIsInheritable(syscall.Handle(p.child.Fd()))
	if err != nil {
		t.Fatalf("child handle info: %v", err)
	}
	if !childInherit {
		t.Error("the child end is not inheritable; Chromium would never receive it")
	}
	parentInherit, err := handleIsInheritable(syscall.Handle(handleOf(t, p.parent)))
	if err != nil {
		t.Fatalf("parent handle info: %v", err)
	}
	if parentInherit {
		t.Error("the parent end is inheritable; Chromium would hold the far side of its own transport open")
	}
}

// childPollable is what separates the transport's child end from a test's. The
// production end is synchronous by design, so it must refuse a deadline; asking
// for a pollable one must actually give a different handle.
func TestPipeChildPollability(t *testing.T) {
	blocking, err := newPipePair(pipeParentReads, false)
	if err != nil {
		t.Fatalf("newPipePair(false): %v", err)
	}
	defer blocking.close()
	if err := blocking.child.SetReadDeadline(time.Now().Add(time.Second)); err == nil {
		t.Error("a synchronous child end accepted a read deadline; childPollable=false did nothing")
	}

	pollable, err := newPipePair(pipeParentReads, true)
	if err != nil {
		t.Fatalf("newPipePair(true): %v", err)
	}
	defer pollable.close()
	if err := pollable.child.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Errorf("a pollable child end refused a read deadline: %v", err)
	}
}

// handleOf reads a file's underlying handle for an inspection that does not do
// I/O through it. It is used only on a parent end here, where Fd would otherwise
// be off limits: taking it drops the runtime poller association, so the pair is
// treated as spent afterwards and the test closes it immediately.
func handleOf(t *testing.T, f *os.File) uintptr {
	t.Helper()
	return f.Fd()
}
