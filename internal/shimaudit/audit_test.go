package shimaudit

import (
	"slices"
	"testing"
)

// surf is a tiny Surface builder: each "target:name" entry becomes a present
// member with the given shape (or a bare data shape).
func surf(window, navigator, document map[string]Shape) Surface {
	s := Surface{Window: window, Navigator: navigator, Document: document}
	return s.normalized()
}

func names(f []Finding) []string {
	out := make([]string, len(f))
	for i, x := range f {
		out[i] = string(x.Target) + "." + x.Name
	}
	return out
}

// TestAuditMissingProbedRealIsGated verifies that a probed Chrome API missing
// from the shim enters the gated bucket.
func TestAuditMissingProbedRealIsGated(t *testing.T) {
	ref := surf(map[string]Shape{"SpeechRecognitionErrorEvent": {Typeof: "function", Event: true, Parent: "Event"}}, nil, nil)
	shim := surf(map[string]Shape{"Object": {}}, nil, nil)
	bare := surf(map[string]Shape{"Object": {}}, nil, nil)
	roots := []Root{{Target: TargetWindow, Name: "SpeechRecognitionErrorEvent"}}

	rep := Audit(ref, shim, bare, roots)
	if got := names(rep.MissingProbedReal); !slices.Equal(got, []string{"window.SpeechRecognitionErrorEvent"}) {
		t.Fatalf("MissingProbedReal = %v, want [window.SpeechRecognitionErrorEvent]", got)
	}
	// The finding carries the reference shape so the CLI can hint placement.
	if sh := rep.MissingProbedReal[0].Shape; !sh.Event || sh.Parent != "Event" {
		t.Errorf("finding lost reference shape: %+v", sh)
	}
}

// TestAuditHoneypotFilteredByReference verifies that roots absent from the
// Chrome reference are advisory only.
func TestAuditHoneypotFilteredByReference(t *testing.T) {
	ref := surf(map[string]Shape{"SpeechRecognition": {Typeof: "function"}}, nil, nil)
	shim := surf(map[string]Shape{}, nil, nil)
	roots := []Root{
		{Target: TargetWindow, Name: "SpeechRecognition"},      // real -> gated
		{Target: TargetWindow, Name: "$cdc_asdjflkjImNotReal"}, // honeypot -> advisory
	}
	rep := Audit(ref, shim, surf(nil, nil, nil), roots)

	if got := names(rep.MissingProbedReal); !slices.Equal(got, []string{"window.SpeechRecognition"}) {
		t.Errorf("MissingProbedReal = %v, want only the real API", got)
	}
	if got := names(rep.AbsentFromReference); !slices.Equal(got, []string{"window.$cdc_asdjflkjImNotReal"}) {
		t.Errorf("AbsentFromReference = %v, want the honeypot", got)
	}
}

// TestAuditPresentIsNotFlagged verifies that present roots are not reported.
func TestAuditPresentIsNotFlagged(t *testing.T) {
	ref := surf(map[string]Shape{"SpeechRecognition": {Typeof: "function"}}, nil, nil)
	shim := surf(map[string]Shape{"SpeechRecognition": {Typeof: "function"}}, nil, nil)
	roots := []Root{{Target: TargetWindow, Name: "SpeechRecognition"}}

	if rep := Audit(ref, shim, surf(nil, nil, nil), roots); len(rep.MissingProbedReal) != 0 {
		t.Errorf("MissingProbedReal = %v, want empty (shim defines it)", names(rep.MissingProbedReal))
	}
}

// TestAuditOverCoverageExcludesEngineBuiltins verifies that the bare runtime
// baseline removes QuickJS builtins from over-coverage.
func TestAuditOverCoverageExcludesEngineBuiltins(t *testing.T) {
	ref := surf(map[string]Shape{}, nil, nil)
	shim := surf(map[string]Shape{
		"Object":        {Typeof: "function"}, // engine builtin
		"PhantomWidget": {Typeof: "function"}, // shim-installed, not in Chrome
	}, nil, nil)
	bare := surf(map[string]Shape{"Object": {Typeof: "function"}}, nil, nil)

	rep := Audit(ref, shim, bare, nil)
	if got := names(rep.OverCoverage); !slices.Equal(got, []string{"window.PhantomWidget"}) {
		t.Errorf("OverCoverage = %v, want [window.PhantomWidget] (Object filtered by bare)", got)
	}
}

// TestAuditMissingRealLongTail verifies advisory findings across all targets.
func TestAuditMissingRealLongTail(t *testing.T) {
	ref := surf(
		map[string]Shape{"Zeta": {}, "Alpha": {}},
		map[string]Shape{"gpu": {}},
		nil,
	)
	shim := surf(map[string]Shape{"Alpha": {}}, map[string]Shape{}, nil)

	rep := Audit(ref, shim, surf(nil, nil, nil), nil)
	if got := names(rep.MissingReal); !slices.Equal(got, []string{"navigator.gpu", "window.Zeta"}) {
		t.Errorf("MissingReal = %v, want [navigator.gpu window.Zeta] (sorted by target,name)", got)
	}
}

// TestAuditDeterministicOrder verifies sorting by target and name.
func TestAuditDeterministicOrder(t *testing.T) {
	ref := surf(map[string]Shape{"B": {}, "A": {}, "C": {}}, nil, nil)
	shim := surf(map[string]Shape{}, nil, nil)
	rep := Audit(ref, shim, surf(nil, nil, nil), nil)
	if got := names(rep.MissingReal); !slices.Equal(got, []string{"window.A", "window.B", "window.C"}) {
		t.Errorf("MissingReal not sorted: %v", got)
	}
}
