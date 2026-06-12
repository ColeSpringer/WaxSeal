// Package innertube fetches BotGuard challenges and guest visitor_data from
// YouTube's InnerTube API. att/get returns structured challenges, and browse
// supplies visitor_data when a caller does not already have it.
//
// Requests use the shared httpx retry and response-limit behavior. Interpreter
// URLs are resolved through botguard.ResolveInterpreter.
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
	// clientVersion is a recent WEB InnerTube version. These guest endpoints
	// require clientName WEB. Callers may replace it with the current version from
	// ytcfg.INNERTUBE_CLIENT_VERSION.
	clientName    = "WEB"
	clientVersion = "2.20260603.05.00"

	maxBody = 4 << 20 // response body cap
)

// att/get returns the bgChallenge, and browse returns visitor_data. Variables let
// tests point these endpoints at an httptest server.
var (
	attGetURL = "https://www.youtube.com/youtubei/v1/att/get?prettyPrint=false"
	browseURL = "https://www.youtube.com/youtubei/v1/browse?prettyPrint=false"
)

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

// bgChallengeEnvelope is the part of the att/get response used by WaxSeal. Field
// names match the camelCase InnerTube wire format.
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

// GenerateVisitorData fetches fresh guest visitor_data via browse. It is used
// only when a caller supplies none of its own.
func GenerateVisitorData(ctx context.Context, client *httpx.Client, userAgent string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"context":  json.RawMessage(defaultContext("")),
		"browseId": "FEwhat_to_watch",
	})
	if err != nil {
		return "", stageErr(botguard.StageTransport, "build browse body: %w", err)
	}

	raw, err := postJSON(ctx, client, browseURL, body, userAgent)
	if err != nil {
		return "", err
	}

	var resp struct {
		ResponseContext struct {
			VisitorData string `json:"visitorData"`
		} `json:"responseContext"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", stageErr(botguard.StageParse, "browse response not JSON: %w", err)
	}
	if resp.ResponseContext.VisitorData == "" {
		return "", stageErr(botguard.StageParse, "visitorData not found in browse response")
	}
	return resp.ResponseContext.VisitorData, nil
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
// categorize them alongside Create/VM/validate failures.
func stageErr(stage botguard.Stage, format string, a ...any) error {
	return &botguard.StageError{Stage: stage, Err: fmt.Errorf(format, a...)}
}
