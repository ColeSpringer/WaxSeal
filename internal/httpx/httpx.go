// Package httpx provides WaxSeal's Google-facing HTTP behavior. It wraps a
// caller's HTTP client with bounded retries, jittered backoff, Retry-After
// handling, and response size limits.
//
// No pause outlasts the caller's deadline. A backoff that would consume the
// remaining budget is skipped and the failure that provoked it is returned
// instead, so the cause reaches the caller rather than a bare context timeout
// that names nothing. MaxDelay is the absolute cap on a pause; this is the
// per-request one.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// ErrBodyTooLarge is returned when a response body exceeds the configured limit.
var ErrBodyTooLarge = errors.New("httpx: response body exceeds cap")

// Client wraps an *http.Client with bounded, jittered retries and Retry-After
// handling. Construct one with New.
type Client struct {
	HTTP       *http.Client
	MaxRetries int           // retries AFTER the first attempt (default 2)
	BaseDelay  time.Duration // backoff base (default 500ms)
	MaxDelay   time.Duration // backoff cap (default 5s)
	Logger     *slog.Logger
}

// New wraps hc with default retry and backoff settings. A nil hc uses
// http.DefaultClient. There is no client-level Timeout (it interacts poorly with
// multi-attempt retries); callers must drive each request with a bounded context.
func New(hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{HTTP: hc, MaxRetries: 2, BaseDelay: 500 * time.Millisecond, MaxDelay: 5 * time.Second}
}

// DoJSON runs req, requires a 2xx status, and returns a body no larger than
// maxBody.
//
// DoJSON reads the body inside the retry loop, so it retries connections that
// fail after headers arrive. ErrBodyTooLarge is never retried.
func (c *Client) DoJSON(req *http.Request, maxBody int64) ([]byte, error) {
	attempts := max(c.MaxRetries+1, 1)
	var (
		lastErr  error
		lastCode int
		delay    time.Duration
	)
	for attempt := range attempts {
		if err := c.preAttempt(req, attempt, delay); err != nil {
			// preAttempt returns either an intended cancellation from the backoff wait
			// or a rewind (GetBody) failure. A cancellation must pass through so the
			// caller's errors.Is(err, context.Canceled) still holds; a rewind failure
			// should not mask the original transport error that triggered the retry.
			//
			// The wait is not where a deadline strands the cause: pauseBlocked has
			// already refused any pause the deadline cannot outlast, so the wait ends
			// early only on cancellation. Deadline handling is kept here for the
			// degenerate case of a wait that overruns its own budget.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if lastErr != nil {
				return nil, fmt.Errorf("%w (retry prep failed: %v)", lastErr, err)
			}
			return nil, err
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr, lastCode = err, 0
			if !retryableErr(err) || attempt == attempts-1 {
				return nil, err
			}
			delay = c.backoff(attempt)
			// The deadline would swallow the retry: report the transport error.
			if berr := pauseBlocked(req.Context(), delay, err); berr != nil {
				return nil, berr
			}
			c.logRetry(req, attempt, 0, delay, err)
			continue
		}

		// Retryable status: skip reading the (error) body, back off, retry.
		if retryableStatus(resp.StatusCode) && attempt < attempts-1 {
			lastErr, lastCode = fmt.Errorf("status %d", resp.StatusCode), resp.StatusCode
			delay = c.retryDelay(resp, attempt)
			resp.Body.Close()
			// A Retry-After the deadline cannot accommodate is reported as the status
			// it came with, rather than slept into a timeout that hides the throttling.
			if berr := pauseBlocked(req.Context(), delay, lastErr); berr != nil {
				return nil, berr
			}
			c.logRetry(req, attempt, resp.StatusCode, delay, nil)
			continue
		}

		data, readErr := ReadBodyCapped(resp.Body, maxBody)
		code := resp.StatusCode
		resp.Body.Close()
		if readErr != nil {
			if errors.Is(readErr, ErrBodyTooLarge) {
				return nil, readErr // a cap breach won't shrink on retry
			}
			lastErr, lastCode = readErr, code
			if !retryableErr(readErr) || attempt == attempts-1 {
				return nil, readErr
			}
			delay = c.backoff(attempt)
			// The deadline would swallow the retry: report the read failure.
			if berr := pauseBlocked(req.Context(), delay, readErr); berr != nil {
				return nil, berr
			}
			c.logRetry(req, attempt, code, delay, readErr)
			continue
		}
		if code < 200 || code >= 300 {
			return nil, fmt.Errorf("status %d", code)
		}
		return data, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("status %d", lastCode)
	}
	return nil, lastErr
}

// ReadBodyCapped reads up to maxBody bytes, returning ErrBodyTooLarge if the
// source has more (no silent truncation).
func ReadBodyCapped(r io.Reader, maxBody int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > maxBody {
		return nil, ErrBodyTooLarge
	}
	return data, nil
}

// retryHeadroom is the slack a pause must leave for the attempt it exists to
// enable. Sleeping down to the wire buys a request that cannot finish, and the
// deadline then masks the real cause all over again.
//
// A second is sized for what this package talks to. Every caller is Google-facing
// over TLS, so a retry that gets less than that essentially cannot complete, and
// shrinking the headroom would only trade a typed error for the bare timeout this
// exists to prevent. It costs those callers nothing, because the budgets it is
// measured against are whole minutes: the server's 3-minute request timeout, the
// 120s warm path, the 100s ping path. A local, millisecond-scale endpoint would
// want a smaller value, but this package serves none.
//
// Only a deadline on the request's context counts. The 60s on the browser
// session's http.Client is a Timeout, which bounds each attempt separately and
// never reaches req.Context().Deadline(), so nothing here can observe it. Resize
// against the caller contexts above, not against that.
const retryHeadroom = time.Second

// pauseBlocked returns the error to report instead of pausing for d, or nil when
// the pause may proceed. pending is the failure the caller is already holding,
// the one that provoked this retry.
//
// Cancellation outranks pending. A context canceled while a request was failing
// is a caller giving up, which the CLI maps to exit 130; reporting it as a 502
// would both misclassify it and lose the interrupt. An expired or too-short
// deadline is the opposite case, and the one this exists for: pending is exactly
// what explains that timeout, so it is returned in place of a bare context error.
func pauseBlocked(ctx context.Context, d time.Duration, pending error) error {
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= d+retryHeadroom {
		return pending
	}
	return nil
}

func (c *Client) backoff(attempt int) time.Duration {
	base := c.BaseDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	max := c.MaxDelay
	if max <= 0 {
		max = 5 * time.Second
	}
	d := base << attempt // base * 2^attempt
	if d > max || d <= 0 {
		d = max
	}
	// Full jitter in [d/2, d]: spreads a thundering herd without starving.
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// retryDelay honors Retry-After (delta-seconds or HTTP-date) on 429/503, else
// falls back to jittered backoff.
func (c *Client) retryDelay(resp *http.Response, attempt int) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return capDelay(time.Duration(secs)*time.Second, c.MaxDelay)
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return capDelay(d, c.MaxDelay)
			}
		}
	}
	return c.backoff(attempt)
}

func capDelay(d, max time.Duration) time.Duration {
	if max <= 0 {
		max = 5 * time.Second
	}
	if d > max {
		return max
	}
	return d
}

func (c *Client) logRetry(req *http.Request, attempt, status int, delay time.Duration, err error) {
	if c.Logger == nil {
		return
	}
	c.Logger.Debug("httpx retry",
		"url", req.URL.Redacted(), "attempt", attempt+1,
		"status", status, "delay", delay, "err", err)
}

// preAttempt prepares a retry by rewinding the request body and waiting for the
// backoff period. The first attempt needs no preparation. DoJSON calls it before
// each attempt.
func (c *Client) preAttempt(req *http.Request, attempt int, delay time.Duration) error {
	if attempt == 0 {
		return nil
	}
	if err := rewind(req); err != nil {
		return err
	}
	select {
	case <-time.After(delay):
		return nil
	case <-req.Context().Done():
		return req.Context().Err()
	}
}

// rewind resets req.Body from GetBody so a retried request re-sends its payload.
func rewind(req *http.Request) error {
	if req.Body == nil {
		return nil
	}
	if req.GetBody == nil {
		return errors.New("httpx: cannot retry request without GetBody")
	}
	body, err := req.GetBody()
	if err != nil {
		return fmt.Errorf("httpx: rewind body: %w", err)
	}
	req.Body = body
	return nil
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// retryableErr treats transport-level failures (timeouts, resets, EOF) as
// retryable; context cancellation is not.
func retryableErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}
