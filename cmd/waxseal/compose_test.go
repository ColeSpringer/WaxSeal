package main

import (
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/readmedoc"
)

// TestREADMEEmbedsComposeFile holds the compose block the README's quick start
// shows against compose.yaml itself, comments included, so what a reader copies
// out of the README is the file `docker compose up` runs from a checkout.
func TestREADMEEmbedsComposeFile(t *testing.T) {
	want, err := os.ReadFile("../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := readmedoc.Fence("../../README.md", "Quick start", "yaml")
	if err != nil {
		t.Fatalf("locate the README's compose block: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the README's Quick start compose block differs from compose.yaml; paste the file in again\n--- README ---\n%s--- compose.yaml ---\n%s", got, want)
	}
}

// TestComposeStopGraceCoversDrain holds compose.yaml's stop_grace_period above
// the daemon's default drain budget, so raising defaultShutdownTimeout without
// the file turns red here rather than as a SIGKILL mid-request on a host.
func TestComposeStopGraceCoversDrain(t *testing.T) {
	grace := durationIn(t, "../../compose.yaml", `(?m)^\s*stop_grace_period:\s*(\S+)`)
	if grace <= defaultShutdownTimeout {
		t.Errorf("compose.yaml stop_grace_period %v does not cover the %v default drain; raise it", grace, defaultShutdownTimeout)
	}
}

// TestImageHealthcheckOutlastsPing holds the Dockerfile's HEALTHCHECK timeout
// above the ping command's own budget, so a probe that spends its whole budget
// reports a probe failure rather than being cut off as a healthcheck timeout.
func TestImageHealthcheckOutlastsPing(t *testing.T) {
	timeout := durationIn(t, "../../Dockerfile", `(?m)^HEALTHCHECK\b.*--timeout=(\S+)`)
	if timeout <= pingTimeout {
		t.Errorf("Dockerfile HEALTHCHECK --timeout=%v does not outlast the ping budget of %v", timeout, pingTimeout)
	}
}

// durationIn reads the one duration a pattern's first group captures in path.
func durationIn(t *testing.T, path, pattern string) time.Duration {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(pattern).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s matches nothing in %s", pattern, path)
	}
	d, err := time.ParseDuration(string(m[1]))
	if err != nil {
		t.Fatalf("%s in %s: %v", string(m[1]), path, err)
	}
	return d
}
