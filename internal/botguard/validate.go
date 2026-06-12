// Package botguard holds the pure-Go parts of the POT flow: challenge
// parse/descramble, GenerateIT, mint-driving, and PO-token validation. The
// protobuf field-6 validator is a port of rustypipe's validate_potoken (MIT).
package botguard

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidToken identifies a token that fails validation. Validation errors
// wrap it without including raw token bytes.
var ErrInvalidToken = errors.New("invalid po token")

// ValidatePOToken decodes a websafe-base64 PO token and requires protobuf
// field 6 to be present (the same check rustypipe/BgUtils rely on). It accepts
// both padded and unpadded base64url: the BgUtils u8ToBase64 helper emits
// websafe chars but keeps '=' padding, while other producers strip it.
//
// On success it returns the field-6 bytes for diagnostics. Callers that only need
// validity can ignore them.
func ValidatePOToken(token string) ([]byte, error) {
	raw, err := decodeBase64URL(token)
	if err != nil {
		// Redacted: report length only, never the token text.
		return nil, fmt.Errorf("%w: base64 decode (len=%d): %v", ErrInvalidToken, len(token), err)
	}
	field6, ok := bytesFromProtobuf(raw, 6)
	if !ok {
		return nil, fmt.Errorf("%w: protobuf field 6 absent (decoded %d bytes)", ErrInvalidToken, len(raw))
	}
	return field6, nil
}

// decodeBase64URL tolerates padded and unpadded input by normalizing to raw
// (unpadded) base64url before decoding.
func decodeBase64URL(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}

// bytesFromProtobuf scans a protobuf message for the given field number and, if
// it is a length-delimited (wire type 2) field, returns its bytes. Varint,
// fixed64, and fixed32 fields are skipped. An unknown wire type aborts the scan.
// Port of rustypipe protobuf.rs bytes_from_pb (MIT).
func bytesFromProtobuf(pb []byte, field uint32) ([]byte, bool) {
	i := 0
	for i < len(pb) {
		tag, n := parseVarint(pb[i:])
		if n == 0 {
			return nil, false
		}
		i += n
		fieldNum := uint32(tag >> 3)
		wire := byte(tag & 0x07)

		switch wire {
		case 0: // varint
			_, n := parseVarint(pb[i:])
			if n == 0 {
				return nil, false
			}
			i += n
		case 1: // fixed 64-bit
			i += 8
		case 5: // fixed 32-bit
			i += 4
		case 2: // length-delimited (string/bytes)
			length, n := parseVarint(pb[i:])
			if n == 0 {
				return nil, false
			}
			i += n
			if i+int(length) > len(pb) {
				return nil, false // truncated
			}
			if fieldNum == field {
				out := make([]byte, length)
				copy(out, pb[i:i+int(length)])
				return out, true
			}
			i += int(length)
		default: // wire types 3,4 (groups, deprecated) or invalid
			return nil, false
		}
	}
	return nil, false
}

// parseVarint reads a base-128 varint, returning the value and the number of
// bytes consumed (0 on malformed/empty input).
func parseVarint(b []byte) (uint64, int) {
	var result uint64
	for i := range b {
		result |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return result, i + 1
		}
		if i >= 9 { // varint longer than 10 bytes is invalid for uint64
			return 0, 0
		}
	}
	return 0, 0 // ran off the end without a terminating byte
}
