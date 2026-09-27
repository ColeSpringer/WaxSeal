package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxseal/internal/botguard"
)

// TestClassifyAttestation covers the integrity-vs-fallback decision and the
// fallback field-6 validation with no browser page. The token vectors mirror
// internal/botguard/validate_test.go.
func TestClassifyAttestation(t *testing.T) {
	// Protobuf field 6 (wire type 2), len 3, "abc": passes field-6 validation.
	validFallback := base64.RawURLEncoding.EncodeToString([]byte{0x32, 0x03, 'a', 'b', 'c'})
	// Field 1 varint + field 5 bytes, no field 6: fails field-6 validation.
	noField6 := base64.RawURLEncoding.EncodeToString([]byte{0x08, 0x96, 0x01, 0x2a, 0x02, 'h', 'i'})

	t.Run("integrity wins even with a fallback present", func(t *testing.T) {
		fallback, err := classifyAttestation(&botguard.GenerateITResult{IntegrityToken: "IT", FallbackToken: validFallback})
		if err != nil || fallback {
			t.Fatalf("classify = (%v, %v), want (false, nil) for the integrity path", fallback, err)
		}
	})
	t.Run("valid fallback only", func(t *testing.T) {
		fallback, err := classifyAttestation(&botguard.GenerateITResult{FallbackToken: validFallback})
		if err != nil || !fallback {
			t.Fatalf("classify = (%v, %v), want (true, nil) for the fallback path", fallback, err)
		}
	})
	t.Run("no token is an error", func(t *testing.T) {
		fallback, err := classifyAttestation(&botguard.GenerateITResult{})
		if err == nil || fallback {
			t.Fatalf("classify = (%v, %v), want a no-token error", fallback, err)
		}
		if !strings.Contains(err.Error(), "no token") {
			t.Errorf("err = %v, want it to mention 'no token'", err)
		}
	})
	t.Run("fallback failing field-6 is an error", func(t *testing.T) {
		fallback, err := classifyAttestation(&botguard.GenerateITResult{FallbackToken: noField6})
		if err == nil || fallback {
			t.Fatalf("classify = (%v, %v), want a field-6 error", fallback, err)
		}
		if !errors.Is(err, botguard.ErrInvalidToken) {
			t.Errorf("err = %v, want it to wrap botguard.ErrInvalidToken", err)
		}
	})
}

// TestMintFallbackDispatch checks that a fallback-attested session's Mint
// serves the stored token without touching the page. It covers dispatch only;
// TestClassifyAttestation covers the decision.
func TestMintFallbackDispatch(t *testing.T) {
	s := &Session{attestKind: "fallback", fallbackToken: "FAKE", lifetimeSecs: 3600}
	res, err := s.Mint(context.Background(), "vid")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if res.Kind != "fallback" || res.Token != "FAKE" || res.Identifier != "vid" {
		t.Errorf("Mint = %+v, want kind=fallback token=FAKE identifier=vid", res)
	}
	if res.Lifetime != 3600 {
		t.Errorf("Lifetime = %d, want 3600", res.Lifetime)
	}
}
