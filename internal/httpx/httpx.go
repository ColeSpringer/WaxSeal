// Package httpx provides WaxSeal's Google-facing HTTP behavior. It wraps a
// caller's HTTP client with bounded retries, jittered backoff, Retry-After
// handling, and response size limits.
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

// Doer is the HTTP surface required by the BotGuard flow. Both *http.Client and
// *Client implement it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

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
// http.DefaultClient.
func New(hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{HTTP: hc, MaxRetries: 2, BaseDelay: 500 * time.Millisecond, MaxDelay: 5 * time.Second}
}

// Do executes req with bounded retries on network errors, 429, and 5xx,
// honoring Retry-After and the caller's context. The request body is rewound
// between attempts via req.GetBody (set automatically for bytes/strings
// readers). The returned response's Body is the caller's to close.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	attempts := max(c.MaxRetries+1, 1)
	var (
		lastErr  error
		lastCode int
		delay    time.Duration
	)
	for attempt := range attempts {
		if err := c.preAttempt(req, attempt, delay); err != nil {
			return nil, err
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr, lastCode = err, 0
			if !retryableErr(err) || attempt == attempts-1 {
				return nil, err
			}
			delay = c.backoff(attempt)
			c.logRetry(req, attempt, 0, delay, err)
			continue
		}

		if retryableStatus(resp.StatusCode) && attempt < attempts-1 {
			lastErr, lastCode = fmt.Errorf("status %d", resp.StatusCode), resp.StatusCode
			delay = c.retryDelay(resp, attempt)
			resp.Body.Close()
			c.logRetry(req, attempt, resp.StatusCode, delay, nil)
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("status %d", lastCode)
	}
	return nil, lastErr
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
			return nil, err
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr, lastCode = err, 0
			if !retryableErr(err) || attempt == attempts-1 {
				return nil, err
			}
			delay = c.backoff(attempt)
			c.logRetry(req, attempt, 0, delay, err)
			continue
		}

		// Retryable status: skip reading the (error) body, back off, retry.
		if retryableStatus(resp.StatusCode) && attempt < attempts-1 {
			lastErr, lastCode = fmt.Errorf("status %d", resp.StatusCode), resp.StatusCode
			delay = c.retryDelay(resp, attempt)
			resp.Body.Close()
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
// backoff period. The first attempt needs no preparation. Do and DoJSON share
// this helper.
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
