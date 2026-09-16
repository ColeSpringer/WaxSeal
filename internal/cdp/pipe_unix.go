//go:build unix

package cdp

import (
	"fmt"
	"os"
)

// newPlatformPipePair returns an anonymous pipe. Go's os.Pipe registers both ends
// with the runtime poller, so deadlines and cancel-on-close already work on both
// sides and childPollable needs no special handling here; it exists for Windows,
// where the child end is deliberately synchronous.
func newPlatformPipePair(dir pipeDir, _ bool) (*pipePair, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("cdp: pipe: %w", err)
	}
	if dir == pipeParentWrites {
		return &pipePair{parent: w, child: r}, nil
	}
	return &pipePair{parent: r, child: w}, nil
}
