package cdp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Page is a CDP page bound to one flat session.
type Page struct {
	conn      *Conn
	ctx       context.Context
	sessionID string
	jsCtx     *jsCtxCache
}

// Context returns a shallow copy of the page that uses ctx for its CDP calls. The
// copy shares the session and the cached window object id.
func (p *Page) Context(ctx context.Context) *Page {
	cp := *p
	cp.ctx = ctx
	return &cp
}

// Navigate stops any current load, drops the cached window object id, and
// points the page at url. The id is dropped whatever the outcome, since a
// navigation that returns an errorText can still swap the execution context.
func (p *Page) Navigate(url string) error {
	if url == "" {
		url = "about:blank"
	}
	_, _ = p.conn.rawCall(p.ctx, p.sessionID, "Page.stopLoading", nil) // best-effort
	p.jsCtx.clear()
	var res navigateResult
	if err := p.conn.call(p.ctx, p.sessionID, "Page.navigate", navigateParams{URL: url}, &res); err != nil {
		return err
	}
	if res.ErrorText != "" {
		return &NavigateError{URL: url, Text: res.ErrorText}
	}
	return nil
}

// NavigateError reports a failed Page.navigate.
type NavigateError struct {
	URL  string
	Text string
}

func (e *NavigateError) Error() string { return fmt.Sprintf("cdp: navigate %s: %s", e.URL, e.Text) }

// waitLoadJS resolves when document.readyState reaches complete. Running it via
// Eval avoids racing a separately registered load-event listener.
const waitLoadJS = `() => new Promise((resolve) => {
	if (document.readyState === 'complete') { resolve(); return; }
	window.addEventListener('load', () => resolve());
})`

// WaitLoad blocks until the page's load event has fired (or already had).
func (p *Page) WaitLoad() error {
	_, err := p.Eval(waitLoadJS)
	return err
}

// Cookies returns the page's cookies for urls via Network.getCookies, without
// enabling the Network domain, which would start an event stream. WaxSeal
// always passes urls; callers needing page-scoped defaults should add them
// explicitly.
func (p *Page) Cookies(urls []string) ([]*Cookie, error) {
	var res getCookiesResult
	if err := p.conn.call(p.ctx, p.sessionID, "Network.getCookies", networkGetCookiesParams{URLs: urls}, &res); err != nil {
		return nil, err
	}
	return res.Cookies, nil
}

// SetBypassCSP toggles Page.setBypassCSP.
func (p *Page) SetBypassCSP(enabled bool) error {
	return p.conn.call(p.ctx, p.sessionID, "Page.setBypassCSP", setBypassCSPParams{Enabled: enabled}, nil)
}

// SetUserAgentOverride applies Network.setUserAgentOverride (UA string plus the
// full UA-CH metadata).
func (p *Page) SetUserAgentOverride(req *NetworkSetUserAgentOverride) error {
	return p.conn.call(p.ctx, p.sessionID, "Network.setUserAgentOverride", req, nil)
}

// WaitCrash enables the Inspector domain and blocks until the page's target
// crashes or detaches, the connection is lost, or ctx is done; ctx also bounds
// the enable call. It returns a diagnostic reason, or "" when ctx is done.
//
// The subscription is registered before Inspector.enable so a crash arriving in
// the enable round trip is not dropped for lack of a subscriber.
func (p *Page) WaitCrash(ctx context.Context) string {
	sub := p.conn.subscribe(p.sessionID, "Inspector.targetCrashed", "Inspector.detached")
	defer p.conn.unsubscribe(sub)
	_ = p.conn.call(ctx, p.sessionID, "Inspector.enable", nil, nil)
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-p.conn.Done():
			return "browser connection lost"
		case ev := <-sub.ch:
			switch ev.method {
			case "Inspector.targetCrashed":
				return "browser target crashed"
			case "Inspector.detached":
				var d struct {
					Reason string `json:"reason"`
				}
				_ = json.Unmarshal(ev.params, &d)
				return "browser detached: " + d.Reason
			}
		}
	}
}
