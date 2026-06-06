package shimaudit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/colespringer/waxseal/internal/jsassets"
	"github.com/colespringer/waxseal/internal/jsruntime/quickjs"
)

// TestEnumerateReadsInheritedAccessorsWithOriginalReceiver verifies that
// inherited getters receive the branded object rather than their prototype.
func TestEnumerateReadsInheritedAccessorsWithOriginalReceiver(t *testing.T) {
	ctx := context.Background()
	eng, err := quickjs.NewEngine(ctx, jsassets.QJSWasm, quickjs.Options{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer func() { _ = eng.Close(ctx) }()
	rt, err := eng.NewRuntime(ctx)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer func() { _ = rt.Close(ctx) }()

	// Model a branded navigator whose prototype getters reject other receivers.
	setup := `
		class Base {}
		class Foo extends Base {}
		var proto = {};
		var inst = Object.create(proto);
		function brand(name, value) {
			Object.defineProperty(proto, name, {
				configurable: true, enumerable: true,
				get: function () {
					if (this !== inst) throw new TypeError('Illegal invocation');
					return value;
				},
			});
		}
		brand('brandedFn', Foo);
		brand('brandedObj', { tag: 1 });
		// A getter that throws for real reasons must still report threw.
		Object.defineProperty(proto, 'alwaysThrows', {
			configurable: true, enumerable: true,
			get: function () { throw new TypeError('boom'); },
		});
		globalThis.navigator = inst;
	`
	out, err := rt.Eval(ctx, setup+"\n"+enumerateJS)
	if err != nil {
		t.Fatalf("eval enumerator: %v", err)
	}
	var s Surface
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("decode surface %s: %v", out, err)
	}

	fn, ok := s.Navigator["brandedFn"]
	if !ok {
		t.Fatalf("brandedFn absent from navigator surface: %+v", s.Navigator)
	}
	if fn.Access != "ok" {
		t.Errorf("brandedFn access = %q, want \"ok\" (inherited getter read with wrong receiver)", fn.Access)
	}
	if fn.Descriptor != "accessor" || fn.Own {
		t.Errorf("brandedFn descriptor/own = %q/%v, want accessor/false", fn.Descriptor, fn.Own)
	}
	// Enrichment recovered only when the getter runs successfully.
	if fn.Typeof != "function" || fn.Interface != "Foo" || fn.Parent != "Base" || !fn.Constructable {
		t.Errorf("brandedFn enrichment = typeof:%q interface:%q parent:%q constructable:%v, want function/Foo/Base/true",
			fn.Typeof, fn.Interface, fn.Parent, fn.Constructable)
	}

	obj, ok := s.Navigator["brandedObj"]
	if !ok || obj.Access != "ok" || obj.Typeof != "object" {
		t.Errorf("brandedObj = %+v, want access:\"ok\" typeof:\"object\"", obj)
	}

	// A getter that always throws still retains its descriptor information.
	thr, ok := s.Navigator["alwaysThrows"]
	if !ok || thr.Access != "threw" || thr.Descriptor != "accessor" {
		t.Errorf("alwaysThrows = %+v, want access:\"threw\" descriptor:\"accessor\"", thr)
	}
}
