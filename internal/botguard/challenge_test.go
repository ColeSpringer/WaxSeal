package botguard

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

// scramble is the inverse of descramble (byte-97, then base64), used to build
// deterministic fixtures without any captured Google data.
func scramble(plain []byte) string {
	out := make([]byte, len(plain))
	for i, b := range plain {
		out[i] = b - 97 // inverse of the wrapping +97
	}
	return base64.StdEncoding.EncodeToString(out)
}

func TestDescrambleRoundTrip(t *testing.T) {
	plain := []byte(`["x",["var globalThis=this;"],[],0,"PROGRAM","globalName"]`)
	got, err := descramble(scramble(plain))
	if err != nil {
		t.Fatalf("descramble: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("descramble round-trip:\n got %s\nwant %s", got, plain)
	}
}

func TestParseChallengeDataInlineJS(t *testing.T) {
	cdata := mustRawArray(t, []any{
		"v", []any{"", "INTERPRETER_JS_HERE"}, []any{}, 0, "PROGRAM", "globalName",
	})
	ch, err := parseChallengeData(cdata)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ch.InterpreterJS != "INTERPRETER_JS_HERE" || ch.Program != "PROGRAM" || ch.GlobalName != "globalName" {
		t.Fatalf("parsed = %+v", ch)
	}
}

func TestParseChallengeDataInterpreterURL(t *testing.T) {
	cdata := mustRawArray(t, []any{
		"v", []any{}, []any{"//www.google.com/js/bg.js"}, 0, "PROGRAM", "globalName",
	})
	ch, err := parseChallengeData(cdata)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ch.InterpreterURL != "//www.google.com/js/bg.js" || ch.InterpreterJS != "" {
		t.Fatalf("parsed = %+v", ch)
	}
}

func TestParseChallengeDataTooShort(t *testing.T) {
	cdata := mustRawArray(t, []any{"v", []any{"x"}})
	if _, err := parseChallengeData(cdata); err == nil {
		t.Fatal("expected error for short challenge array")
	}
}

func TestParseCreateArrayScrambledAndStructured(t *testing.T) {
	inner := []any{"v", []any{"JS"}, []any{}, 0, "PROG", "gn"}
	innerJSON, _ := json.Marshal(inner)

	// Family A: scrambled string at arr[1].
	scrambled := scramble(innerJSON)
	arrA := mustRawArray(t, []any{"hdr", scrambled})
	chA, err := parseCreateArray(arrA)
	if err != nil {
		t.Fatalf("scrambled family: %v", err)
	}
	if chA.InterpreterJS != "JS" || chA.Program != "PROG" {
		t.Fatalf("scrambled parsed = %+v", chA)
	}

	// Family B: structured challenge array at arr[0].
	arrB := mustRawArray(t, []any{inner})
	chB, err := parseCreateArray(arrB)
	if err != nil {
		t.Fatalf("structured family: %v", err)
	}
	if chB.InterpreterJS != "JS" || chB.GlobalName != "gn" {
		t.Fatalf("structured parsed = %+v", chB)
	}
}

func TestParseProvidedChallengeObject(t *testing.T) {
	raw := json.RawMessage(`{
		"interpreterUrl": {"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue": "//www.google.com/js/bg.js"},
		"interpreterHash": "h",
		"program": "PROG",
		"globalName": "gn"
	}`)
	ch, err := ParseProvidedChallenge(raw)
	if err != nil {
		t.Fatalf("object: %v", err)
	}
	if ch.InterpreterURL != "//www.google.com/js/bg.js" || ch.Program != "PROG" || ch.GlobalName != "gn" {
		t.Fatalf("object parsed = %+v", ch)
	}
	if ch.InterpreterJS != "" {
		t.Fatal("object challenge should be URL-only (unresolved)")
	}
}

func TestParseProvidedChallengeArray(t *testing.T) {
	inner := []any{"v", []any{"INLINE_JS"}, []any{}, 0, "PROG", "gn"}
	b, _ := json.Marshal(inner)
	ch, err := ParseProvidedChallenge(b)
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	if ch.InterpreterJS != "INLINE_JS" || ch.Program != "PROG" {
		t.Fatalf("array parsed = %+v", ch)
	}
}

func TestParseProvidedChallengeStringScrambled(t *testing.T) {
	inner := []any{"v", []any{"INLINE_JS"}, []any{}, 0, "PROG", "gn"}
	innerJSON, _ := json.Marshal(inner)
	s, _ := json.Marshal(scramble(innerJSON)) // JSON string of the scrambled payload
	ch, err := ParseProvidedChallenge(s)
	if err != nil {
		t.Fatalf("scrambled string: %v", err)
	}
	if ch.InterpreterJS != "INLINE_JS" || ch.GlobalName != "gn" {
		t.Fatalf("scrambled string parsed = %+v", ch)
	}
}

func TestParseProvidedChallengeBad(t *testing.T) {
	cases := map[string]string{
		"empty":             ``,
		"object no program": `{"interpreterUrl":{"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com/x.js"}}`,
		"object no URL":     `{"program":"P","globalName":"g"}`,
		"garbage string":    `"not a challenge"`,
		"number":            `42`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProvidedChallenge(json.RawMessage(raw)); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestHostAllowlist(t *testing.T) {
	allowed := []string{"google.com", "www.google.com", "youtube.com", "s.youtube.com", "a.b.google.com"}
	denied := []string{"evilgoogle.com", "google.com.evil.com", "googlexcom", "example.com", "notyoutube.com", ""}
	for _, h := range allowed {
		if !hostAllowed(h) {
			t.Errorf("host %q should be allowed", h)
		}
	}
	for _, h := range denied {
		if hostAllowed(h) {
			t.Errorf("host %q should be denied", h)
		}
	}
}

// Stage tagging survives wrapping so drift telemetry can branch on the category.
func TestStageErrorUnwrap(t *testing.T) {
	base := errors.New("boom")
	err := stageErr(StageDescramble, "%w", base)
	se, ok := errors.AsType[*StageError](err)
	if !ok || se.Stage != StageDescramble {
		t.Fatalf("stage not preserved: %v", err)
	}
	if !errors.Is(err, base) {
		t.Fatal("wrapped error not unwrappable")
	}
}

func mustRawArray(t *testing.T, v []any) []json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil {
		t.Fatal(err)
	}
	return arr
}
