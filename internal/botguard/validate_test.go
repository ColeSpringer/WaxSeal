package botguard

import (
	"encoding/base64"
	"errors"
	"testing"
)

// Vector lifted from rustypipe protobuf.rs (MIT): a real token whose field 6
// decodes to a known value. Proves our scanner matches the reference.
func TestValidatePOTokenReferenceVector(t *testing.T) {
	const token = "MlJ0C4bw9OdQ1nQ3M4AKb9hdTYvhORYpaKAnKK5fE1Jcf5kd988G3WHrMWU-nCS0nDwoHlg27bfcBHpSg0_F3aaXu3da8V2QE3q0M_97L-iBTWF0"
	const wantField6 = "dAuG8PTnUNZ0NzOACm_YXU2L4TkWKWigJyiuXxNSXH-ZHffPBt1h6zFlPpwktJw8KB5YNu233AR6UoNPxd2ml7t3WvFdkBN6tDP_ey_ogU1hdA=="

	field6, err := ValidatePOToken(token)
	if err != nil {
		t.Fatalf("validate reference token: %v", err)
	}
	got := base64.URLEncoding.EncodeToString(field6)
	if got != wantField6 {
		t.Fatalf("field6 = %s\nwant     %s", got, wantField6)
	}
}

// The BgUtils u8ToBase64 helper keeps '=' padding while emitting websafe chars;
// other producers strip it. Both must validate.
func TestValidateAcceptsPaddedAndUnpadded(t *testing.T) {
	// protobuf: field 6 (wire type 2), len 3, "abc"
	raw := []byte{0x32, 0x03, 'a', 'b', 'c'}
	padded := base64.URLEncoding.EncodeToString(raw)      // has '='
	unpadded := base64.RawURLEncoding.EncodeToString(raw) // no '='

	for name, tok := range map[string]string{"padded": padded, "unpadded": unpadded} {
		field6, err := ValidatePOToken(tok)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(field6) != "abc" {
			t.Fatalf("%s: field6 = %q", name, field6)
		}
	}
}

func TestValidateRejectsMissingField6(t *testing.T) {
	// Field 1 varint, field 5 bytes, no field 6.
	raw := []byte{0x08, 0x96, 0x01, 0x2a, 0x02, 'h', 'i'}
	_, err := ValidatePOToken(base64.RawURLEncoding.EncodeToString(raw))
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

func TestValidateRejectsBadBase64(t *testing.T) {
	_, err := ValidatePOToken("!!!not base64!!!")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

// A length-delimited field whose length varint exceeds MaxInt64 must be rejected,
// not panic: a signed cast would wrap it negative, letting it pass the bounds
// check and reach make([]byte, length) or a negative slice index.
func TestValidateRejectsOverflowLength(t *testing.T) {
	// 2^63 encoded as a varint: nine 0x80 continuation bytes, then 0x01.
	overflow := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}
	for name, tag := range map[string]byte{
		"field6 (validated, makeslice path)": 0x32, // field 6, wire type 2
		"field1 (skipped, cursor path)":      0x0a, // field 1, wire type 2
	} {
		t.Run(name, func(t *testing.T) {
			raw := append([]byte{tag}, overflow...)
			if _, ok := bytesFromProtobuf(raw, 6); ok {
				t.Errorf("bytesFromProtobuf reported ok for an overflow length")
			}
			if _, err := ValidatePOToken(base64.RawURLEncoding.EncodeToString(raw)); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("ValidatePOToken err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

// A fixed64/fixed32 field with fewer trailing bytes than its width must be
// rejected instead of advancing the cursor past the buffer.
func TestValidateRejectsTruncatedFixed(t *testing.T) {
	for name, raw := range map[string][]byte{
		"fixed64": {0x09, 1, 2, 3}, // field 1 wire 1, only 3 of 8 bytes
		"fixed32": {0x0d, 1, 2},    // field 1 wire 5, only 2 of 4 bytes
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := bytesFromProtobuf(raw, 6); ok {
				t.Errorf("reported ok for a truncated fixed-width field")
			}
		})
	}
}

// Field 6 must be reachable past skipped varint/fixed64/fixed32 fields.
func TestValidateSkipsOtherWireTypes(t *testing.T) {
	raw := []byte{
		0x08, 0x01, // field 1 varint
		0x11, 1, 2, 3, 4, 5, 6, 7, 8, // field 2 fixed64
		0x25, 9, 9, 9, 9, // field 4 fixed32
		0x32, 0x02, 'o', 'k', // field 6 bytes "ok"
	}
	field6, err := ValidatePOToken(base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if string(field6) != "ok" {
		t.Fatalf("field6 = %q", field6)
	}
}
