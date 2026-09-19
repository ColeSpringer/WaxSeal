package provider

import (
	"context"
	"time"
)

// SetSleepForTest swaps the retry's wait for a recorder and returns a function
// restoring the real one.
func SetSleepForTest(fn func(ctx context.Context, d time.Duration) error) func() {
	prev := sleep
	sleep = fn
	return func() { sleep = prev }
}
