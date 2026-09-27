//go:build unix

package cdp

import (
	"fmt"
	"os"
)

// newPlatformPipePair returns an anonymous pipe. os.Pipe registers both ends
// with the runtime poller, which gives them deadlines and cancel-on-close;
// childPollable exists for Windows, whose child end is synchronous.
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
