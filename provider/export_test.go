package provider

import (
	"context"
	"time"
)

// RealSleep is the retry's wait as shipped, captured before any test swaps it,
// for an arm that needs the wait to honor the context rather than a recorder.
var RealSleep = sleep

// SetSleepForTest swaps the retry's wait for fn and returns a function
// restoring the previous one.
func SetSleepForTest(fn func(ctx context.Context, d time.Duration) error) func() {
	prev := sleep
	sleep = fn
	return func() { sleep = prev }
}
