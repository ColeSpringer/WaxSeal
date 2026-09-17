package server_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/server"
)

// TestPlayerContextShapeContract holds the three descriptions of the
// /player-context response together: the browser struct the handler embeds, the
// client struct a consumer decodes into, and the README block that is the
// authoritative contract. Each is reduced to the same shape, JSON key to the kind
// of its value, so a field added to one and forgotten in the others, or typed
// differently in one of them, fails here instead of reaching a consumer.
//
// The test lives in the external server_test package so it can import both
// browser and client; neither imports server, so there is no cycle.
func TestPlayerContextShapeContract(t *testing.T) {
	browserShape := structShape(reflect.TypeOf(browser.PlayerContext{}))
	clientShape := structShape(reflect.TypeOf(client.PlayerContext{}))

	// The server embeds browser.PlayerContext and adds session_generation, so the
	// client type carries exactly one key the browser type does not.
	want := maps.Clone(browserShape)
	want["session_generation"] = "number"
	if diff := shapeDiff(want, clientShape); diff != "" {
		t.Errorf("client.PlayerContext drifted from browser.PlayerContext plus session_generation:\n%s", diff)
	}
	if diff := shapeDiff(want, readmeResponseShape(t, "/player-context")); diff != "" {
		t.Errorf("the README /player-context block drifted from the structs (the README shape is the contract):\n%s", diff)
	}
}

// TestSessionShapeContract holds the /session response struct and the README
// block that documents it together, the way TestPlayerContextShapeContract does
// for /player-context. WaxTap reads the identity from this response, so a key
// renamed on one side and not the other fails here.
func TestSessionShapeContract(t *testing.T) {
	if diff := shapeDiff(structShape(reflect.TypeOf(server.SessionResponse{})), readmeResponseShape(t, "/session")); diff != "" {
		t.Errorf("the README /session block drifted from server.SessionResponse (the README shape is the contract):\n%s", diff)
	}
}

// shape maps a JSON key to the kind of its value: string, number, bool, array,
// object, or null. A key inside a nested object, or inside the objects of an
// array, is flattened as "<parent>.<child>", so audio_formats and thumbnails are
// compared rung by rung.
type shape map[string]string

// structShape derives a struct type's wire shape from its json tags. Fields
// without a json tag, and tagged "-", are skipped the way encoding/json skips
// them.
func structShape(typ reflect.Type) shape {
	out := make(shape)
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		addTypeShape(out, name, f.Type)
	}
	return out
}

// addTypeShape records the wire kind encoding/json gives a Go type, and the
// fields of a struct or a slice of structs under name.
func addTypeShape(out shape, name string, t reflect.Type) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		out[name] = "string"
	case reflect.Bool:
		out[name] = "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		out[name] = "number"
	case reflect.Slice, reflect.Array:
		out[name] = "array"
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.Struct {
			for k, kind := range structShape(elem) {
				out[name+"."+k] = kind
			}
		}
	case reflect.Struct:
		out[name] = "object"
		for k, kind := range structShape(t) {
			out[name+"."+k] = kind
		}
	case reflect.Map:
		out[name] = "object"
	default:
		out[name] = t.Kind().String()
	}
}

// addValueShape records the kind of a decoded JSON value. The objects of an
// array contribute the union of their keys, so a second example object that
// lists only the fields that differ still counts.
func addValueShape(out shape, name string, v any) {
	switch v := v.(type) {
	case string:
		out[name] = "string"
	case float64:
		out[name] = "number"
	case bool:
		out[name] = "bool"
	case nil:
		out[name] = "null"
	case []any:
		out[name] = "array"
		for _, e := range v {
			if obj, ok := e.(map[string]any); ok {
				for k, ev := range obj {
					addValueShape(out, name+"."+k, ev)
				}
			}
		}
	case map[string]any:
		out[name] = "object"
		for k, ev := range v {
			addValueShape(out, name+"."+k, ev)
		}
	}
}

// shapeDiff reports the keys missing from got, the keys got has that want does
// not, and the keys whose kinds differ. It returns "" when the shapes agree.
func shapeDiff(want, got shape) string {
	var missing, extra, differ []string
	for k, kind := range want {
		switch g, ok := got[k]; {
		case !ok:
			missing = append(missing, k)
		case g != kind:
			differ = append(differ, fmt.Sprintf("%s (%s, want %s)", k, g, kind))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	var b strings.Builder
	for _, sec := range []struct {
		label string
		keys  []string
	}{{"missing", missing}, {"unexpected", extra}, {"kind differs", differ}} {
		if len(sec.keys) > 0 {
			slices.Sort(sec.keys)
			b.WriteString("  " + sec.label + ": " + strings.Join(sec.keys, ", ") + "\n")
		}
	}
	return b.String()
}

// readmeResponseShape returns the shape of the README response example under the
// first "###" heading containing heading. It locates the block by that heading
// and by the "// response" line that opens it, so the section's request example
// and a neighbouring endpoint's block cannot leak keys in, then decodes the
// example with its comments stripped.
func readmeResponseShape(t *testing.T, heading string) shape {
	t.Helper()
	raw, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	i := 0
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "###") && strings.Contains(lines[i], heading) {
			break
		}
	}
	if i == len(lines) {
		t.Fatalf("README has no %s heading", heading)
	}
	// The first fenced block after the heading whose first line is "// response"
	// is the documented response shape.
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "##") && !strings.Contains(lines[i], heading) {
			t.Fatalf("README %s section ends before a // response block", heading)
		}
		if !strings.HasPrefix(lines[i], "```") {
			continue
		}
		end := i + 1
		for ; end < len(lines) && !strings.HasPrefix(lines[end], "```"); end++ {
		}
		if end == len(lines) {
			t.Fatalf("README has an unterminated fenced block after %s", heading)
		}
		body := lines[i+1 : end]
		if len(body) > 0 && strings.TrimSpace(body[0]) == "// response" {
			var top map[string]any
			if err := json.Unmarshal([]byte(stripLineComments(strings.Join(body, "\n"))), &top); err != nil {
				t.Fatalf("README %s response block is not JSON once its comments are stripped: %v", heading, err)
			}
			out := make(shape)
			for k, v := range top {
				addValueShape(out, k, v)
			}
			return out
		}
		i = end
	}
	t.Fatalf("README %s section has no // response block", heading)
	return nil
}

// stripLineComments removes // comments from a JSON-with-comments document. It
// tracks string literals, so a "//" inside a URL survives.
func stripLineComments(src string) string {
	var b strings.Builder
	inString := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inString && c == '\\' && i+1 < len(src):
			b.WriteByte(c)
			i++
			c = src[i]
		case c == '"':
			inString = !inString
		case !inString && c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			i-- // leave the newline for the loop to copy
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
