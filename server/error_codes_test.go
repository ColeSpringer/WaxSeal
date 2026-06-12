package server_test

import (
	"testing"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/server"
)

// TestErrorCodeContract keeps the server and client constants aligned with the
// documented wire values.
func TestErrorCodeContract(t *testing.T) {
	codes := []struct {
		name           string
		server, client string
		want           string
	}{
		{"unauthorized", server.CodeUnauthorized, client.CodeUnauthorized, "unauthorized"},
		{"method-not-allowed", server.CodeMethodNotAllowed, client.CodeMethodNotAllowed, "method-not-allowed"},
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
