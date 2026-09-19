package cdp

import "encoding/json"

// This file holds the CDP request and result structs WaxSeal needs. JSON tags,
// field order, and omitempty choices intentionally match the generated protocol
// types used by the previous CDP driver. Golden tests pin the payloads where
// order affects the browser fingerprint, so do not reorder those structs.

// VersionResult is the Browser.getVersion result.
type VersionResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Product         string `json:"product"`
	Revision        string `json:"revision"`
	UserAgent       string `json:"userAgent"`
	JsVersion       string `json:"jsVersion"`
}

// TargetCreateTarget is the Target.createTarget params. WaxSeal only sets URL;
// Browser.Page fills BrowserContextID from the (incognito) browser.
type TargetCreateTarget struct {
	URL              string `json:"url"`
	BrowserContextID string `json:"browserContextId,omitempty"`
}

type createTargetResult struct {
	TargetID string `json:"targetId"`
}

type attachToTargetParams struct {
	TargetID string `json:"targetId"`
	Flatten  bool   `json:"flatten,omitempty"`
}

type attachToTargetResult struct {
	SessionID string `json:"sessionId"`
}

type closeTargetParams struct {
	TargetID string `json:"targetId"`
}

type createBrowserContextResult struct {
	BrowserContextID string `json:"browserContextId"`
}

type disposeBrowserContextParams struct {
	BrowserContextID string `json:"browserContextId"`
}

type navigateParams struct {
	URL string `json:"url"`
}

type navigateResult struct {
	ErrorText string `json:"errorText,omitempty"`
}

type setBypassCSPParams struct {
	Enabled bool `json:"enabled"`
}

// networkGetCookiesParams is Network.getCookies (per-session, scoped by URL).
type networkGetCookiesParams struct {
	URLs []string `json:"urls,omitempty"`
}

// storageGetCookiesParams is Storage.getCookies (browser-level, scoped by context).
type storageGetCookiesParams struct {
	BrowserContextID string `json:"browserContextId,omitempty"`
}

type getCookiesResult struct {
	Cookies []*Cookie `json:"cookies"`
}

// Cookie is the subset of CDP Network.Cookie WaxSeal reads. Extra fields
// Chromium sends are ignored.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
	Expires  float64 `json:"expires"`  // Unix seconds; Chromium sends -1 for session cookies
	Session  bool    `json:"session"`  // true when the cookie has no persistent expiry
	SameSite string  `json:"sameSite"` // "Strict", "Lax", or "None"; absent when unset
}

// NetworkSetUserAgentOverride is the Network.setUserAgentOverride params. Field
// order and tags are pinned by a golden; UA-CH metadata fidelity depends on it.
type NetworkSetUserAgentOverride struct {
	UserAgent         string             `json:"userAgent"`
	AcceptLanguage    string             `json:"acceptLanguage,omitempty"`
	Platform          string             `json:"platform,omitempty"`
	UserAgentMetadata *UserAgentMetadata `json:"userAgentMetadata,omitempty"`
}

// UserAgentMetadata mirrors Emulation.UserAgentMetadata. Platform,
// PlatformVersion, Architecture, Model, and Mobile deliberately have no omitempty
// tag: empty Model and PlatformVersion must serialize as "" to preserve the
// fingerprint.
type UserAgentMetadata struct {
	Brands          []*UserAgentBrandVersion `json:"brands,omitempty"`
	FullVersionList []*UserAgentBrandVersion `json:"fullVersionList,omitempty"`
	FullVersion     string                   `json:"fullVersion,omitempty"`
	Platform        string                   `json:"platform"`
	PlatformVersion string                   `json:"platformVersion"`
	Architecture    string                   `json:"architecture"`
	Model           string                   `json:"model"`
	Mobile          bool                     `json:"mobile"`
	Bitness         string                   `json:"bitness,omitempty"`
	Wow64           bool                     `json:"wow64,omitempty"`
}

// UserAgentBrandVersion is one Sec-CH-UA brand/version pair.
type UserAgentBrandVersion struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

type runtimeEvaluateParams struct {
	Expression string `json:"expression"`
}

type runtimeEvaluateResult struct {
	Result remoteObject `json:"result"`
}

// runtimeCallFunctionOn is the Runtime.callFunctionOn params. TestEvalGolden pins
// the field order (functionDeclaration, objectId, arguments, returnByValue,
// awaitPromise); do not reorder.
type runtimeCallFunctionOn struct {
	FunctionDeclaration string         `json:"functionDeclaration"`
	ObjectID            string         `json:"objectId,omitempty"`
	Arguments           []callArgument `json:"arguments,omitempty"`
	ReturnByValue       bool           `json:"returnByValue,omitempty"`
	AwaitPromise        bool           `json:"awaitPromise,omitempty"`
}

// callArgument carries a by-value argument (CDP-serialized JSON, never embedded in
// the function source).
type callArgument struct {
	Value    json.RawMessage `json:"value,omitempty"`
	ObjectID string          `json:"objectId,omitempty"`
}

type runtimeCallResult struct {
	Result           remoteObject      `json:"result"`
	ExceptionDetails *exceptionDetails `json:"exceptionDetails,omitempty"`
}

// remoteObject is the subset of Runtime.RemoteObject the eval path reads.
type remoteObject struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
	Description string          `json:"description,omitempty"`
	ObjectID    string          `json:"objectId,omitempty"`
}

// exceptionDetails is the subset of Runtime.ExceptionDetails used to describe a JS
// exception in EvalError.
type exceptionDetails struct {
	Text         string        `json:"text"`
	LineNumber   int           `json:"lineNumber"`
	ColumnNumber int           `json:"columnNumber"`
	Exception    *remoteObject `json:"exception,omitempty"`
}
