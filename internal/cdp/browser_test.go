package cdp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// readRequest reads one NUL-delimited request frame and returns its id, method,
// and raw params. readRequestID reads only the id, which cannot tell one call in
// a sequence from the next.
func readRequest(t *testing.T, r *bufio.Reader, src *os.File) (id int64, method string, params json.RawMessage) {
	t.Helper()
	// The child end is pollable, so a frame that never arrives fails the test
	// instead of hanging the package; an unsent frame is the regression here.
	if err := src.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	frame, err := r.ReadBytes(0)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	var req struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame[:len(frame)-1], &req); err != nil {
		t.Fatalf("parse request %q: %v", frame, err)
	}
	return req.ID, req.Method, req.Params
}

// writeResponse answers one request frame.
func writeResponse(t *testing.T, w *os.File, frame string) {
	t.Helper()
	if _, err := w.Write(append([]byte(frame), 0)); err != nil {
		t.Fatalf("write response: %v", err)
	}
}

// A target created but never handed back has to be closed, or its renderer runs
// for the browser's lifetime, invisible to every caller.
func TestPageClosesTheTargetWhenAttachFails(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)
	b := &Browser{conn: c, ctx: context.Background()}

	done := make(chan error, 1)
	go func() {
		_, err := b.Page(TargetCreateTarget{URL: "about:blank"})
		done <- err
	}()

	id, method, _ := readRequest(t, br, reqR)
	if method != "Target.createTarget" {
		t.Fatalf("first call = %q, want Target.createTarget", method)
	}
	writeResponse(t, respW, fmt.Sprintf(`{"id":%d,"result":{"targetId":"T1"}}`, id))

	id, method, _ = readRequest(t, br, reqR)
	if method != "Target.attachToTarget" {
		t.Fatalf("second call = %q, want Target.attachToTarget", method)
	}
	writeResponse(t, respW, fmt.Sprintf(`{"id":%d,"error":{"code":-32000,"message":"no such target"}}`, id))

	id, method, params := readRequest(t, br, reqR)
	if method != "Target.closeTarget" {
		t.Fatalf("third call = %q, want Target.closeTarget: the created target leaked", method)
	}
	var got closeTargetParams
	if err := json.Unmarshal(params, &got); err != nil {
		t.Fatalf("parse closeTarget params: %v", err)
	}
	if got.TargetID != "T1" {
		t.Errorf("closeTarget targetId = %q, want the created T1", got.TargetID)
	}
	writeResponse(t, respW, fmt.Sprintf(`{"id":%d,"result":{"success":true}}`, id))

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "attach target") {
			t.Fatalf("Page error = %v, want the attach failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Page hung after the attach failure")
	}
}

// runIncognito runs Incognito on a root Browser over c, answers
// Target.createBrowserContext with result, and returns what Incognito did.
func runIncognito(t *testing.T, c *Conn, br *bufio.Reader, reqR, respW *os.File, result string) (root, incog *Browser, err error) {
	t.Helper()
	root = &Browser{conn: c, ctx: context.Background()}
	done := make(chan error, 1)
	go func() {
		var ierr error
		incog, ierr = root.Incognito()
		done <- ierr
	}()
	rid, method, _ := readRequest(t, br, reqR)
	if method != "Target.createBrowserContext" {
		t.Fatalf("call = %q, want Target.createBrowserContext", method)
	}
	writeResponse(t, respW, fmt.Sprintf(`{"id":%d,"result":%s}`, rid, result))
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Incognito hung after its answer")
	}
	return root, incog, err
}

// newIncognito is runIncognito answered with context id; it must succeed.
func newIncognito(t *testing.T, c *Conn, br *bufio.Reader, reqR, respW *os.File, id string) (root, incog *Browser) {
	t.Helper()
	root, incog, err := runIncognito(t, c, br, reqR, respW, fmt.Sprintf(`{"browserContextId":%q}`, id))
	if err != nil {
		t.Fatalf("Incognito: %v", err)
	}
	return root, incog
}

// answerDispose reads the next request, requires it to dispose context id, and
// answers it with reply, a "result" or "error" member.
func answerDispose(t *testing.T, br *bufio.Reader, reqR, respW *os.File, id, reply string) {
	t.Helper()
	rid, method, params := readRequest(t, br, reqR)
	if method != "Target.disposeBrowserContext" {
		t.Fatalf("call = %q, want Target.disposeBrowserContext", method)
	}
	var got disposeBrowserContextParams
	if err := json.Unmarshal(params, &got); err != nil {
		t.Fatalf("parse dispose params: %v", err)
	}
	if got.BrowserContextID != id {
		t.Errorf("dispose browserContextId = %q, want %q", got.BrowserContextID, id)
	}
	writeResponse(t, respW, fmt.Sprintf(`{"id":%d,%s}`, rid, reply))
}

// goClose runs b.Close on its own goroutine, since a Close that sends the
// dispose blocks until the test answers it.
func goClose(b *Browser) <-chan error {
	done := make(chan error, 1)
	go func() { done <- b.Close() }()
	return done
}

// waitClose returns the result of a Close started by goClose. A Close that
// hangs is waiting on a reply the test never sends, so the failure names the
// request it sent.
func waitClose(t *testing.T, done <-chan error, br *bufio.Reader, reqR *os.File) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
	}
	_, method, _ := readRequest(t, br, reqR)
	t.Fatalf("Close did not return: it is waiting on a reply to %s", method)
	return nil
}

// requireNothingSent proves no request was sent since the last one read: a
// Browser.getVersion sent now through root must be the next frame.
func requireNothingSent(t *testing.T, root *Browser, br *bufio.Reader, reqR, respW *os.File) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := root.Version()
		done <- err
	}()
	rid, method, _ := readRequest(t, br, reqR)
	if method != "Browser.getVersion" {
		t.Fatalf("next call = %q, want the Browser.getVersion probe", method)
	}
	writeResponse(t, respW, fmt.Sprintf(`{"id":%d,"result":{}}`, rid))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("probe Version: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probe Version hung after its answer")
	}
}

// A second Close on an incognito copy must not re-send the dispose for a
// context Chromium has already dropped.
func TestIncognitoCloseTwiceDisposesOnce(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)
	root, incog := newIncognito(t, c, br, reqR, respW, "C1")

	done := goClose(incog)
	answerDispose(t, br, reqR, respW, "C1", `"result":{}`)
	if err := waitClose(t, done, br, reqR); err != nil {
		t.Fatalf("first Close = %v, want nil", err)
	}
	if err := waitClose(t, goClose(incog), br, reqR); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	requireNothingSent(t, root, br, reqR, respW)
}

// Context copies share the dispose, including one made before the Close, so
// closing a copy after the original sends nothing.
func TestIncognitoContextCopiesShareTheDispose(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)
	root, incog := newIncognito(t, c, br, reqR, respW, "C1")
	before := incog.Context(context.Background())

	done := goClose(incog)
	answerDispose(t, br, reqR, respW, "C1", `"result":{}`)
	if err := waitClose(t, done, br, reqR); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	for _, cp := range []*Browser{before, incog.Context(context.Background())} {
		if err := waitClose(t, goClose(cp), br, reqR); err != nil {
			t.Fatalf("copy Close = %v, want nil", err)
		}
	}
	requireNothingSent(t, root, br, reqR, respW)
}

// Concurrent Closes across copies run one at a time, so a successful dispose is
// sent once and every Close returns nil.
func TestIncognitoConcurrentClosesDisposeOnce(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)
	root, incog := newIncognito(t, c, br, reqR, respW, "C1")

	dones := make([]<-chan error, 8)
	for i := range dones {
		b := incog
		if i%2 == 1 {
			b = incog.Context(context.Background())
		}
		dones[i] = goClose(b)
	}
	answerDispose(t, br, reqR, respW, "C1", `"result":{}`)
	for _, done := range dones {
		if err := waitClose(t, done, br, reqR); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
	}
	requireNothingSent(t, root, br, reqR, respW)
}

// A failed dispose leaves the context undisposed, whether or not its frame went
// out, so the next Close retries it; the first success is final.
func TestIncognitoFailedDisposeIsRetried(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)
	root, incog := newIncognito(t, c, br, reqR, respW, "C1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := incog.Context(ctx).Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with a canceled ctx = %v, want context.Canceled", err)
	}
	// A dispose must be the next frame; a read timeout here means none was sent.
	done := goClose(incog)
	answerDispose(t, br, reqR, respW, "C1", `"error":{"code":-32000,"message":"boom"}`)
	var re *rpcError
	if err := waitClose(t, done, br, reqR); !errors.As(err, &re) {
		t.Fatalf("Close = %v, want the dispose's rpc error", err)
	}
	done = goClose(incog)
	answerDispose(t, br, reqR, respW, "C1", `"result":{}`)
	if err := waitClose(t, done, br, reqR); err != nil {
		t.Fatalf("retried Close = %v, want nil", err)
	}
	if err := waitClose(t, goClose(incog), br, reqR); err != nil {
		t.Fatalf("Close after the success = %v, want nil", err)
	}
	requireNothingSent(t, root, br, reqR, respW)
}

// Incognito must reject a reply without a context id: a copy with an empty
// contextID would take Close's root path and tear down the shared browser.
func TestIncognitoRejectsEmptyContextID(t *testing.T) {
	for _, tc := range []struct{ name, result string }{
		{"missing", `{}`},
		{"empty", `{"browserContextId":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, reqR, respW := newPipeConn(t)
			br := bufio.NewReader(reqR)
			_, incog, err := runIncognito(t, c, br, reqR, respW, tc.result)
			if err == nil {
				t.Fatalf("Incognito succeeded on %s, want an error", tc.result)
			}
			if incog != nil {
				t.Errorf("Incognito returned a Browser along with %v", err)
			}
		})
	}
}
