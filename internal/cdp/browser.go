package cdp

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// gracefulCloseTimeout bounds the polite Browser.close before the root teardown
// falls back to a group kill.
const gracefulCloseTimeout = 2 * time.Second

// Browser is a CDP connection to the root Chromium browser or an isolated
// incognito context sharing that connection.
type Browser struct {
	conn      *Conn
	ctx       context.Context
	contextID string // "" = root browser target; non-empty = an incognito context
}

// Context returns a shallow copy of the browser that uses ctx for its CDP calls.
func (b *Browser) Context(ctx context.Context) *Browser {
	cp := *b
	cp.ctx = ctx
	return &cp
}

// Version returns Browser.getVersion. It doubles as the liveness probe.
func (b *Browser) Version() (*VersionResult, error) {
	var res VersionResult
	if err := b.conn.call(b.ctx, "", "Browser.getVersion", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Incognito creates an isolated browser context and returns a Browser copy that
// shares the connection but is scoped to the new context. Closing that copy
// disposes only the context.
func (b *Browser) Incognito() (*Browser, error) {
	var res createBrowserContextResult
	if err := b.conn.call(b.ctx, "", "Target.createBrowserContext", nil, &res); err != nil {
		return nil, err
	}
	cp := *b
	cp.contextID = res.BrowserContextID
	return &cp, nil
}

// Page creates a target in this browser context, attaches a flat CDP session, and
// enables the Page domain.
func (b *Browser) Page(target TargetCreateTarget) (*Page, error) {
	target.BrowserContextID = b.contextID
	var ct createTargetResult
	if err := b.conn.call(b.ctx, "", "Target.createTarget", target, &ct); err != nil {
		return nil, fmt.Errorf("cdp: create target: %w", err)
	}
	var att attachToTargetResult
	if err := b.conn.call(b.ctx, "", "Target.attachToTarget", attachToTargetParams{TargetID: ct.TargetID, Flatten: true}, &att); err != nil {
		return nil, fmt.Errorf("cdp: attach target: %w", err)
	}
	if att.SessionID == "" {
		return nil, fmt.Errorf("cdp: attach target returned no session id")
	}
	p := &Page{conn: b.conn, ctx: b.ctx, sessionID: att.SessionID, jsCtx: &jsCtxCache{}}
	if err := b.conn.call(b.ctx, att.SessionID, "Page.enable", nil, nil); err != nil {
		return nil, fmt.Errorf("cdp: enable page: %w", err)
	}
	return p, nil
}

// GetCookies returns the browser-level cookies for this context (Storage.getCookies
// scoped by browserContextId; "" is the root context). This stays correct across
// pooled incognito sessions.
func (b *Browser) GetCookies() ([]*Cookie, error) {
	var res getCookiesResult
	if err := b.conn.call(b.ctx, "", "Storage.getCookies", storageGetCookiesParams{BrowserContextID: b.contextID}, &res); err != nil {
		return nil, err
	}
	return res.Cookies, nil
}

// PID returns the Chromium process id, or 0. For an incognito copy it is still the
// shared process's id.
func (b *Browser) PID() int {
	if b == nil {
		return 0
	}
	return b.conn.pid()
}

// Close tears down the browser. For an incognito context it disposes only that
// context, leaving the shared process running. For the root it asks Chromium to
// close, then terminates the process group and closes the pipes. It is
// idempotent.
func (b *Browser) Close() error {
	if b == nil || b.conn == nil {
		return nil
	}
	if b.contextID != "" {
		return b.conn.call(b.ctx, "", "Target.disposeBrowserContext", disposeBrowserContextParams{BrowserContextID: b.contextID}, nil)
	}
	b.conn.closeRoot(b.ctx)
	return nil
}

// closeRoot asks the root browser to close, terminates whatever is left, closes
// the pipes, and then waits for the process to be reaped. Runs at most once.
func (c *Conn) closeRoot(ctx context.Context) {
	c.closeBrowserOnce.Do(func() {
		cctx, cancel := context.WithTimeout(ctx, gracefulCloseTimeout)
		_, _ = c.rawCall(cctx, "", "Browser.close", nil)
		cancel()
		c.forceClose(errors.New("browser closed"))
		if !c.waitExited() {
			c.log.Warn("cdp: chromium was not reaped before Close returned; a profile removal may find files still open",
				"budget", waitDelay, "pid", c.pid())
		}
	})
}

// waitExited blocks until the reaper has returned from cmd.Wait, and reports
// whether it did. A Conn with no process returns at once.
//
// Callers remove the profile directory right after teardown, and Chromium holds
// its files open until the process object is gone: on Unix that leaves a stray
// directory, on Windows the remove fails outright. The budget is waitDelay, which
// is what cmd.Wait may legitimately take after the process itself is gone. No
// caller context bounds it: the pool's teardown context is the same length and
// the polite close has already spent part of it, so it could never cover the
// budget the warning reports, and an expired one would make the wait a no-op.
func (c *Conn) waitExited() bool {
	if c == nil || c.cmd == nil {
		return true
	}
	timer := time.NewTimer(waitDelay)
	defer timer.Stop()
	select {
	case <-c.exited:
		return true
	case <-timer.C:
		return false
	}
}
