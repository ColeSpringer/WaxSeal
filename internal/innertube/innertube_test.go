package innertube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
)

func TestParseBGChallenge(t *testing.T) {
	raw := []byte(`{
		"bgChallenge": {
			"interpreterUrl": {
				"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue": "//www.google.com/js/th/bg.js"
			},
			"interpreterHash": "abc123",
			"program": "PROGRAM",
			"globalName": "GLOBAL"
		}
	}`)
	ch, err := parseBGChallenge(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ch.InterpreterURL != "//www.google.com/js/th/bg.js" {
		t.Errorf("interpreter URL = %q", ch.InterpreterURL)
	}
	if ch.Program != "PROGRAM" || ch.GlobalName != "GLOBAL" {
		t.Errorf("parsed = %+v", ch)
	}
	if ch.InterpreterJS != "" {
		t.Errorf("interpreter JS should be unresolved, got %q", ch.InterpreterJS)
	}
}

func TestParseBGChallengeMissingFields(t *testing.T) {
	cases := map[string]string{
		"no interpreterUrl": `{"bgChallenge":{"program":"P","globalName":"G"}}`,
		"no program":        `{"bgChallenge":{"interpreterUrl":{"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com/x.js"},"globalName":"G"}}`,
		"empty bgChallenge": `{"bgChallenge":{}}`,
		"not json":          `not json`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseBGChallenge([]byte(raw)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGetChallengeWiresResolution(t *testing.T) {
	// att/get returns a valid bgChallenge whose interpreter host is outside the
	// allowlist, so resolution rejects it before any network call. This verifies
	// This covers fetch, parse, and ResolveInterpreter without contacting that host.
	var gotBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"bgChallenge":{
			"interpreterUrl":{"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//cdn.evil.example/bg.js"},
			"program":"P","globalName":"G"}}`))
	}))
	defer srv.Close()
	defer swap(&attGetURL, srv.URL)()

	_, err := GetChallenge(context.Background(), httpx.New(srv.Client()), "UA/1.0", nil)
	se, ok := errors.AsType[*botguard.StageError](err)
	if !ok || se.Stage != botguard.StageInterp {
		t.Fatalf("want StageInterp error, got %v", err)
	}
	if _, ok := gotBody["engagementType"]; !ok {
		t.Error("att/get body missing engagementType")
	}
	if _, ok := gotBody["context"]; !ok {
		t.Error("att/get body missing context")
	}
}

func TestGetChallengeBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{`))
	}))
	defer srv.Close()
	defer swap(&attGetURL, srv.URL)()

	_, err := GetChallenge(context.Background(), httpx.New(srv.Client()), "UA/1.0", nil)
	se, ok := errors.AsType[*botguard.StageError](err)
	if !ok || se.Stage != botguard.StageParse {
		t.Fatalf("want StageParse error, got %v", err)
	}
}

func TestGetChallengePassesInnertubeContext(t *testing.T) {
	custom := json.RawMessage(`{"client":{"clientName":"WEB","clientVersion":"9.9"}}`)
	var gotCtx json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Context json.RawMessage `json:"context"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotCtx = body.Context
		_, _ = w.Write([]byte(`{`)) // fail fast after capturing the context
	}))
	defer srv.Close()
	defer swap(&attGetURL, srv.URL)()

	_, _ = GetChallenge(context.Background(), httpx.New(srv.Client()), "UA/1.0", custom)
	if string(gotCtx) != string(custom) {
		t.Fatalf("context not passed through: got %s", gotCtx)
	}
}

func TestGenerateVisitorData(t *testing.T) {
	const want = "CgtDZjBSbE5uZDJlQSij6bbFBjIKCgJVUxIEGgAgYA%3D%3D"
	var gotPath, gotBrowseID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			BrowseID string `json:"browseId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotBrowseID = body.BrowseID
		_, _ = w.Write([]byte(`{"responseContext":{"visitorData":"` + want + `"}}`))
	}))
	defer srv.Close()
	defer swap(&browseURL, srv.URL)()

	got, err := GenerateVisitorData(context.Background(), httpx.New(srv.Client()), "UA/1.0")
	if err != nil {
		t.Fatalf("GenerateVisitorData: %v", err)
	}
	if got != want {
		t.Errorf("visitorData = %q, want %q", got, want)
	}
	if gotBrowseID != "FEwhat_to_watch" {
		t.Errorf("browseId = %q", gotBrowseID)
	}
	if gotPath == "" {
		t.Error("no request received")
	}
}

func TestGenerateVisitorDataMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"responseContext":{}}`))
	}))
	defer srv.Close()
	defer swap(&browseURL, srv.URL)()

	_, err := GenerateVisitorData(context.Background(), httpx.New(srv.Client()), "UA/1.0")
	se, ok := errors.AsType[*botguard.StageError](err)
	if !ok || se.Stage != botguard.StageParse {
		t.Fatalf("want StageParse error, got %v", err)
	}
}

func TestGenerateVisitorDataServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	defer swap(&browseURL, srv.URL)()

	// httpx retries 5xx; with a 1-attempt client it still surfaces an error.
	c := httpx.New(srv.Client())
	c.MaxRetries = 0
	if _, err := GenerateVisitorData(context.Background(), c, "UA/1.0"); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestDefaultContextShape(t *testing.T) {
	var ctx struct {
		Client map[string]any `json:"client"`
	}
	if err := json.Unmarshal(defaultContext(""), &ctx); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ctx.Client["clientName"] != clientName {
		t.Errorf("clientName = %v", ctx.Client["clientName"])
	}
	if _, ok := ctx.Client["visitorData"]; ok {
		t.Error("empty visitorData should be omitted")
	}

	var withVD struct {
		Client map[string]any `json:"client"`
	}
	_ = json.Unmarshal(defaultContext("VD"), &withVD)
	if withVD.Client["visitorData"] != "VD" {
		t.Errorf("visitorData not embedded: %v", withVD.Client["visitorData"])
	}
}

// swap sets *p to v and returns a function restoring the old value.
func swap(p *string, v string) func() {
	old := *p
	*p = v
	return func() { *p = old }
}
