package botguard

import (
	"errors"
	"testing"
)

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

// Stage tagging survives wrapping so callers can branch on the category.
func TestStageErrorUnwrap(t *testing.T) {
	base := errors.New("boom")
	err := stageErr(StageInterp, "%w", base)
	se, ok := errors.AsType[*StageError](err)
	if !ok || se.Stage != StageInterp {
		t.Fatalf("stage not preserved: %v", err)
	}
	if !errors.Is(err, base) {
		t.Fatal("wrapped error not unwrappable")
	}
}
