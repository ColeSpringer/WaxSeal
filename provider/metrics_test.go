package provider_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

// The /metrics helpers live in this untagged file, with their tests, so the
// browser-free, network-free tests run under plain `go test` and in CI. The e2e
// suite in e2e_test.go shares them.

// metricsSnapshot is one /metrics scrape reduced to the numbers the suite reads.
// counters holds the process-lifetime counters, summed across tenants when the
// daemon returned per-tenant detail. detailed records which shape came back:
// per-tenant detail carries state such as generation, the redacted aggregate
// carries lifetime counters only.
type metricsSnapshot struct {
	counters map[string]int64
	detailed bool
}

// readMetrics scrapes /metrics once and reads whichever shape the daemon
// returns. A keyed daemon redacts /metrics to summed lifetime counters unless
// the caller holds the operator --metrics-key; a tenant key does not unlock the
// detail (server.metricsFull says so explicitly), so an external keyed daemon,
// which is the WAXSEAL_URL plus WAXSEAL_KEY workflow the README recommends,
// normally answers redacted and has no per_tenant map at all. The key is still
// sent, because it does unlock detail when the operator points WAXSEAL_KEY at
// the metrics key.
func readMetrics(t *testing.T, base string) metricsSnapshot {
	t.Helper()
	m, err := fetchMetrics(base, os.Getenv("WAXSEAL_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// fetchMetrics is readMetrics without the fatal, so its failures are testable.
// The daemon says which shape it sent: the redacted body carries "redacted":
// true, and only that body is read as the aggregate. A non-redacted body with
// no tenants is an error in its own right (the daemon has served nobody yet),
// not a redaction, so it is not mistaken for one.
func fetchMetrics(base, key string) (metricsSnapshot, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	if err != nil {
		return metricsSnapshot{}, fmt.Errorf("build GET /metrics: %w", err)
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return metricsSnapshot{}, fmt.Errorf("GET /metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return metricsSnapshot{}, fmt.Errorf("GET /metrics: status %d, want 200 (the endpoint never needs a key; a non-200 is not redaction)", resp.StatusCode)
	}
	// Per-tenant entries mix counters with strings, booleans, and nulls, so decode
	// into any and keep the integers. UseNumber avoids reading a counter through a
	// float64.
	var body struct {
		Redacted  bool                      `json:"redacted"`
		PerTenant map[string]map[string]any `json:"per_tenant"`
		Aggregate map[string]any            `json:"aggregate"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return metricsSnapshot{}, fmt.Errorf("decode /metrics: %w", err)
	}
	out := metricsSnapshot{counters: make(map[string]int64)}
	if body.Redacted {
		for k, v := range body.Aggregate {
			if n, ok := metricInt(v); ok {
				out.counters[k] = n
			}
		}
		return out, nil
	}
	if len(body.PerTenant) == 0 {
		return metricsSnapshot{}, errors.New("/metrics is not redacted but lists no tenants: the daemon has not served a tenant yet, so there are no counters to read")
	}
	// Sum across tenants: the cold daemon's tenant label is not known here, and
	// an external daemon may serve several.
	out.detailed = true
	for _, per := range body.PerTenant {
		for k, v := range per {
			if n, ok := metricInt(v); ok {
				out.counters[k] += n
			}
		}
	}
	return out, nil
}

// metricInt reads an integer metric value, reporting false for the strings,
// booleans, and nulls that share the per-tenant map.
func metricInt(v any) (int64, bool) {
	num, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := num.Int64()
	if err != nil {
		return 0, false
	}
	return n, true
}

// counter reads one lifetime counter. A counter that neither shape carries is a
// hard failure, not a zero: a silent zero compares equal to the zero read before
// it, which turns a before-and-after assertion into a no-op that passes on every
// run.
func (m metricsSnapshot) counter(t *testing.T, name string) int64 {
	t.Helper()
	v, err := m.lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// lookup is counter without the fatal, so the failure itself is testable.
func (m metricsSnapshot) lookup(name string) (int64, error) {
	v, ok := m.counters[name]
	if !ok {
		shape := "redacted aggregate"
		if m.detailed {
			shape = "per-tenant detail"
		}
		return 0, fmt.Errorf("/metrics %s carries no %q counter (keys: %v)", shape, name, slices.Sorted(maps.Keys(m.counters)))
	}
	return v, nil
}

// playerContexts reads the lifetime player-context counter from whichever
// /metrics shape the daemon serves.
func playerContexts(t *testing.T, base string) int64 {
	t.Helper()
	return readMetrics(t, base).counter(t, "player_contexts")
}

// escalationMetrics contains the counters used together to detect an unnecessary
// relaunch. Values are summed so the test does not depend on the cold daemon's
// tenant label. GenerationKnown is false when the daemon served the redacted
// aggregate: generation is per-tenant state, so redaction drops it. Attestations
// survives redaction and rises on the same relaunch, so the check does not go
// blind.
//
// Attestations and Generation are the relaunch detectors. Escalations is
// narrower: it counts a request abandoning a generation it was still using, and
// a request handed the replacement after somebody else retired its generation
// relaunches without one. Reading it alone as "the ladder relaunched" would
// under-report.
type escalationMetrics struct {
	Generation            int64
	GenerationKnown       bool
	Attestations          int64
	Escalations           int64
	PlayerContextFailures int64
}

func readEscalationMetrics(t *testing.T, base string) escalationMetrics {
	t.Helper()
	// One scrape, so the four values describe the same moment.
	m := readMetrics(t, base)
	out := escalationMetrics{
		GenerationKnown:       m.detailed,
		Attestations:          m.counter(t, "attestations"),
		Escalations:           m.counter(t, "escalations"),
		PlayerContextFailures: m.counter(t, "player_context_failures"),
	}
	// Per-tenant detail always carries generation, so read it through the same
	// fatal helper: a silent zero here would reduce the generation check to the
	// 0 == 0 no-op this helper exists to remove.
	if m.detailed {
		out.Generation = m.counter(t, "generation")
	}
	return out
}

// TestMetricsHelperReadsBothShapes pins the helper against both /metrics bodies
// without a browser or the network. The bug it replaces decoded only per_tenant,
// so a keyed daemon's redacted aggregate yielded a nil map and a silent zero,
// and a zero read before compares equal to a zero read after.
func TestMetricsHelperReadsBothShapes(t *testing.T) {
	var body string
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer srv.Close()
	t.Setenv("WAXSEAL_KEY", "KEYA")

	// Per-tenant detail: counters sum across tenants and generation is readable.
	body = `{"tenants":2,"per_tenant":{
		"alice":{"generation":3,"session_live":true,"attest_kind":"integrity","last_browser_proof_age_secs":null,"player_contexts":2,"attestations":1,"escalations":0,"player_context_failures":4},
		"bob":{"generation":1,"session_live":false,"attest_kind":"","last_browser_proof_age_secs":7,"player_contexts":5,"attestations":2,"escalations":1,"player_context_failures":0}}}`
	if got := playerContexts(t, srv.URL); got != 7 {
		t.Errorf("per-tenant player_contexts = %d, want 7", got)
	}
	if gotKey != "KEYA" {
		t.Errorf("X-API-Key = %q, want %q", gotKey, "KEYA")
	}
	em := readEscalationMetrics(t, srv.URL)
	want := escalationMetrics{Generation: 4, GenerationKnown: true, Attestations: 3, Escalations: 1, PlayerContextFailures: 4}
	if em != want {
		t.Errorf("per-tenant escalation metrics = %+v, want %+v", em, want)
	}

	// The redacted aggregate: lifetime counters survive, per-tenant state does not.
	body = `{"redacted":true,"aggregate":{"player_contexts":9,"attestations":3,"escalations":2,"player_context_failures":1}}`
	if got := playerContexts(t, srv.URL); got != 9 {
		t.Errorf("aggregate player_contexts = %d, want 9", got)
	}
	em = readEscalationMetrics(t, srv.URL)
	want = escalationMetrics{Generation: 0, GenerationKnown: false, Attestations: 3, Escalations: 2, PlayerContextFailures: 1}
	if em != want {
		t.Errorf("aggregate escalation metrics = %+v, want %+v", em, want)
	}
}

// TestMetricsHelperRejectsWrongShapes pins that the helper does not read a
// non-200 or a tenant-less body as the redacted aggregate, since either would
// otherwise fail later with a message blaming redaction.
func TestMetricsHelperRejectsWrongShapes(t *testing.T) {
	var status int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()

	status, body = http.StatusUnauthorized, `{"error":"nope","code":"unauthorized"}`
	if _, err := fetchMetrics(srv.URL, ""); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Errorf("non-200: err = %v, want one naming status 401", err)
	}
	status, body = http.StatusOK, `{"tenants":0,"per_tenant":{}}`
	if _, err := fetchMetrics(srv.URL, ""); err == nil || !strings.Contains(err.Error(), "no tenants") {
		t.Errorf("tenant-less: err = %v, want one saying no tenants", err)
	}
	// The redacted body is recognised by its flag, not by an empty per_tenant.
	status, body = http.StatusOK, `{"redacted":true,"aggregate":{"player_contexts":3}}`
	m, err := fetchMetrics(srv.URL, "")
	if err != nil || m.detailed {
		t.Fatalf("redacted: (detailed=%v, %v), want the aggregate shape", m.detailed, err)
	}
	if got, err := m.lookup("player_contexts"); err != nil || got != 3 {
		t.Errorf("redacted player_contexts = (%d, %v), want (3, nil)", got, err)
	}
}

// TestMetricsHelperFailsOnMissingCounter pins that an absent counter stops the
// test rather than reading as zero, which is the whole defect being fixed here.
func TestMetricsHelperFailsOnMissingCounter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"redacted":true,"aggregate":{"mints":1}}`)
	}))
	defer srv.Close()

	m := readMetrics(t, srv.URL)
	v, err := m.lookup("player_contexts")
	if err == nil {
		t.Fatalf("lookup of an absent counter returned %d and no error, want an error", v)
	}
	// The message has to name the counter and the shape, or the next reader cannot
	// tell an absent counter from a daemon that redacts.
	for _, want := range []string{"player_contexts", "redacted aggregate", "mints"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// The reader that does fatal must be built on the same lookup.
	if got, err := m.lookup("mints"); err != nil || got != 1 {
		t.Errorf("lookup(mints) = (%d, %v), want (1, nil)", got, err)
	}
}
