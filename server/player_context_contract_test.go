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
// client struct a consumer decodes into, and the README block, which is the
// contract. Each reduces to a shape (JSON key to value kind), so a field
// missing or mistyped in one fails here instead of reaching a consumer.
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

// TestSessionShapeContract holds server.SessionResponse to the README /session
// block the way TestPlayerContextShapeContract holds /player-context. WaxTap
// reads the identity from this response, so a renamed key fails here.
func TestSessionShapeContract(t *testing.T) {
	if diff := shapeDiff(structShape(reflect.TypeOf(server.SessionResponse{})), readmeResponseShape(t, "/session")); diff != "" {
		t.Errorf("the README /session block drifted from server.SessionResponse (the README shape is the contract):\n%s", diff)
	}
}

// TestGetPotShapeContract and TestReportShapeContract do the same for /get_pot
// and /report. They depend on those handlers writing structs: a map literal has
// no shape to read.
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

// TestStructShape holds structShape to encoding/json's field rules, so a field
// the encoder writes cannot be missing from a contract test's struct shape.
func TestStructShape(t *testing.T) {
	type Promoted struct {
		A string `json:"a"`
	}
	type ViaPointer struct {
		B bool `json:"b"`
	}
	type inner struct {
		D string `json:"d"`
		//lint:ignore U1000 structShape must skip this unexported field
		hidden string
	}
	type named struct {
		C int `json:"c"`
	}
	type fixture struct {
		Promoted                   // untagged embedded struct: fields promoted
		*ViaPointer                // through a pointer too
		inner                      // unexported, but a struct, so it promotes
		named       `json:"named"` // tagged embedded struct: one named field
		Plain       string         // untagged: the Go name
		Opts        int            `json:",omitempty"` // no name: the Go name
		//lint:ignore U1000 structShape must skip this unexported field
		unexported string
		// Dash is named "-". Skipped follows it with another kind, so keeping
		// Skipped would change the kind of "-".
		//lint:ignore SA5008 tests encoding/json's "-," naming rule
		Dash    string `json:"-,"`
		Skipped bool   `json:"-"`
		// ",string" makes a bool, number, or string a JSON string, following one
		// pointer, and is ignored on other kinds.
		Quoted     int    `json:"quoted,string"`
		QuotedBool bool   `json:"quoted_bool,string"`
		QuotedStr  string `json:"quoted_str,string"`
		QuotedPtr  *int   `json:"quoted_ptr,string"`
		//lint:ignore SA5008 tests that encoding/json ignores ",string" on a slice
		QuotedSlice []int `json:"quoted_slice,string"`
	}
	want := shape{
		"a": "string", "b": "bool", "d": "string",
		"named": "object", "named.c": "number",
		"Plain": "string", "Opts": "number", "-": "string",
		"quoted": "string", "quoted_bool": "string", "quoted_str": "string",
		"quoted_ptr": "string", "quoted_slice": "array",
	}
	if diff := shapeDiff(want, structShape(reflect.TypeOf(fixture{}))); diff != "" {
		t.Errorf("structShape(fixture) differs from encoding/json:\n%s", diff)
	}

	// encoding/json is the reference. The value leaves no field to be dropped or
	// encoded as null, so its encoding must carry exactly want.
	raw, err := json.Marshal(fixture{ViaPointer: &ViaPointer{}, Opts: 1, QuotedPtr: new(int), QuotedSlice: []int{}})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	encoded := make(shape)
	for k, v := range top {
		addValueShape(encoded, k, v)
	}
	if diff := shapeDiff(want, encoded); diff != "" {
		t.Errorf("want disagrees with encoding/json's output %s:\n%s", raw, diff)
	}
}

// shape maps a JSON key to the kind of its value: string, number, bool, array,
// object, or null. A key inside a nested object, or inside the objects of an
// array, is flattened as "<parent>.<child>", so the fields of audio_formats and
// thumbnails entries are compared too.
type shape map[string]string

// structShape derives a struct type's wire shape by encoding/json's rules: it
// skips fields tagged "-" and unexported fields other than embedded structs,
// names an untagged field by its Go name, promotes the fields of an untagged
// embedded struct or pointer to one, and reads a bool or number tagged
// ",string" as a string. It does not resolve two fields with one JSON name; no
// compared struct has them.
func structShape(typ reflect.Type) shape {
	out := make(shape)
	for i := range typ.NumField() {
		f := typ.Field(i)
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		embeddedStruct := f.Anonymous && ft.Kind() == reflect.Struct
		tag := f.Tag.Get("json")
		if tag == "-" || (!f.IsExported() && !embeddedStruct) {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" && embeddedStruct {
			maps.Copy(out, structShape(ft))
			continue
		}
		if name == "" {
			name = f.Name
		}
		if slices.Contains(strings.Split(opts, ","), "string") && quotable(ft.Kind()) {
			out[name] = "string"
			continue
		}
		addTypeShape(out, name, f.Type)
	}
	return out
}

// quotable reports whether encoding/json honors ",string" on kind k: bools,
// numbers, and strings only.
func quotable(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.String, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return false
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
