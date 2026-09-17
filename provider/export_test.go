package provider

import (
	"context"
	"time"
)

// RetryWaitForTest exposes the retry rule to the external test package, so a
// test can assert that a refusal earns no retry without reaching for the wire.
func RetryWaitForTest(err error) (time.Duration, bool) { return retryWait(err) }

// SetSleepForTest swaps the retry's wait for a recorder and returns a function
// restoring the real one.
func SetSleepForTest(fn func(ctx context.Context, d time.Duration) error) func() {
	prev := sleep
	sleep = fn
	return func() { sleep = prev }
}
