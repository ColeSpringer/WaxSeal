package botguard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/colespringer/waxseal/internal/httpx"
)

// GenerateITResult preserves integrity and fallback tokens separately. A valid
// fallback token still makes the attestation successful when no integrity token
// is present. CacheMaxTTL may shorten the validity reported by LifetimeSecs and
// RefreshThreshold, but it never extends it.
type GenerateITResult struct {
	IntegrityToken   string // arr[0]
	LifetimeSecs     int    // arr[1]
	RefreshThreshold int    // arr[2]
	FallbackToken    string // arr[3], websafe fallback PO token
}

// HasIntegrity reports whether the warm-minter path is available.
func (r *GenerateITResult) HasIntegrity() bool { return r != nil && r.IntegrityToken != "" }

// HasFallback reports whether a directly-usable fallback token was issued.
func (r *GenerateITResult) HasFallback() bool { return r != nil && r.FallbackToken != "" }

// GenerateIT posts the botguardResponse and parses the result tuple. All HTTP is
// done in Go. A response carrying only the fallback token is successful. Only a
// response with neither token is an error.
func GenerateIT(ctx context.Context, client *httpx.Client, userAgent, botguardResponse string, ep Endpoint) (*GenerateITResult, error) {
	ep = ep.orDefault()
	body, _ := json.Marshal([]string{RequestKey, botguardResponse})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.GenerateITURL, bytes.NewReader(body))
	if err != nil {
		return nil, stageErr(StageGenerateIT, "build request: %w", err)
	}
	setProtoHeaders(req, userAgent)

	raw, err := client.DoJSON(req, maxChallengeBody)
	if err != nil {
		return nil, stageErr(StageGenerateIT, "%w", err)
	}
	return parseGenerateIT(raw)
}

// parseGenerateIT decodes the [integrityToken, lifetime, refreshThreshold,
// fallbackToken] tuple, treating fallback-only as a successful attestation.
func parseGenerateIT(raw []byte) (*GenerateITResult, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) < 2 {
		return nil, stageErr(StageGenerateIT, "unexpected response shape")
	}
	it := &GenerateITResult{}
	_ = json.Unmarshal(arr[0], &it.IntegrityToken)
	_ = json.Unmarshal(arr[1], &it.LifetimeSecs)
	if len(arr) >= 3 {
		_ = json.Unmarshal(arr[2], &it.RefreshThreshold)
	}
	if len(arr) >= 4 {
		_ = json.Unmarshal(arr[3], &it.FallbackToken)
	}
	if !it.HasIntegrity() && !it.HasFallback() {
		return nil, stageErr(StageGenerateIT, "no integrity or fallback token")
	}
	return it, nil
}
