// Package cdp is a small standard-library Chrome DevTools Protocol client. It
// drives Chromium over --remote-debugging-pipe, framing messages as NUL-delimited
// JSON. The package preserves the request JSON and Chromium argv that WaxSeal's
// browser fingerprint depends on.
//
// Two transports, one protocol. Unix passes an anonymous pair as the child's fd 3
// and fd 4, where --remote-debugging-pipe looks by convention; Windows has no such
// convention and no pollable anonymous pipe, so the pair is two overlapped named
// pipes named in the argv. Everything above the pipe is identical.
//
// The package must not import internal/browser.
package cdp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Tunables for the transport. writeTimeout bounds a single pipe write so an
// unresponsive Chromium cannot stall the transport indefinitely;
// defaultStderrMax bounds the crash-diagnostics ring buffer. maxFrameBytes bounds
// per-frame memory. Normal CDP eval responses are small, and media bytes are
// fetched over HTTPS by the consumer rather than through this pipe.
const (
	writeTimeout     = 10 * time.Second
	defaultStderrMax = 64 << 10
	eventChanBuffer  = 8
	maxFrameBytes    = 64 << 20
)

// ErrConnClosed reports that the CDP connection was torn down: Chromium exited,
// the pipe hit EOF, or Close ran. Callers waiting on a request use it as a
// relaunch signal.
var ErrConnClosed = errors.New("cdp: connection closed")

// errFrameTooLarge reports that an inbound frame exceeded maxFrameBytes. A
// live-but-misbehaving browser sent it, so the read loop force-closes rather than
// waiting for the pipe EOF used by normal teardown.
var errFrameTooLarge = errors.New("cdp: inbound frame exceeds size limit")

// rpcError is a CDP protocol error returned in a response. It is the error value
// surfaced from a Call so context-loss retries can match on it.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data is the optional error detail. CDP usually sends a string, but JSON-RPC
	// permits any JSON value. Keeping it raw prevents object or array data from
	// failing frame unmarshal and leaving the pending call open.
	Data json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("cdp rpc error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("cdp rpc error %d: %s", e.Code, e.Message)
}

// request is the outbound JSON-RPC envelope (id, sessionId, method, params), the
// standard CDP JSON-RPC request framing.
type request struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// inFrame is a single inbound message: a response carries id; an event carries
// method without id.
type inFrame struct {
	ID        *int64          `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Result    json.RawMessage `json:"result"`
	Error     *rpcError       `json:"error"`
	Params    json.RawMessage `json:"params"`
}

// rpcResult delivers a response to a waiting Call.
type rpcResult struct {
	result json.RawMessage
	err    error
}

// eventMsg is a delivered protocol event.
type eventMsg struct {
	method string
	params json.RawMessage
}

// subscription receives events for one session, filtered to a method set so a
// busy domain cannot crowd out the events a watcher actually waits for.
type subscription struct {
	id        int
	sessionID string
	methods   map[string]bool
	ch        chan eventMsg
}

// Conn owns the pipe transport to one Chromium process: the write side, a read
// loop, the pending-response map, and the event router.
type Conn struct {
	cmd   *exec.Cmd
	wpipe *os.File // parent writes commands here; the child reads them
	rpipe *os.File // parent reads responses and events here; the child writes them
	log   *slog.Logger

	// guard owns the platform's process-lifetime mechanism (a Unix process group,
	// a Windows job object). It is nil for a Conn a test built directly, so every
	// use goes through killProcess.
	guard *procGuard

	// exited is closed by the reaper once cmd.Wait has returned. Callers that
	// remove the profile directory right after teardown wait on it so they are not
	// racing files Chromium still has open. Only Spawn starts that reaper, so on a
	// Conn built without a process the channel is never closed; that is why
	// waitExited keys off cmd rather than off this channel.
	exited chan struct{}

	// procExited is set by the reaper once cmd.Wait has returned. After that the OS
	// may recycle the PID, so the process group must no longer be signaled.
	procExited atomic.Bool

	// writeSem is a 1-token semaphore guarding the write side. Unlike a mutex its
	// acquisition is cancellable, so a caller can observe its own context (or
	// teardown) instead of parking behind a writer stalled in wpipe.Write.
	writeSem chan struct{}

	mu       sync.Mutex
	closed   bool
	closeCh  chan struct{} // closed once on teardown
	closeErr error
	nextID   int64
	pending  map[int64]chan rpcResult

	subMu   sync.Mutex
	subs    map[int]*subscription
	nextSub int

	closeBrowserOnce sync.Once

	// warnDeadlineOnce keeps a pipe that refuses write deadlines to one log line
	// rather than one per frame.
	warnDeadlineOnce sync.Once
}

// newConn initializes the channels used by Conn. Tests should use it too; a
// zero-value writeSem would leave write waiting on a send that can never proceed.
func newConn(cmd *exec.Cmd, wpipe, rpipe *os.File, log *slog.Logger) *Conn {
	return &Conn{
		cmd:      cmd,
		wpipe:    wpipe,
		rpipe:    rpipe,
		log:      log,
		closeCh:  make(chan struct{}),
		exited:   make(chan struct{}),
		pending:  make(map[int64]chan rpcResult),
		writeSem: make(chan struct{}, 1),
	}
}

// Done returns a channel closed when the connection is torn down.
func (c *Conn) Done() <-chan struct{} { return c.closeCh }

// pid returns the spawned process id, or 0.
func (c *Conn) pid() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// readLoop reads NUL-delimited frames and dispatches them. A malformed frame is
// logged and skipped. A normal EOF/IO error means Chromium is going away, so the
// connection is torn down; an oversized frame means a live browser is
// misbehaving, so the process group is force-closed.
func (c *Conn) readLoop() {
	r := bufio.NewReaderSize(c.rpipe, 64<<10)
	for {
		line, err := readFrame(r, maxFrameBytes)
		if err != nil {
			if errors.Is(err, errFrameTooLarge) {
				c.forceClose(err)
				return
			}
			c.teardown(fmt.Errorf("%w: %v", ErrConnClosed, err))
			return
		}
		if n := len(line); n > 0 && line[n-1] == 0 {
			line = line[:n-1]
		}
		if len(line) == 0 {
			continue
		}
		var f inFrame
		if jerr := json.Unmarshal(line, &f); jerr != nil {
			c.log.Warn("cdp: skipping malformed frame", "err", jerr, "bytes", len(line))
			continue
		}
		switch {
		case f.ID != nil:
			c.dispatchResponse(*f.ID, f)
		case f.Method != "":
			c.dispatchEvent(f)
		}
	}
}

// readFrame reads one NUL-delimited frame and returns errFrameTooLarge if the
// accumulated frame would exceed limit. The returned slice is freshly allocated:
// readLoop may hand it to other goroutines as json.RawMessage, while ReadSlice
// aliases bufio.Reader's buffer. Partial chunks are copied before the next read.
func readFrame(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice(0)
		if len(buf)+len(chunk) > limit {
			return nil, errFrameTooLarge
		}
		if err == bufio.ErrBufferFull {
			buf = append(buf, chunk...) // copy out before the next read overwrites the buffer
			continue
		}
		if err != nil {
			return nil, err
		}
		// append copies chunk, which aliases the bufio buffer, into either a new slice
		// for single-chunk frames or the heap slice from prior iterations. The returned
		// frame never aliases the reader buffer.
		return append(buf, chunk...), nil
	}
}

func (c *Conn) dispatchResponse(id int64, f inFrame) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	var res rpcResult
	if f.Error != nil {
		res.err = f.Error
	} else {
		res.result = f.Result
	}
	ch <- res // buffered (cap 1): never blocks the read loop
}

func (c *Conn) dispatchEvent(f inFrame) {
	c.subMu.Lock()
	for _, s := range c.subs {
		if s.sessionID != "" && s.sessionID != f.SessionID {
			continue
		}
		if !s.methods[f.Method] {
			continue
		}
		select {
		case s.ch <- eventMsg{method: f.Method, params: f.Params}:
		default: // full subscriber: drop, do not block the read loop
		}
	}
	c.subMu.Unlock()
}

// teardown marks the connection closed, releases the pipes, fails every pending
// Call, and wakes everything waiting on Done. It is idempotent, so it owns pipe
// closing for every shutdown path: read-loop EOF, the process-exit reaper, and
// forceClose. Closing the pipes also wakes a readLoop parked on rpipe.
func (c *Conn) teardown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	pend := c.pending
	c.pending = nil
	close(c.closeCh)
	c.mu.Unlock()

	c.closePipes()
	for _, ch := range pend {
		ch <- rpcResult{err: err} // buffered (cap 1)
	}
}

// rawCall sends method+params and returns the raw response result. It registers a
// pending entry, writes under the cancellable write semaphore with a deadline, and
// selects on the response, the caller ctx, and connection teardown.
func (c *Conn) rawCall(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("cdp: marshal %s params: %w", method, err)
		}
		raw = b
	}

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	data, err := json.Marshal(request{ID: id, SessionID: sessionID, Method: method, Params: raw})
	if err != nil {
		c.dropPending(id)
		return nil, fmt.Errorf("cdp: marshal %s request: %w", method, err)
	}
	data = append(data, 0) // NUL-terminate

	if err := c.write(ctx, data); err != nil {
		c.dropPending(id)
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.dropPending(id)
		return nil, ctx.Err()
	case <-c.closeCh:
		return nil, c.closeErr
	case res := <-ch:
		return res.result, res.err
	}
}

// call sends method+params and unmarshals the response result into result when
// non-nil.
func (c *Conn) call(ctx context.Context, sessionID, method string, params, result any) error {
	raw, err := c.rawCall(ctx, sessionID, method, params)
	if err != nil {
		return err
	}
	if result != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("cdp: unmarshal %s result: %w", method, err)
		}
	}
	return nil
}

func (c *Conn) dropPending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// write serializes one frame through a 1-token semaphore and sets a fixed pipe
// deadline. The semaphore acquire observes ctx and teardown, so callers with short
// deadlines do not wait behind another writer stuck in wpipe.Write. The pipe
// deadline is deliberately independent of ctx: a short request deadline should not
// tear down the shared Conn for every tenant, and aborting mid-frame would corrupt
// the stream. Any actual write failure force-closes the connection.
func (c *Conn) write(ctx context.Context, data []byte) error {
	select {
	case c.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return c.closeErr
	}
	defer func() { <-c.writeSem }()
	// If ctx or closeCh became ready at the same time as the semaphore, select may
	// still choose the semaphore case. Check again before putting bytes on the pipe.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return c.closeErr
	default:
	}
	if derr := c.wpipe.SetWriteDeadline(time.Now().Add(writeTimeout)); derr != nil {
		// A pipe that cannot take a deadline leaves the write unbounded, so the
		// 10 s stall guard is silently gone. That should be impossible (Windows
		// proves the association when it builds the pair, and Unix pipes are always
		// pollable), which is exactly why it is worth one line if it ever happens.
		c.warnDeadlineOnce.Do(func() {
			c.log.Warn("cdp: command pipe does not support write deadlines; the write-stall guard is inactive", "err", derr)
		})
	}
	if _, err := c.wpipe.Write(data); err != nil {
		werr := fmt.Errorf("cdp: write: %w", err)
		c.forceClose(werr)
		return werr
	}
	return nil
}

// subscribe registers an event subscription for sessionID and the given methods.
func (c *Conn) subscribe(sessionID string, methods ...string) *subscription {
	set := make(map[string]bool, len(methods))
	for _, m := range methods {
		set[m] = true
	}
	c.subMu.Lock()
	c.nextSub++
	s := &subscription{id: c.nextSub, sessionID: sessionID, methods: set, ch: make(chan eventMsg, eventChanBuffer)}
	if c.subs == nil {
		c.subs = make(map[int]*subscription)
	}
	c.subs[s.id] = s
	c.subMu.Unlock()
	return s
}

func (c *Conn) unsubscribe(s *subscription) {
	if s == nil {
		return
	}
	c.subMu.Lock()
	delete(c.subs, s.id)
	c.subMu.Unlock()
}

// closePipes closes the parent ends, which lets Chromium read EOF on its command
// pipe and exit, and unblocks the read loop and cmd.Wait. Both platforms rely on
// Close waking a parked reader: on Unix the poller does it, and on Windows the
// parent ends are overlapped files the runtime registered with IOCP, so Close
// cancels the pending overlapped read rather than leaving the goroutine stuck
// until data arrives.
func (c *Conn) closePipes() {
	if c.wpipe != nil {
		_ = c.wpipe.Close()
	}
	if c.rpipe != nil {
		_ = c.rpipe.Close()
	}
}

// killProcess terminates Chromium the way this platform does it (a SIGKILL to the
// process group on Unix, a job-object termination on Windows), unless the process
// has already been reaped. Once cmd.Wait has returned, the OS may have recycled
// the PID, so a kill by pid could hit an unrelated process; and when the process
// is already gone there is nothing left to kill.
func (c *Conn) killProcess() {
	if c.procExited.Load() || c.guard == nil {
		return
	}
	c.guard.kill(c.cmd)
}

// forceClose terminates the process group, then tears down (which closes the pipes
// and fails every pending call). It is the single force-close sequence shared by
// the handshake-timeout, write-stall, oversized-frame, and browser-close paths.
// teardown keeps the sequence idempotent.
func (c *Conn) forceClose(err error) {
	c.killProcess()
	c.teardown(fmt.Errorf("%w: %v", ErrConnClosed, err))
}

// ringBuffer is a bounded io.Writer keeping the most recent max bytes.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = append(r.buf[:0], r.buf[len(r.buf)-r.max:]...)
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
