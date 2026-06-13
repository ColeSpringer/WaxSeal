package browser

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLaunchWithin(t *testing.T) {
	u, err := launchWithin(func() (string, error) { return "ws://ok", nil }, time.Second)
	if err != nil || u != "ws://ok" {
		t.Errorf("success path = (%q, %v), want (ws://ok, nil)", u, err)
	}
	_, err = launchWithin(func() (string, error) { return "", errors.New("boom") }, time.Second)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error path = %v, want it to carry \"boom\"", err)
	}
	start := time.Now()
	_, err = launchWithin(func() (string, error) { select {} }, 50*time.Millisecond)
	if !errors.Is(err, errLaunchTimeout) {
		t.Errorf("timeout path = %v, want errLaunchTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("timeout took %s, want ~50ms", elapsed)
	}
}

func TestDetectChromeEnvOverride(t *testing.T) {
	t.Setenv("WAXSEAL_CHROME_BIN", "/custom/chromium")
	got, err := DetectChrome()
	if err != nil || got != "/custom/chromium" {
		t.Fatalf("DetectChrome() = %q, %v; want /custom/chromium", got, err)
	}
}

func TestWithDefaults(t *testing.T) {
	o := withDefaults(Options{})
	if o.Logger == nil {
		t.Error("Logger default is nil")
	}
	if o.NavTimeout <= 0 {
		t.Errorf("NavTimeout default = %v, want > 0", o.NavTimeout)
	}
	if got := withDefaults(Options{NavTimeout: 5 * time.Second}).NavTimeout; got != 5*time.Second {
		t.Errorf("explicit NavTimeout overwritten: %v", got)
	}
}

func TestDefaultVideoSet(t *testing.T) {
	if DefaultVideo == "" {
		t.Error("DefaultVideo must be a non-empty (non-copyrighted) video id")
	}
}

// TestAudioFormatTagDrift keeps the extracted JSON fields in sync with AudioFormat.
func TestAudioFormatTagDrift(t *testing.T) {
	const payload = `{"itag":251,"lmt":"171","is_drc":true,"audio_track_id":"en.4"}`
	var f AudioFormat
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !f.IsDrc {
		t.Error("is_drc did not decode into IsDrc")
	}
	if f.AudioTrackID != "en.4" {
		t.Errorf("audio_track_id = %q, want en.4", f.AudioTrackID)
	}
}

// TestConfirmTerminal covers stale evidence that must not mark the current video
// unavailable.
func TestConfirmTerminal(t *testing.T) {
	const want = "vid123"
	raw := func(mut func(*playerContextRaw)) playerContextRaw {
		r := playerContextRaw{Error: "pending"}
		mut(&r)
		return r
	}
	tests := []struct {
		name         string
		raw          playerContextRaw
		wantTerminal bool
		wantStatus   string
	}{
		{"gen-matched onError 100, id match", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = want }), true, "ERROR"},
		{"gen-matched onError 150, id match", raw(func(r *playerContextRaw) { r.ErrCode = 150; r.ErrGenMatch = true; r.ErrVideoID = want }), true, "ERROR"},
		{"gen-matched onError 100, stale video id", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = "othervid" }), false, ""},
		{"gen-matched onError 100, empty video id", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = "" }), false, ""},
		{"non-OK status + id match", raw(func(r *playerContextRaw) { r.Status = "LOGIN_REQUIRED"; r.VideoIDMatch = true }), true, "LOGIN_REQUIRED"},
		{"non-OK status for another video", raw(func(r *playerContextRaw) { r.Status = "ERROR"; r.VideoIDMatch = false }), false, ""},
		{"onError 100 with gen mismatch", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = false; r.ErrVideoID = want }), false, ""},
		{"onError 5 (non-terminal code)", raw(func(r *playerContextRaw) { r.ErrCode = 5; r.ErrGenMatch = true; r.ErrVideoID = want }), false, ""},
		{"status OK + id match", raw(func(r *playerContextRaw) { r.Status = "OK"; r.VideoIDMatch = true }), false, ""},
		{"no evidence", raw(func(r *playerContextRaw) {}), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ue, ok := confirmTerminal(tt.raw, want)
			if ok != tt.wantTerminal {
				t.Fatalf("terminal = %v, want %v", ok, tt.wantTerminal)
			}
			if !ok {
				if ue != nil {
					t.Errorf("non-terminal returned a non-nil error: %v", ue)
				}
				return
			}
			if ue.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", ue.Status, tt.wantStatus)
			}
		})
	}
}

func TestIsUnavailableCode(t *testing.T) {
	for _, c := range []int{2, 100, 101, 150} {
		if !isUnavailableCode(c) {
			t.Errorf("isUnavailableCode(%d) = false, want true", c)
		}
	}
	for _, c := range []int{0, 5, 3, 104, 999} {
		if isUnavailableCode(c) {
			t.Errorf("isUnavailableCode(%d) = true, want false", c)
		}
	}
}

func TestFullLengthProbeModel(t *testing.T) {
	outcomes := map[string]bool{
		OutcomeFullLength:        true,
		OutcomeTargetNotBuffered: true,
		OutcomeNotEstablished:    true,
		OutcomeVideoTooShort:     true,
		OutcomeCanceled:          true,
	}
	if len(outcomes) != 5 {
		t.Fatalf("outcome constants are not all distinct: %v", outcomes)
	}
	if OutcomeFullLength != "full-length" {
		t.Errorf("OutcomeFullLength = %q, want full-length", OutcomeFullLength)
	}
}
