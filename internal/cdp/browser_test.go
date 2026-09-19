package cdp

import (
	"bufio"
	"context"
	"encoding/json"
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
	// The child end is pollable, so a frame that never arrives fails this test
	// rather than hanging until the whole package times out. That matters here:
	// the regression this test exists to catch is a frame that is never sent.
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

// A target created but never handed back has to be closed. Chromium keeps a
// renderer alive for every open target, so an attach that fails would otherwise
// leave one running for as long as the browser does, invisible to every caller.
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
