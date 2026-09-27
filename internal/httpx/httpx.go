// Package httpx provides WaxSeal's Google-facing HTTP behavior. It wraps a
// caller's HTTP client with bounded retries, jittered backoff, Retry-After
// handling, and response size limits.
//
// No pause outlasts the caller's deadline. A backoff that would consume the
// remaining budget is skipped and the failure that provoked it is returned, so
// the caller sees the cause instead of a bare context timeout. MaxDelay is the
// fixed cap on a pause; the deadline is the per-request one.
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

// New wraps hc with default retry and backoff settings; a nil hc uses
// http.DefaultClient. It adds no client Timeout (that bounds each attempt, not
// the retry sequence), so callers must bound each request with a context.
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
			// preAttempt fails on a cancellation during the backoff wait or on a
			// rewind (GetBody) failure. A cancellation passes through so
			// errors.Is(err, context.Canceled) holds; a rewind failure must not mask
			// the failure that triggered the retry. pauseBlocked already refused
			// pauses the deadline cannot outlast, so DeadlineExceeded here only
			// covers a wait that overran its own budget.
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
			err = keepCause(req.Context(), lastErr, err)
			lastErr, lastCode = err, 0
			if attempt == attempts-1 {
				return nil, err
			}
			delay = c.backoff(attempt)
			// The deadline would swallow the retry: report the transport error.
			if berr := c.pauseBlocked(req.Context(), delay, err); berr != nil {
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
			if berr := c.pauseBlocked(req.Context(), delay, lastErr); berr != nil {
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
			readErr = keepCause(req.Context(), lastErr, readErr)
			lastErr, lastCode = readErr, code
			if attempt == attempts-1 {
				return nil, readErr
			}
			delay = c.backoff(attempt)
			// The deadline would swallow the retry: report the read failure.
			if berr := c.pauseBlocked(req.Context(), delay, readErr); berr != nil {
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

// retryHeadroom is the time a pause must leave for the retry it enables. A
// Google-facing TLS request rarely completes in under a second, and a second
// is cheap against caller budgets of minutes (the 3-minute request timeout,
// the 120 s startup warm-up).
const retryHeadroom = time.Second

// pauseBlocked returns the error to report instead of pausing for d, or nil when
// the pause may proceed. pending is the failure that provoked this retry.
// Cancellation and deadline come from ctx, never from pending: a per-attempt
// http.Client.Timeout or dial timeout also matches context.DeadlineExceeded.
// After such a timeout the deadline must also fit a whole c.HTTP.Timeout,
// since an endpoint that stalled once likely stalls the retry too.
//
// Cancellation outranks pending: it is a caller giving up, which the CLI maps
// to exit 130, and reporting it as a 502 would lose the interrupt. An expired
// or too-short deadline returns pending, since pending explains the timeout and
// a bare context error does not.
func (c *Client) pauseBlocked(ctx context.Context, d time.Duration, pending error) error {
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	need := d + retryHeadroom
	if errors.Is(pending, context.DeadlineExceeded) {
		need += c.HTTP.Timeout
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= need {
		return pending
	}
	return nil
}

// keepCause returns err, or prev with err wrapped too when err is a retry cut
// off by the caller's deadline: err then names only the deadline, and prev the
// failure that forced the retry.
func keepCause(ctx context.Context, prev, err error) error {
	if prev == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		!errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w (retry: %w)", prev, err)
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
	// Equal jitter in [d/2, d]: spreads a thundering herd, keeps a d/2 floor.
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// retryDelay honors Retry-After (delta-seconds or HTTP-date), capped at
// MaxDelay, else falls back to jittered backoff.
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

// preAttempt rewinds the request body and waits out the backoff before each
// retry. The first attempt needs neither.
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
