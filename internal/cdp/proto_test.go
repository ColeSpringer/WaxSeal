package cdp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// These goldens pin request bytes captured from the previous CDP driver. They are
// fast regression checks for fingerprint-relevant payloads: any drift in the
// launch argv or UA-CH payload fails without needing a browser.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestArgvGolden asserts the cdp launch argv equals the previous driver's formatted
// argv for the same launcher, minus remote-debugging-port (replaced by
// remote-debugging-pipe).
func TestArgvGolden(t *testing.T) {
	const profile = "/waxseal-profile"

	var want []string
	for _, line := range strings.Split(strings.TrimSpace(string(readFixture(t, "argv_baseline.txt"))), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "--remote-debugging-port=") {
			continue // cdp uses the pipe transport instead
		}
		want = append(want, line)
	}
	want = append(want, "--remote-debugging-pipe")
	sort.Strings(want)

	got := BuildArgs(profile, false)
	if len(got) != len(want) {
		t.Fatalf("argv length = %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestArgvHeadfulDropsHeadless confirms headful mode drops the headless flag.
func TestArgvHeadfulDropsHeadless(t *testing.T) {
	for _, a := range BuildArgs("/p", true) {
		if strings.HasPrefix(a, "--headless") {
			t.Errorf("headful argv contains %q", a)
		}
	}
	var hasHeadless bool
	for _, a := range BuildArgs("/p", false) {
		if a == "--headless=new" {
			hasHeadless = true
		}
	}
	if !hasHeadless {
		t.Error("headless argv missing --headless=new")
	}
}

// TestUACHGolden asserts the marshaled Network.setUserAgentOverride matches the
// pinned golden byte-for-byte, including the non-omitempty empty
// model/platformVersion.
//
// The literal below is written for this test and is deliberately independent of
// what internal/browser produces: this pins the wire shape (field order, tags,
// which fields survive omitempty), not the values WaxSeal chooses. They are
// allowed to diverge, and they do, since the browser package now echoes the real
// browser's brands and build version. internal/browser's own TestUAOverride and
// TestUAOverrideFromMetadata pin the producer.
func TestUACHGolden(t *testing.T) {
	const major = "149"
	const full = "149.0.0.0"
	req := NetworkSetUserAgentOverride{
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
		UserAgentMetadata: &UserAgentMetadata{
			Brands: []*UserAgentBrandVersion{
				{Brand: "Chromium", Version: major},
				{Brand: "Not)A;Brand", Version: "24"},
			},
			FullVersionList: []*UserAgentBrandVersion{
				{Brand: "Chromium", Version: full},
				{Brand: "Not)A;Brand", Version: "24.0.0.0"},
			},
			Platform:        "Linux",
			PlatformVersion: "",
			Architecture:    "x86",
			Bitness:         "64",
			Mobile:          false,
			FullVersion:     full,
		},
	}
	got, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := readFixture(t, "ua_ch.json")
	if string(got) != string(want) {
		t.Errorf("UA-CH payload drift:\n got: %s\nwant: %s", got, want)
	}
	// Guard the specific fields omitempty would silently drop.
	for _, must := range []string{`"model":""`, `"platformVersion":""`} {
		if !strings.Contains(string(got), must) {
			t.Errorf("UA-CH payload missing %s (omitempty regression)", must)
		}
	}
}

// TestEvalGolden asserts the Runtime.callFunctionOn params match the pinned golden
// byte-for-byte: the trimmed wrapper, the by-value argument, and field
// order/presence.
func TestEvalGolden(t *testing.T) {
	args, err := buildArgs([]any{"VIDEOID"})
	if err != nil {
		t.Fatal(err)
	}
	req := runtimeCallFunctionOn{
		FunctionDeclaration: formatToJSFunc(`(id) => mint(id)`),
		ObjectID:            "OBJ",
		Arguments:           args,
		ReturnByValue:       true,
		AwaitPromise:        true,
	}
	got, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := readFixture(t, "eval_callfunctionon.json")
	if string(got) != string(want) {
		t.Errorf("callFunctionOn params drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestFormatToJSFunc covers the trim cutset and wrapper.
func TestFormatToJSFunc(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`() => 1`, `function() { return (() => 1).apply(this, arguments) }`},
		{"  () => 1 ;\n", `function() { return (() => 1).apply(this, arguments) }`},
		{"\t\n() => 1\r\n", `function() { return (() => 1).apply(this, arguments) }`},
	} {
		if got := formatToJSFunc(c.in); got != c.want {
			t.Errorf("formatToJSFunc(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEvalResultCoercion covers the coercion semantics expected by browser
// sessions.
func TestEvalResultCoercion(t *testing.T) {
	r := func(s string) EvalResult { return EvalResult{Value: json.RawMessage(s)} }
	for _, c := range []struct {
		name string
		val  EvalResult
		str  string
		i    int
		b    bool
	}{
		{"string", r(`"hi"`), "hi", 0, false},
		{"int", r(`42`), "", 42, false},
		{"float-truncates", r(`12.9`), "", 12, false},
		{"bool-true", r(`true`), "", 0, true},
		{"bool-false", r(`false`), "", 0, false},
		{"big-int", r(`20000`), "", 20000, false},
		{"null", r(`null`), "", 0, false},
		{"empty", EvalResult{}, "", 0, false},
		{"string-not-number", r(`"5"`), "5", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.val.Str(); got != c.str {
				t.Errorf("Str() = %q, want %q", got, c.str)
			}
			if got := c.val.Int(); got != c.i {
				t.Errorf("Int() = %d, want %d", got, c.i)
			}
			if got := c.val.Bool(); got != c.b {
				t.Errorf("Bool() = %v, want %v", got, c.b)
			}
		})
	}
}

// TestIsContextLost covers the retryable RPC errors and the non-retryable ones.
func TestIsContextLost(t *testing.T) {
	for _, m := range []string{
		"Cannot find context with specified id",
		"Execution context was destroyed.",
		"Invalid object id",
	} {
		if !isContextLost(&rpcError{Code: -32000, Message: m}) {
			t.Errorf("isContextLost(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"Some other error", "boom"} {
		if isContextLost(&rpcError{Code: -32000, Message: m}) {
			t.Errorf("isContextLost(%q) = true, want false", m)
		}
	}
	if isContextLost(&EvalError{Text: "x"}) {
		t.Error("isContextLost(*EvalError) = true; JS exceptions must not be treated as context loss")
	}
}

// TestCookieUnmarshalsExtras covers the cookie fields WaxSeal needs beyond name,
// value, domain, path, and flags.
func TestCookieUnmarshalsExtras(t *testing.T) {
	const raw = `{"name":"PREF","value":"x","domain":".youtube.com","path":"/","secure":true,"httpOnly":false,"expires":1750000000,"session":false,"sameSite":"Lax"}`
	var c Cookie
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Name != "PREF" || c.Domain != ".youtube.com" || !c.Secure {
		t.Errorf("base fields = %+v", c)
	}
	if c.Expires != 1750000000 {
		t.Errorf("expires = %v, want 1750000000", c.Expires)
	}
	if c.Session {
		t.Errorf("session = %v, want false", c.Session)
	}
	if c.SameSite != "Lax" {
		t.Errorf("sameSite = %q, want Lax", c.SameSite)
	}
}
