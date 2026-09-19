package server_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/internal/readmedoc"
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

// TestGetPotShapeContract and TestReportShapeContract do for the two remaining
// documented responses what the tests above do for /player-context and /session.
// Both handlers used to write map literals, which no shape could be read from.
func TestGetPotShapeContract(t *testing.T) {
	if diff := shapeDiff(structShape(reflect.TypeOf(server.TokenResponse{})), readmeResponseShape(t, "/get_pot")); diff != "" {
		t.Errorf("the README /get_pot block drifted from server.TokenResponse (the README shape is the contract):\n%s", diff)
	}
}

func TestReportShapeContract(t *testing.T) {
	if diff := shapeDiff(structShape(reflect.TypeOf(server.ReportResponse{})), readmeResponseShape(t, "/report")); diff != "" {
		t.Errorf("the README /report block drifted from server.ReportResponse (the README shape is the contract):\n%s", diff)
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

// readmeResponseShape reduces the README response example for heading to a
// shape. internal/readmedoc locates the block; only the reduction belongs here.
func readmeResponseShape(t *testing.T, heading string) shape {
	t.Helper()
	raw, err := readmedoc.Response("../README.md", heading)
	if err != nil {
		t.Fatalf("locate the README %s response block: %v", heading, err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("README %s response block is not JSON once its comments are stripped: %v", heading, err)
	}
	out := make(shape)
	for k, v := range top {
		addValueShape(out, k, v)
	}
	return out
}
