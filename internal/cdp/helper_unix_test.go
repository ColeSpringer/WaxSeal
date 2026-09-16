//go:build unix

package cdp

import "os"

// helperTransport opens the inherited transport the way Chromium does on Unix:
// --remote-debugging-pipe reads commands from fd 3 and writes responses to fd 4,
// by convention, with nothing in the argv to say so.
func helperTransport() (in, out *os.File, err error) {
	return os.NewFile(3, "cdp-in"), os.NewFile(4, "cdp-out"), nil
}
