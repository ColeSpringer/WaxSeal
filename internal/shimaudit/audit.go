// Package shimaudit compares WaxSeal's browser shim with a pinned Chrome global
// surface. It reports probed Chrome APIs missing from the shim, other missing
// Chrome APIs, and shim-installed names absent from Chrome.
//
// The audit itself is pure. Its inputs come from QuickJS enumeration, committed
// fixtures, and the optional online discovery runner.
//
// The initial gate checks name presence from property descriptors. Recorded
// shape, inheritance, and alias details are advisory and do not affect the gate.
//
// Workflow:
//
//	go run ./cmd/waxseal shim coverage
//	make jsbundle
//	go test ./internal/shimaudit/...
//
// To merge missing roots observed by a live VM:
//
//	go run ./cmd/waxseal shim discover --merge internal/shimaudit/fixtures/observed_missing_roots.json
//
// Refresh the Chrome reference on Windows with the pinned Chrome for Testing
// build by running make chrome-globals.
package shimaudit

import (
	"cmp"
	"slices"
)

// Target is one of the three roots the surface is enumerated from. BotGuard
// reads globals through window/navigator/document, so a "root" is the first
// path segment after one of these.
type Target string

const (
	TargetWindow    Target = "window"
	TargetNavigator Target = "navigator"
	TargetDocument  Target = "document"
)

// Targets is the fixed, ordered set of enumeration roots.
var Targets = []Target{TargetWindow, TargetNavigator, TargetDocument}

// Shape is the recorded, advisory fidelity of one named member. Presence and
// descriptor kind come from descriptors and never depend on reading a value;
// the remaining fields are best-effort enrichment (Access records whether the
// value read succeeded). None of this is gated in V1.
type Shape struct {
	Typeof        string `json:"typeof,omitempty"`
	Descriptor    string `json:"descriptor,omitempty"` // "data" | "accessor"
	Own           bool   `json:"own,omitempty"`        // own on the target vs inherited
	Constructable bool   `json:"constructable,omitempty"`
	Event         bool   `json:"event,omitempty"`     // Event-prototype ancestry
	Interface     string `json:"interface,omitempty"` // constructor .name
	Parent        string `json:"parent,omitempty"`    // interface object's [[Prototype]] name
	Alias         string `json:"alias,omitempty"`     // first name sharing this value (pointer-alias)
	Access        string `json:"access,omitempty"`    // "ok" | "threw"
}

// Surface contains the enumerated names and shapes for each target.
type Surface struct {
	Window    map[string]Shape `json:"window"`
	Navigator map[string]Shape `json:"navigator"`
	Document  map[string]Shape `json:"document"`
}

// names returns the name->Shape map for a target (never nil).
func (s Surface) names(t Target) map[string]Shape {
	switch t {
	case TargetWindow:
		return s.Window
	case TargetNavigator:
		return s.Navigator
	case TargetDocument:
		return s.Document
	}
	return nil
}

// Root is the first segment of a path BotGuard probed while it was missing from
// the shim. Its timestamps record missing observations, not current usage.
type Root struct {
	Target               Target `json:"target"`
	Name                 string `json:"name"`
	FirstObservedMissing string `json:"firstObservedMissing,omitempty"`
	LastObservedMissing  string `json:"lastObservedMissing,omitempty"`
}

// Finding is one audited member with its recorded shape, for advisory hints.
type Finding struct {
	Target Target
	Name   string
	Shape  Shape
}

// Report buckets the audit. Only MissingProbedReal is gated.
type Report struct {
	// MissingProbedReal contains probed roots present in Chrome but absent from
	// the shim. This is the gated bucket.
	MissingProbedReal []Finding
	// MissingReal contains other Chrome APIs absent from the shim.
	MissingReal []Finding
	// OverCoverage contains names installed by the shim but absent from Chrome.
	// Subtracting the bare QuickJS surface excludes engine builtins.
	OverCoverage []Finding
	// AbsentFromReference = probed roots not in the reference. Advisory: absence
	// from one clean-baseline capture does not prove a honeypot.
	AbsentFromReference []Finding
}

// Audit compares the shim with the Chrome reference. The bare runtime identifies
// names installed by the shim, and the probed roots determine the gated set.
// Every bucket is sorted by target and name.
func Audit(ref, shim, bare Surface, roots []Root) Report {
	var rep Report

	for _, t := range Targets {
		refNames := ref.names(t)
		shimNames := shim.names(t)
		bareNames := bare.names(t)

		// MissingReal contains Chrome APIs the shim does not define.
		for name, sh := range refNames {
			if _, ok := shimNames[name]; !ok {
				rep.MissingReal = append(rep.MissingReal, Finding{t, name, sh})
			}
		}
		// OverCoverage contains shim-installed names absent from Chrome.
		for name, sh := range shimNames {
			if _, isEngine := bareNames[name]; isEngine {
				continue
			}
			if _, real := refNames[name]; real {
				continue
			}
			rep.OverCoverage = append(rep.OverCoverage, Finding{t, name, sh})
		}
	}

	// Use the reference shape for placement hints when the root exists in Chrome.
	for _, r := range roots {
		refShape, inRef := ref.names(r.Target)[r.Name]
		_, inShim := shim.names(r.Target)[r.Name]
		switch {
		case inRef && !inShim:
			rep.MissingProbedReal = append(rep.MissingProbedReal, Finding{r.Target, r.Name, refShape})
		case !inRef:
			rep.AbsentFromReference = append(rep.AbsentFromReference, Finding{r.Target, r.Name, shim.names(r.Target)[r.Name]})
		}
	}

	sortFindings(rep.MissingProbedReal)
	sortFindings(rep.MissingReal)
	sortFindings(rep.OverCoverage)
	sortFindings(rep.AbsentFromReference)
	return rep
}

// PlacementHint suggests where a missing member belongs in the shim. The
// constructable probe cannot distinguish a usable constructor from an illegal
// constructor interface object, so the WPT/IDL determines that choice.
func PlacementHint(sh Shape) string {
	var hint string
	switch {
	case sh.Event && sh.Parent != "" && sh.Parent != "Event":
		hint = "EVENT_BATTERY-special (extends " + sh.Parent + ")"
	case sh.Event:
		hint = "EVENT_BATTERY (extends Event)"
	case sh.Constructable && sh.Parent == "EventTarget":
		hint = "EventTarget-derived (EventTarget-constructible or no-constructor; see IDL)"
	case sh.Constructable && sh.Parent == "":
		hint = "plain (plain-constructible or illegal-constructor; see IDL)"
	case sh.Constructable:
		hint = "extends " + sh.Parent + " (see IDL)"
	case sh.Typeof == "object":
		hint = "value singleton (profile/value path)"
	case sh.Typeof != "":
		hint = "value (" + sh.Typeof + ")"
	default:
		hint = "presence only"
	}
	if sh.Alias != "" {
		hint += "; pointer-alias of " + sh.Alias
	}
	return hint
}

// sortFindings orders a bucket by (target, name) for deterministic output.
func sortFindings(f []Finding) {
	slices.SortFunc(f, func(a, b Finding) int {
		if c := cmp.Compare(a.Target, b.Target); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}
