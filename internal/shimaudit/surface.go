package shimaudit

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/colespringer/waxseal/internal/jsassets"
	"github.com/colespringer/waxseal/internal/jsruntime/quickjs"
)

// enumerateJS is shared with build/js/capture-globals.mjs so both surfaces are
// read with the same descriptor-based logic.
//
//go:embed enumerate.js
var enumerateJS string

// ShimSurface enumerates the browser-visible window, navigator, and document
// surfaces installed by the committed bundle.
func ShimSurface(ctx context.Context) (Surface, error) {
	return enumerateSurface(ctx, jsassets.BGBundle)
}

// BareRuntimeSurface enumerates the same roots in QuickJS without the bundle.
// Subtracting this baseline excludes engine builtins from shim coverage.
func BareRuntimeSurface(ctx context.Context) (Surface, error) {
	return enumerateSurface(ctx, nil)
}

// enumerateSurface evaluates the shared enumerator in a temporary runtime.
func enumerateSurface(ctx context.Context, bundle []byte) (Surface, error) {
	eng, err := quickjs.NewEngine(ctx, jsassets.QJSWasm, quickjs.Options{PreloadBundle: bundle})
	if err != nil {
		return Surface{}, fmt.Errorf("shimaudit: new engine: %w", err)
	}
	defer func() { _ = eng.Close(ctx) }()

	rt, err := eng.NewRuntime(ctx)
	if err != nil {
		return Surface{}, fmt.Errorf("shimaudit: new runtime: %w", err)
	}
	defer func() { _ = rt.Close(ctx) }()

	out, err := rt.Eval(ctx, enumerateJS)
	if err != nil {
		return Surface{}, fmt.Errorf("shimaudit: enumerate surface: %w", err)
	}
	var s Surface
	if err := json.Unmarshal(out, &s); err != nil {
		return Surface{}, fmt.Errorf("shimaudit: decode surface: %w", err)
	}
	return s.normalized(), nil
}
