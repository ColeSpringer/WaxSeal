package browser

import (
	"regexp"
	"strings"
	"unicode"
)

// MaxContentBindingBytes is the maximum content_binding size accepted by
// token-minting endpoints. The limit rejects accidental oversized inputs without
// imposing a format on generic bindings.
const MaxContentBindingBytes = 4096

// videoIDPattern defines the bare video ID format accepted by WaxSeal. YouTube
// video IDs are currently 11 characters, but the wider bound avoids making
// that length part of the API contract.
var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidVideoID reports whether s is a syntactically valid bare video ID. It
// does not verify that the video exists.
func ValidVideoID(s string) bool { return videoIDPattern.MatchString(s) }

// URLBindingWarning explains why a URL-shaped binding is a mistake: the token
// mints, but YouTube's SABR layer rejects it. The text names both scopes
// because scope is optional on /get_pot and absent from the CLI, so a
// player-only line would mislead a gvs caller.
const URLBindingWarning = "looks like a URL; a token binds a bare identifier " +
	"(a video ID for player scope, or visitor_data for gvs), not a URL, so YouTube rejects it"

// URLBindingWarningFor returns the warning for a content_binding value that
// looks like a pasted URL (see LooksLikeWatchURL). field is the caller's name
// for the input ("content_binding" for the HTTP API, "content-binding" for the
// CLI flag) and prefixes URLBindingWarning. For a normal binding, warn is false
// and msg is empty. CLI and HTTP share it so their warnings cannot drift.
func URLBindingWarningFor(field, value string) (msg string, warn bool) {
	if !LooksLikeWatchURL(value) {
		return "", false
	}
	return field + " " + URLBindingWarning, true
}

// LooksLikeWatchURL reports whether s looks like a pasted YouTube link, not the
// bare identifier a token binds. It backs the content_binding warning and the
// landing-video error. The host match catches scheme-less pastes, but it also
// flags "youtube.com:4416", a valid --addr (see hasScheme in cmd/waxseal).
func LooksLikeWatchURL(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	lower := strings.ToLower(s)
	return strings.Contains(lower, "youtube.com") || strings.Contains(lower, "youtu.be")
}

// HasControlChars reports whether s contains a Unicode control character (C0
// 0x00-0x1F, DEL 0x7F, or C1 0x80-0x9F). Bindings are printable (video IDs,
// base64url visitor_data), so one marks a binding malformed; CJK and emoji
// pass. So does invalid UTF-8, which decodes and JSON-encodes as U+FFFD; add
// utf8.ValidString only if content_binding gains a strict UTF-8 contract.
func HasControlChars(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}
