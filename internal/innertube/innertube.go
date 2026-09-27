// Package innertube fetches BotGuard challenges from YouTube's InnerTube
// att/get endpoint and builds the guest WEB context it takes.
package innertube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
)

const (
	// clientName is the InnerTube client the guest att/get call requires.
	// clientVersion is the fallback for a caller with no version of its own; the
	// daemon passes the one it captures (see FallbackClientVersion). YouTube
	// ships a new WEB version most days, so when it falls far behind, refresh it
	// from `go run ./cmd/waxseal doctor 2>&1 | grep client_version`.
	clientName    = "WEB"
	clientVersion = "2.20260901.00.00"

	maxBody = 4 << 20 // response body cap
)

// FallbackClientVersion is the pinned WEB version above. A browser session
// whose page never exposed ytcfg.INNERTUBE_CLIENT_VERSION publishes it through
// /session instead of an empty string, because a consumer builds its InnerTube
// context from that value and a drifted version beats an empty one.
const FallbackClientVersion = clientVersion

// attGetURL is a variable so tests can point it at an httptest server.
var attGetURL = "https://www.youtube.com/youtubei/v1/att/get?prettyPrint=false"

// GetChallenge fetches a structured BotGuard challenge from att/get and resolves
// its interpreter URL. A non-empty innertubeContext is sent verbatim. An empty
// value uses a default guest WEB context. userAgent is the active profile's UA.
func GetChallenge(ctx context.Context, client *httpx.Client, userAgent string, innertubeContext json.RawMessage) (*botguard.Challenge, error) {
	reqCtx := innertubeContext
	if len(reqCtx) == 0 {
		reqCtx = defaultContext("")
	}
	body, err := json.Marshal(map[string]any{
		"context":        json.RawMessage(reqCtx),
		"engagementType": "ENGAGEMENT_TYPE_UNBOUND",
	})
	if err != nil {
		return nil, stageErr(botguard.StageTransport, "build att/get body: %w", err)
	}

	raw, err := postJSON(ctx, client, attGetURL, body, userAgent)
	if err != nil {
		return nil, err
	}

	ch, err := parseBGChallenge(raw)
	if err != nil {
		return nil, err
	}
	if err := botguard.ResolveInterpreter(ctx, client, ch, userAgent); err != nil {
		return nil, err
	}
	return ch, nil
}

// bgChallengeEnvelope is the part of the att/get response WaxSeal reads.
type bgChallengeEnvelope struct {
	BGChallenge struct {
		InterpreterURL struct {
			PrivateDoNotAccessOrElseTrustedResourceURLWrappedValue string `json:"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue"`
		} `json:"interpreterUrl"`
		InterpreterHash string `json:"interpreterHash"`
		Program         string `json:"program"`
		GlobalName      string `json:"globalName"`
	} `json:"bgChallenge"`
}

// parseBGChallenge extracts the interpreter URL, program, and globalName from an
// att/get response into an unresolved botguard.Challenge.
func parseBGChallenge(raw []byte) (*botguard.Challenge, error) {
	var env bgChallengeEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, stageErr(botguard.StageParse, "att/get response not JSON: %w", err)
	}
	bg := env.BGChallenge
	url := bg.InterpreterURL.PrivateDoNotAccessOrElseTrustedResourceURLWrappedValue
	if url == "" {
		return nil, stageErr(botguard.StageParse, "bgChallenge missing interpreterUrl")
	}
	if bg.Program == "" || bg.GlobalName == "" {
		return nil, stageErr(botguard.StageParse, "bgChallenge missing program or globalName")
	}
	return &botguard.Challenge{
		InterpreterURL:  url,
		InterpreterHash: bg.InterpreterHash,
		Program:         bg.Program,
		GlobalName:      bg.GlobalName,
	}, nil
}

// GuestContext builds a guest WEB InnerTube context, adding visitorData when set.
// An empty clientVer uses the package default.
func GuestContext(visitorData, clientVer string) json.RawMessage {
	cv := clientVer
	if cv == "" {
		cv = clientVersion
	}
	clientObj := map[string]any{
		"clientName":    clientName,
		"clientVersion": cv,
		"hl":            "en",
		"gl":            "US",
	}
	if visitorData != "" {
		clientObj["visitorData"] = visitorData
	}
	b, _ := json.Marshal(map[string]any{"client": clientObj})
	return b
}

// defaultContext builds a guest WEB context with the package client version.
func defaultContext(visitorData string) json.RawMessage {
	return GuestContext(visitorData, "")
}

// postJSON posts a JSON body to an InnerTube endpoint through httpx and returns
// the capped response body. InnerTube guest endpoints take a plain JSON body and
// a browser UA (no attestation proto headers).
func postJSON(ctx context.Context, client *httpx.Client, url string, body []byte, userAgent string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, stageErr(botguard.StageTransport, "build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	raw, err := client.DoJSON(req, maxBody)
	if err != nil {
		return nil, stageErr(botguard.StageTransport, "%w", err)
	}
	return raw, nil
}

// stageErr tags InnerTube failures with a botguard.Stage so callers can
// categorize them alongside interpreter-fetch and GenerateIT failures.
func stageErr(stage botguard.Stage, format string, a ...any) error {
	return &botguard.StageError{Stage: stage, Err: fmt.Errorf(format, a...)}
}
