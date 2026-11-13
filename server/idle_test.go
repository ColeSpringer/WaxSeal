package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
)

// A daemon whose Chromium was closed for idleness is healthy and says why on
// either probe: the benign no-session every consumer already reads, with
// browser_idle set. Nothing is logged as a failure, and strict keeps it at 200,
// so the image's health check passes while the daemon sleeps.
func TestPingReportsAnIdleBrowser(t *testing.T) {
	for _, tc := range []struct {
		name  string
		keys  map[string]string
		key   string
		probe string
	}{
		{"daemon probe", keyedOnly, "", PingProbeDaemon},
		{"keyed tenant probe", keyedOnly, "K", PingProbeTenant},
		{"keyless tenant probe", nil, "", PingProbeTenant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := liveServer(t, tc.keys, nil)
			proberOf(s, &fakeProber{health: answer(browser.RecoverySuspended, nil)})
			logs := &capturingHandler{}
			s.log = slog.New(logs)

			for _, target := range []string{"/ping", "/ping?strict=true"} {
				code, resp := pingWith(t, s, target, tc.key)
				if code != http.StatusOK {
					t.Errorf("%s status = %d, want 200", target, code)
				}
				if resp["ok"] != false || resp["reason"] != PingReasonNoSession || resp["probe"] != tc.probe {
					t.Errorf("%s body = %v, want ok:false reason:no-session probe:%s", target, resp, tc.probe)
				}
				if resp["browser_idle"] != true || resp["browser_relaunched"] != false {
					t.Errorf("%s browser_idle=%v browser_relaunched=%v, want true/false", target, resp["browser_idle"], resp["browser_relaunched"])
				}
			}
			if logs.has(slog.LevelWarn, "ping probe failed") {
				t.Error("an idle browser was logged as a failed probe")
			}
		})
	}
}

// browser_idle is on every health body and false while a browser runs,
// whatever the probe found.
func TestPingBrowserIdleIsFalseWhileABrowserRuns(t *testing.T) {
	healthy := map[string]*fakePlayerSession{"K": {abrURL: "https://r/ok", vd: "vd"}}
	for _, tc := range []struct {
		name     string
		sessions map[string]*fakePlayerSession
		key      string
		rec      browser.Recovery
	}{
		{"healthy session", healthy, "K", browser.RecoveryNone},
		{"no session, browser answered", nil, "K", browser.RecoveryNone},
		{"no session, browser relaunched", nil, "K", browser.RecoveryRelaunched},
		{"daemon probe, browser answered", nil, "", browser.RecoveryNone},
		{"daemon probe, browser torn down", nil, "", browser.RecoveryTornDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := liveServer(t, keyedOnly, tc.sessions)
			proberOf(s, &fakeProber{health: answer(tc.rec, nil)})
			_, resp := pingWith(t, s, "/ping", tc.key)
			if got, present := resp["browser_idle"]; !present || got != false {
				t.Errorf("browser_idle = %v (present=%v), want false", got, present)
			}
		})
	}
}

// Both /metrics views count the browsers closed for idleness, at top level
// beside the other daemon-wide browser counters.
func TestMetricsCountIdleBrowserCloses(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys map[string]string
	}{
		{"full", nil},
		{"redacted", keyedOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := liveServer(t, tc.keys, nil)
			proberOf(s, &fakeProber{idleCloses: 3})
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			var m map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
				t.Fatalf("decode /metrics: %v", err)
			}
			if m["browser_idle_closes"] != float64(3) {
				t.Errorf("browser_idle_closes = %v, want 3", m["browser_idle_closes"])
			}
		})
	}
}

// Shutdown stops idle closing before it drains. Otherwise a pass during the
// drain could close the browser under a request still being served, and one
// caught mid-close would hold the exit past the drain budget.
func TestShutdownStopsIdleClosingBeforeTheDrain(t *testing.T) {
	s := liveServer(t, nil, nil)
	probing, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	prober := proberOf(s, &fakeProber{health: func(context.Context) (browser.Recovery, error) {
		once.Do(func() { close(probing) })
		<-release
		return browser.RecoveryNone, nil
	}})
	// With no tenant ever used, every pass asks for the browser to be closed,
	// which makes the passes countable.
	s.tenants.CloseIdleAfter(4 * time.Millisecond)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.srv = newHTTPServer(ln.Addr().String(), s.routes())
	go func() { _ = s.Serve(ln) }()

	// A probe held in the browser check keeps the drain open without being use.
	pinged := make(chan struct{})
	go func() {
		defer close(pinged)
		if resp, err := http.Get("http://" + ln.Addr().String() + "/ping"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-probing
	for deadline := time.Now().Add(5 * time.Second); prober.suspends.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the idle loop never ran a pass")
		}
	}

	stopped := make(chan error, 1)
	go func() { stopped <- s.Shutdown(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // Shutdown is draining; a pass in flight has finished
	before := prober.suspends.Load()
	time.Sleep(100 * time.Millisecond)
	if after := prober.suspends.Load(); after != before {
		t.Errorf("idle passes kept running during the drain: %d more", after-before)
	}
	close(release)
	<-pinged
	if err := <-stopped; err != nil {
		t.Errorf("Shutdown = %v", err)
	}
}
