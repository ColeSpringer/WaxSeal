package cdp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// EvalResult wraps the JSON value returned by Eval. Its coercers return the zero
// value when the JSON type does not match. WaxSeal evals return
// JSON.stringify(...) or primitive values, so these helpers cover all call sites.
type EvalResult struct {
	Value json.RawMessage
}

// Str returns the result when it is a JSON string, else "".
func (r EvalResult) Str() string {
	var s string
	if len(r.Value) > 0 && json.Unmarshal(r.Value, &s) == nil {
		return s
	}
	return ""
}

// Int returns int(float64) when the result is a JSON number, else 0.
func (r EvalResult) Int() int {
	var f float64
	if len(r.Value) > 0 && json.Unmarshal(r.Value, &f) == nil {
		return int(f)
	}
	return 0
}

// Bool returns the result when it is a JSON bool, else false.
func (r EvalResult) Bool() bool {
	var b bool
	if len(r.Value) > 0 && json.Unmarshal(r.Value, &b) == nil {
		return b
	}
	return false
}

// EvalError wraps a JavaScript exception thrown during an Eval. It is a real
// failure and is never retried.
type EvalError struct {
	Text        string
	Description string
}

func (e *EvalError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("cdp: eval exception: %s: %s", e.Text, e.Description)
	}
	return fmt.Sprintf("cdp: eval exception: %s", e.Text)
}

// Eval runs js as a function applied to args on the page's window, awaiting
// promises and returning the result by value (TestEvalGolden pins the request).
// Args are CDP-serialized, never embedded in the source. A JavaScript exception
// returns *EvalError; a context-loss RPC error clears the cached window object
// id and retries after a backoff.
func (p *Page) Eval(js string, args ...any) (EvalResult, error) {
	callArgs, err := buildArgs(args)
	if err != nil {
		return EvalResult{}, err
	}
	fn := formatToJSFunc(js)

	var backoff time.Duration
	for {
		objID, err := p.windowObjectID()
		if err != nil {
			if retry, rerr := p.handleContextLoss(err, &backoff); retry {
				continue
			} else if rerr != nil {
				return EvalResult{}, rerr
			}
			return EvalResult{}, err
		}

		var res runtimeCallResult
		cerr := p.conn.call(p.ctx, p.sessionID, "Runtime.callFunctionOn", runtimeCallFunctionOn{
			FunctionDeclaration: fn,
			ObjectID:            objID,
			Arguments:           callArgs,
			ReturnByValue:       true,
			AwaitPromise:        true,
		}, &res)
		if cerr != nil {
			if retry, rerr := p.handleContextLoss(cerr, &backoff); retry {
				continue
			} else if rerr != nil {
				return EvalResult{}, rerr
			}
			return EvalResult{}, cerr
		}
		if res.ExceptionDetails != nil {
			return EvalResult{}, evalError(res.ExceptionDetails)
		}
		return EvalResult{Value: res.Result.Value}, nil
	}
}

// handleContextLoss decides whether err is a retryable context-loss error. When it
// is, it clears the cached window object id and sleeps a growing backoff (capped),
// returning retry=true. ctx cancellation during the backoff returns it as rerr.
func (p *Page) handleContextLoss(err error, backoff *time.Duration) (retry bool, rerr error) {
	if !isContextLost(err) {
		return false, nil
	}
	p.jsCtx.clear()
	if *backoff == 0 {
		*backoff = 30 * time.Millisecond
	} else if *backoff *= 2; *backoff > 3*time.Second {
		*backoff = 3 * time.Second
	}
	select {
	case <-p.ctx.Done():
		return false, p.ctx.Err()
	case <-time.After(*backoff):
		return true, nil
	}
}

// contextLostMessages are the RPC errors meaning the cached window object id's
// execution context is gone and the id must be resolved again. They match as
// substrings because trailing punctuation varies across Chrome versions.
var contextLostMessages = []string{
	"Execution context was destroyed",
	"Cannot find context with specified id",
	"Invalid object id",
}

// isContextLost reports whether err is an RPC-level context-loss error.
func isContextLost(err error) bool {
	var re *rpcError
	if !errors.As(err, &re) {
		return false
	}
	for _, m := range contextLostMessages {
		if strings.Contains(re.Message, m) {
			return true
		}
	}
	return false
}

// buildArgs CDP-serializes each Go arg as a by-value call argument.
func buildArgs(args []any) ([]callArgument, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make([]callArgument, 0, len(args))
	for i, a := range args {
		raw, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("cdp: marshal eval arg %d: %w", i, err)
		}
		out = append(out, callArgument{Value: raw})
	}
	return out, nil
}

// formatToJSFunc trims the source and wraps it as Runtime.callFunctionOn expects.
// TestEvalGolden pins the exact wrapper.
func formatToJSFunc(js string) string {
	js = strings.Trim(js, "\t\n\v\f\r ;")
	return fmt.Sprintf(`function() { return (%s).apply(this, arguments) }`, js)
}

// windowObjectID returns the cached window object id, resolving it via
// Runtime.evaluate("window") on first use (and after a context loss clears it).
func (p *Page) windowObjectID() (string, error) {
	p.jsCtx.mu.Lock()
	id := p.jsCtx.id
	p.jsCtx.mu.Unlock()
	if id != "" {
		return id, nil
	}

	// Resolve outside the lock so a stalled CDP call cannot block a concurrent
	// clear or Eval. Two concurrent resolves both get a valid id and converge
	// on the first one stored.
	var res runtimeEvaluateResult
	if err := p.conn.call(p.ctx, p.sessionID, "Runtime.evaluate", runtimeEvaluateParams{Expression: "window"}, &res); err != nil {
		return "", err
	}
	if res.Result.ObjectID == "" {
		return "", errors.New("cdp: window resolved to no object id")
	}

	p.jsCtx.mu.Lock()
	if p.jsCtx.id == "" {
		p.jsCtx.id = res.Result.ObjectID
	}
	id = p.jsCtx.id
	p.jsCtx.mu.Unlock()
	return id, nil
}

func evalError(d *exceptionDetails) *EvalError {
	e := &EvalError{Text: d.Text}
	if d.Exception != nil {
		e.Description = d.Exception.Description
	}
	return e
}

// jsCtxCache holds the resolved window object id shared by all Context(ctx) copies
// of one page.
type jsCtxCache struct {
	mu sync.Mutex
	id string
}

func (j *jsCtxCache) clear() {
	j.mu.Lock()
	j.id = ""
	j.mu.Unlock()
}
