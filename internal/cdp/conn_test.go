package cdp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// write must observe an already-canceled context and put no frame on the pipe,
// so a caller with a short deadline cannot park behind a stalled writer.
func TestConnWriteHonorsCanceledContext(t *testing.T) {
	c, reqR, _ := newPipeConn(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.write(ctx, append([]byte(`{"id":1}`), 0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("write err = %v, want context.Canceled", err)
	}
	// Nothing must have been written: a read times out rather than returning a frame.
	_ = reqR.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, err := reqR.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read after canceled write: n=%d err=%v, want 0 bytes and a deadline timeout", n, err)
	}
}

// After teardown, write must return ErrConnClosed even if select chooses the
// writeSem acquire, not a raw write-to-closed-pipe error.
func TestConnWriteAfterTeardownReturnsClosed(t *testing.T) {
	c, _, _ := newPipeConn(t)
	c.teardown(fmt.Errorf("%w: test", ErrConnClosed))
	for i := range 16 { // both closeCh and writeSem are ready, so exercise both outcomes
		if err := c.write(context.Background(), append([]byte("{}"), 0)); !errors.Is(err, ErrConnClosed) {
			t.Fatalf("iter %d: write err = %v, want ErrConnClosed", i, err)
		}
	}
}

// readFrame stops at the limit instead of buffering an unbounded frame.
func TestReadFrameRejectsOversize(t *testing.T) {
	r := bufio.NewReaderSize(bytes.NewReader(bytes.Repeat([]byte{'a'}, 1024)), 64) // no NUL within the limit
	if _, err := readFrame(r, 100); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("readFrame err = %v, want errFrameTooLarge", err)
	}
}

// readFrame returns the frame, including its trailing NUL, as a fresh slice.
func TestReadFrameReturnsFrame(t *testing.T) {
	got, err := readFrame(bufio.NewReaderSize(bytes.NewReader([]byte("hello\x00")), 64), 100)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(got) != "hello\x00" {
		t.Fatalf("got %q, want %q", got, "hello\x00")
	}
}

// teardown owns pipe closing for every path, so both parent-side ends are closed
// after it runs.
func TestConnTeardownClosesPipes(t *testing.T) {
	c, _, _ := newPipeConn(t)
	c.teardown(errors.New("boom"))
	if _, err := c.wpipe.Write([]byte{0}); !errors.Is(err, os.ErrClosed) {
		t.Errorf("wpipe write after teardown: err = %v, want os.ErrClosed", err)
	}
	if _, err := c.rpipe.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Errorf("rpipe read after teardown: err = %v, want os.ErrClosed", err)
	}
}

// newPipeConn wires a Conn to two os.Pipes so a test can act as the browser side:
// it reads outgoing requests from reqR and writes responses to respW.
func newPipeConn(t *testing.T) (c *Conn, reqR *os.File, respW *os.File) {
	t.Helper()
	// Both pairs are built through the transport's own constructor so these tests
	// run over the real thing on each platform, and both ask for a pollable child
	// end: here the test stands in for Chromium, and it needs deadlines on the end
	// Chromium would otherwise read and write synchronously.
	resp, err := newPipePair(pipeParentReads, true)
	if err != nil {
		t.Fatal(err)
	}
	req, err := newPipePair(pipeParentWrites, true)
	if err != nil {
		resp.close()
		t.Fatal(err)
	}
	c = newConn(nil, req.parent, resp.parent, slog.New(slog.DiscardHandler))
	go c.readLoop()
	t.Cleanup(func() { req.close(); resp.close() })
	return c, req.child, resp.child
}

// readRequestID reads one NUL-delimited request frame and returns its id.
func readRequestID(t *testing.T, r *bufio.Reader) int64 {
	t.Helper()
	frame, err := r.ReadBytes(0)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	frame = frame[:len(frame)-1] // strip NUL
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(frame, &req); err != nil {
		t.Fatalf("parse request %q: %v", frame, err)
	}
	return req.ID
}

// Protocol error data can be any JSON value. Object data must still complete the
// pending call with rpcError instead of dropping the frame and leaving the call
// open.
func TestConnErrorWithObjectDataCompletesCall(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)

	done := make(chan error, 1)
	go func() {
		_, err := c.rawCall(context.Background(), "", "Target.createBrowserContext", nil)
		done <- err
	}()

	id := readRequestID(t, br)
	resp := fmt.Sprintf(`{"id":%d,"error":{"code":-32000,"message":"boom","data":{"foo":"bar","n":1}}}`, id)
	if _, err := respW.Write(append([]byte(resp), 0)); err != nil {
		t.Fatalf("write response: %v", err)
	}

	select {
	case err := <-done:
		var re *rpcError
		if !errors.As(err, &re) {
			t.Fatalf("rawCall error = %v; want *rpcError", err)
		}
		if re.Code != -32000 || !strings.Contains(re.Message, "boom") {
			t.Errorf("rpcError = %+v; want code -32000 message containing boom", re)
		}
		if !strings.Contains(re.Error(), "foo") {
			t.Errorf("Error() = %q; want it to include the object data", re.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rawCall hung: an error frame with object data was dropped instead of completing the call")
	}
}

// String error data (the common case) must still round-trip.
func TestConnErrorWithStringDataCompletesCall(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)

	done := make(chan error, 1)
	go func() {
		_, err := c.rawCall(context.Background(), "", "Some.method", nil)
		done <- err
	}()

	id := readRequestID(t, br)
	resp := fmt.Sprintf(`{"id":%d,"error":{"code":-32000,"message":"nope","data":"detail text"}}`, id)
	if _, err := respW.Write(append([]byte(resp), 0)); err != nil {
		t.Fatalf("write response: %v", err)
	}

	select {
	case err := <-done:
		var re *rpcError
		if !errors.As(err, &re) {
			t.Fatalf("rawCall error = %v; want *rpcError", err)
		}
		if !strings.Contains(re.Error(), "detail text") {
			t.Errorf("Error() = %q; want it to include the string data", re.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rawCall hung on a string-data error frame")
	}
}

// A successful response must complete the call with the result bytes.
func TestConnResultCompletesCall(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)

	done := make(chan rpcResult, 1)
	go func() {
		raw, err := c.rawCall(context.Background(), "", "Browser.getVersion", nil)
		done <- rpcResult{result: raw, err: err}
	}()

	id := readRequestID(t, br)
	resp := fmt.Sprintf(`{"id":%d,"result":{"product":"Chrome/149"}}`, id)
	if _, err := respW.Write(append([]byte(resp), 0)); err != nil {
		t.Fatalf("write response: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("rawCall err = %v", r.err)
		}
		if !strings.Contains(string(r.result), "Chrome/149") {
			t.Errorf("result = %s; want it to carry the product", r.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rawCall hung on a normal result frame")
	}
}
