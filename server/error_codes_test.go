package server_test

import (
	"testing"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/server"
)

// TestErrorCodeContract pins the error-code wire values and guards the two
// declarations against drift. The server emits these codes, and client declares
// matching values so consumers do not need to import the server package and its
// Chromium dependencies. A rename must update both declarations and the README
// table. This test catches a mismatch before it breaks a consumer's
// `apiErr.Code == client.Code*` comparison.
func TestErrorCodeContract(t *testing.T) {
	codes := []struct {
		name           string
		server, client string
		want           string
	}{
		{"unauthorized", server.CodeUnauthorized, client.CodeUnauthorized, "unauthorized"},
		{"invalid-request", server.CodeInvalidRequest, client.CodeInvalidRequest, "invalid-request"},
		{"mint-failed", server.CodeMintFailed, client.CodeMintFailed, "mint-failed"},
		{"video-unavailable", server.CodeVideoUnavailable, client.CodeVideoUnavailable, "video-unavailable"},
		{"timeout", server.CodeTimeout, client.CodeTimeout, "timeout"},
		{"player-context-failed", server.CodePlayerContextFailed, client.CodePlayerContextFailed, "player-context-failed"},
		{"no-session", server.CodeNoSession, client.CodeNoSession, "no-session"},
	}
	for _, c := range codes {
		if c.server != c.want {
			t.Errorf("server.Code %s = %q, want %q (update the README error-code table)", c.name, c.server, c.want)
		}
		if c.client != c.want {
			t.Errorf("client.Code %s = %q, want %q (drifted from the server's wire value)", c.name, c.client, c.want)
		}
	}
}
