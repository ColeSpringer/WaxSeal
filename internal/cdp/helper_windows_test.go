//go:build windows

package cdp

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// helperTransport opens the inherited transport the way Chromium does on Windows:
// there is no fd convention, so the two handle values arrive in the argv and are
// adopted from there. Parsing the real ioPipesFlagPrefix is what makes this test
// binary a faithful stand-in for the browser on this platform.
func helperTransport() (in, out *os.File, err error) {
	raw := ""
	for _, a := range os.Args {
		if strings.HasPrefix(a, ioPipesFlagPrefix) {
			raw = strings.TrimPrefix(a, ioPipesFlagPrefix)
			break
		}
	}
	if raw == "" {
		return nil, nil, fmt.Errorf("no %s in argv %v", ioPipesFlagPrefix, os.Args)
	}
	inRaw, outRaw, ok := strings.Cut(raw, ",")
	if !ok {
		return nil, nil, fmt.Errorf("malformed %s%s", ioPipesFlagPrefix, raw)
	}
	inHandle, err := strconv.ParseUint(inRaw, 10, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("read handle %q: %w", inRaw, err)
	}
	outHandle, err := strconv.ParseUint(outRaw, 10, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("write handle %q: %w", outRaw, err)
	}
	return os.NewFile(uintptr(inHandle), "cdp-in"), os.NewFile(uintptr(outHandle), "cdp-out"), nil
}
