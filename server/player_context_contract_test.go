package server_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/internal/browser"
)

// TestPlayerContextShapeContract holds the three descriptions of the
// /player-context response together: the browser struct the handler embeds, the
// client struct a consumer decodes into, and the README block that is the
// authoritative contract. A field added to one and forgotten in the others fails
// here instead of reaching a consumer.
//
// The test lives in the external server_test package so it can import both
// browser and client; neither imports server, so there is no cycle.
func TestPlayerContextShapeContract(t *testing.T) {
	browserKeys := jsonKeys(t, reflect.TypeOf(browser.PlayerContext{}))
	clientKeys := jsonKeys(t, reflect.TypeOf(client.PlayerContext{}))

	// The server embeds browser.PlayerContext and adds session_generation, so the
	// client type carries exactly one key the browser type does not.
	wantClient := union(browserKeys, "session_generation")
	if diff := keyDiff(wantClient, clientKeys); diff != "" {
		t.Errorf("client.PlayerContext drifted from browser.PlayerContext plus session_generation:\n%s", diff)
	}

	docKeys := readmePlayerContextKeys(t)
	if diff := keyDiff(wantClient, docKeys); diff != "" {
		t.Errorf("the README /player-context block drifted from the structs (the README shape is the contract):\n%s", diff)
	}
}

// jsonKeys returns the JSON key set of a struct type, flattening one level of
// nested object fields as "<parent>.<child>" so audio_formats and thumbnails are
// compared rung by rung. Fields without a json tag, and tagged "-", are skipped
// the way encoding/json skips them.
func jsonKeys(t *testing.T, typ reflect.Type) map[string]bool {
	t.Helper()
	keys := make(map[string]bool)
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		keys[name] = true
		elem := f.Type
		for elem.Kind() == reflect.Slice || elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() != reflect.Struct {
			continue
		}
		for sub := range jsonKeys(t, elem) {
			keys[name+"."+sub] = true
		}
	}
	return keys
}

// union copies keys and adds extra.
func union(keys map[string]bool, extra ...string) map[string]bool {
	out := make(map[string]bool, len(keys)+len(extra))
	for k := range keys {
		out[k] = true
	}
	for _, k := range extra {
		out[k] = true
	}
	return out
}

// keyDiff reports the keys that differ between want and got, in both directions.
// It returns "" when the two sets are equal.
func keyDiff(want, got map[string]bool) string {
	var missing, extra []string
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	if len(missing) > 0 {
		b.WriteString("  missing: " + strings.Join(sorted(missing), ", ") + "\n")
	}
	if len(extra) > 0 {
		b.WriteString("  unexpected: " + strings.Join(sorted(extra), ", ") + "\n")
	}
	return b.String()
}

// sorted returns in sorted, so a failure reads the same on every run.
func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// readmePlayerContextKeys returns the keys documented in the README's
// /player-context response block, flattened like jsonKeys. It locates the block
// by the heading and by the "// response" line that opens it, so the section's
// request example and the neighbouring /session block cannot leak keys in.
func readmePlayerContextKeys(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	i := 0
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "###") && strings.Contains(lines[i], "/player-context") {
			break
		}
	}
	if i == len(lines) {
		t.Fatal("README has no /player-context heading")
	}
	// The first fenced block after the heading whose first line is "// response"
	// is the documented response shape.
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "##") && !strings.Contains(lines[i], "/player-context") {
			t.Fatal("README /player-context section ends before a // response block")
		}
		if !strings.HasPrefix(lines[i], "```") {
			continue
		}
		end := i + 1
		for ; end < len(lines) && !strings.HasPrefix(lines[end], "```"); end++ {
		}
		if end == len(lines) {
			t.Fatal("README has an unterminated fenced block after /player-context")
		}
		body := lines[i+1 : end]
		if len(body) > 0 && strings.TrimSpace(body[0]) == "// response" {
			return jsoncKeys(strings.Join(body, "\n"))
		}
		i = end
	}
	t.Fatal("README /player-context section has no // response block")
	return nil
}

// jsoncKeys scans a JSON-with-comments object literal and returns its keys,
// flattened as "<parent>.<child>" for keys inside a nested object or array of
// objects. It tracks strings so a "//" inside a URL is not read as a comment and
// a ":" inside a value is not read as a key separator.
func jsoncKeys(src string) map[string]bool {
	keys := make(map[string]bool)
	var stack []string // prefix of each open container, innermost last
	prefix := ""       // current container's key prefix
	pendingKey := ""   // key whose value has not been seen yet
	lastString := ""   // most recently closed string literal
	for i := 0; i < len(src); i++ {
		switch c := src[i]; c {
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
			}
		case '"':
			var b strings.Builder
			for i++; i < len(src) && src[i] != '"'; i++ {
				if src[i] == '\\' && i+1 < len(src) {
					i++
				}
				b.WriteByte(src[i])
			}
			lastString = b.String()
		case ':':
			if prefix == "" {
				keys[lastString] = true
			} else {
				keys[prefix+"."+lastString] = true
			}
			pendingKey = lastString
		case '{', '[':
			stack = append(stack, prefix)
			if pendingKey != "" {
				if prefix == "" {
					prefix = pendingKey
				} else {
					prefix += "." + pendingKey
				}
				pendingKey = ""
			}
		case '}', ']':
			if len(stack) > 0 {
				prefix = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
			pendingKey = ""
		case ',':
			pendingKey = ""
		}
	}
	return keys
}
