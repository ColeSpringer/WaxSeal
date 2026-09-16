package cdp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// The spawn path is the one part of this package that differs per platform, and
// it used to be covered only by the -tags live tests against a real Chromium.
// These tests replace the browser with this test binary, re-executed as a helper,
// so the inheritance, the handle numbering, and the handshake are exercised
// offline on whatever platform is running.
//
// TestMain dispatches on WAXSEAL_TEST_HELPER before m.Run, the standard way to
// make a test binary re-executable as a child process.

// waxsealTestHelperEnv selects a helper mode instead of running the test suite,
// and waxsealTestPIDFileEnv names a file the helper writes its own pid into, so a
// test can watch a child whose Spawn never returned a Browser to ask.
const (
	waxsealTestHelperEnv  = "WAXSEAL_TEST_HELPER"
	waxsealTestPIDFileEnv = "WAXSEAL_TEST_HELPER_PIDFILE"
)

func TestMain(m *testing.M) {
	mode := os.Getenv(waxsealTestHelperEnv)
	if mode != "" {
		helperWritePID()
	}
	switch mode {
	case "stall":
		// A child that starts, inherits the pipes, and then says nothing. It is how
		// a test drives the handshake timeout.
		time.Sleep(60 * time.Second)
		os.Exit(0)
	case "cdp-echo":
		helperCDPEcho()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helperWritePID records the helper's pid where the parent test can read it.
func helperWritePID() {
	path := os.Getenv(waxsealTestPIDFileEnv)
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// helperCDPEcho stands in for Chromium: it opens the inherited transport, reads
// NUL-delimited request frames, and answers each one with a Browser.getVersion
// shaped result that also echoes its own argv, so the caller can assert what it
// was launched with. It returns at EOF, which is what a parent's death looks like
// from here.
func helperCDPEcho() {
	in, out, err := helperTransport()
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(1)
	}
	r := bufio.NewReaderSize(in, 64<<10)
	for {
		frame, rerr := r.ReadBytes(0)
		if len(frame) > 1 {
			var req struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(frame[:len(frame)-1], &req) == nil {
				resp, _ := json.Marshal(map[string]any{
					"id": req.ID,
					"result": map[string]any{
						"product":    "HelperChrome/1.0",
						"helperArgv": os.Args,
					},
				})
				if _, werr := out.Write(append(resp, 0)); werr != nil {
					return
				}
			}
		}
		if rerr != nil {
			return
		}
	}
}
