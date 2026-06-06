package quickjs_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/colespringer/waxseal/internal/jsruntime"
)

// DOM fidelity checks run offline in QuickJS. They cover prototype chains,
// createElement typing, native-looking Function.prototype.toString,
// Request/EventTarget, canvas/media probes, and timezone/Date coherence. These
// assert local browser-surface invariants; the live integrity-token outcome
// still needs network and real BotGuard.

// evalTrue asserts a JS boolean expression evaluates to true under the loaded
// bg_bundle (default BrowserProfile applied).
func evalTrue(t *testing.T, rt jsruntime.Runtime, name, expr string) {
	t.Helper()
	out, err := rt.Eval(context.Background(), "Boolean("+expr+")")
	if err != nil {
		t.Errorf("%s: eval error: %v", name, err)
		return
	}
	if string(out) != "true" {
		// Re-eval raw for a useful failure message.
		raw, _ := rt.Eval(context.Background(), "String("+expr+")")
		t.Errorf("%s: got false, want true (value=%s)", name, raw)
	}
}

// awaitTrue resolves promiseExpr and evaluates pred with the result bound to v.
// Separate Eval calls schedule the promise, drain jobs, and read the result.
func awaitTrue(t *testing.T, rt jsruntime.Runtime, name, promiseExpr, pred string) {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.Eval(ctx, "globalThis.__awaitRV = false; ("+promiseExpr+").then(v => { try { globalThis.__awaitRV = !!("+pred+"); } catch (_) {} });"); err != nil {
		t.Errorf("%s: schedule: %v", name, err)
		return
	}
	if _, err := rt.Eval(ctx, "__wx_runTimers()"); err != nil {
		t.Errorf("%s: drain: %v", name, err)
		return
	}
	evalTrue(t, rt, name, "globalThis.__awaitRV === true")
}

func evalString(t *testing.T, rt jsruntime.Runtime, expr string) string {
	t.Helper()
	out, err := rt.Eval(context.Background(), expr)
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("eval %q: not a string: %s", expr, out)
	}
	return s
}

// The canonical chain HTMLDivElement -> HTMLElement -> Element -> Node ->
// EventTarget, and createElement instances satisfy instanceof at every level.
func TestDOMPrototypeChain(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"proto: HTMLDivElement->HTMLElement": "Object.getPrototypeOf(HTMLDivElement.prototype) === HTMLElement.prototype",
		"proto: HTMLElement->Element":        "Object.getPrototypeOf(HTMLElement.prototype) === Element.prototype",
		"proto: Element->Node":               "Object.getPrototypeOf(Element.prototype) === Node.prototype",
		"proto: Node->EventTarget":           "Object.getPrototypeOf(Node.prototype) === EventTarget.prototype",
		"div instanceof HTMLDivElement":      "document.createElement('div') instanceof HTMLDivElement",
		"div instanceof HTMLElement":         "document.createElement('div') instanceof HTMLElement",
		"div instanceof Element":             "document.createElement('div') instanceof Element",
		"div instanceof Node":                "document.createElement('div') instanceof Node",
		"div instanceof EventTarget":         "document.createElement('div') instanceof EventTarget",
		"div.tagName":                        "document.createElement('div').tagName === 'DIV'",
		"div.appendChild works":              "(() => { const d=document.createElement('div'); const s=document.createElement('span'); d.appendChild(s); return d.childNodes[0]===s && s.parentNode===d; })()",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// Window geometry must match browser types. Live BotGuard probes have read
// window.screenY, and modern browsers expose the legacy screenLeft/screenTop
// aliases alongside inner/outer dimensions.
func TestWindowGeometry(t *testing.T) {
	rt := newBundledRT(t)
	for _, prop := range []string{
		"screenX", "screenY", "screenLeft", "screenTop",
		"innerWidth", "innerHeight", "outerWidth", "outerHeight", "devicePixelRatio",
	} {
		evalTrue(t, rt, prop+" is a number", "typeof window."+prop+" === 'number'")
	}
	evalTrue(t, rt, "screenLeft mirrors screenX", "window.screenLeft === window.screenX")
	evalTrue(t, rt, "screenTop mirrors screenY", "window.screenTop === window.screenY")
}

// createElement maps tags to the correct interface (and the media/SVG sub-chains
// hold), so the whole instanceof battery agrees with createElement.
func TestCreateElementTyping(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"canvas":             "document.createElement('canvas') instanceof HTMLCanvasElement",
		"video->media":       "document.createElement('video') instanceof HTMLVideoElement && document.createElement('video') instanceof HTMLMediaElement",
		"audio->media":       "document.createElement('audio') instanceof HTMLAudioElement && document.createElement('audio') instanceof HTMLMediaElement",
		"anchor":             "document.createElement('a') instanceof HTMLAnchorElement",
		"img":                "document.createElement('img') instanceof HTMLImageElement",
		"script":             "document.createElement('script') instanceof HTMLScriptElement",
		"iframe":             "document.createElement('iframe') instanceof HTMLIFrameElement",
		"unknown->Unknown":   "document.createElement('blink-xyz') instanceof HTMLUnknownElement",
		"svg via NS":         "document.createElementNS('http://www.w3.org/2000/svg','svg') instanceof SVGSVGElement",
		"svg->graphics->svg": "document.createElementNS('http://www.w3.org/2000/svg','svg') instanceof SVGGraphicsElement && document.createElementNS('http://www.w3.org/2000/svg','svg') instanceof SVGElement && document.createElementNS('http://www.w3.org/2000/svg','svg') instanceof Element",
		"svg path":           "document.createElementNS('http://www.w3.org/2000/svg','path') instanceof SVGPathElement",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// The standard HTML element battery is present and createElement-coherent. Live
// probes have included window.HTMLMeterElement, so every tag must mint an
// instance of its declared interface and every interface must be a real function
// on window.
func TestFullElementBattery(t *testing.T) {
	rt := newBundledRT(t)
	// tag -> interface name (a representative cross-section incl. the special
	// parents and the probed HTMLMeterElement).
	tagIface := map[string]string{
		"br": "HTMLBRElement", "hr": "HTMLHRElement", "pre": "HTMLPreElement",
		"q": "HTMLQuoteElement", "blockquote": "HTMLQuoteElement", "meter": "HTMLMeterElement",
		"progress": "HTMLProgressElement", "output": "HTMLOutputElement", "details": "HTMLDetailsElement",
		"dialog": "HTMLDialogElement", "fieldset": "HTMLFieldSetElement", "legend": "HTMLLegendElement",
		"h1": "HTMLHeadingElement", "h6": "HTMLHeadingElement", "ins": "HTMLModElement",
		"del": "HTMLModElement", "td": "HTMLTableCellElement", "th": "HTMLTableCellElement",
		"tr": "HTMLTableRowElement", "thead": "HTMLTableSectionElement", "col": "HTMLTableColElement",
		"slot": "HTMLSlotElement", "time": "HTMLTimeElement", "track": "HTMLTrackElement",
		"map": "HTMLMapElement", "area": "HTMLAreaElement", "object": "HTMLObjectElement",
		"embed": "HTMLEmbedElement", "menu": "HTMLMenuElement", "data": "HTMLDataElement",
		"datalist": "HTMLDataListElement", "optgroup": "HTMLOptGroupElement", "caption": "HTMLTableCaptionElement",
	}
	for tag, iface := range tagIface {
		evalTrue(t, rt, "createElement("+tag+") instanceof "+iface,
			"typeof window."+iface+" === 'function' && document.createElement('"+tag+"') instanceof "+iface+
				" && document.createElement('"+tag+"') instanceof HTMLElement")
	}
	// A live-probed interface must no longer be undefined.
	evalTrue(t, rt, "HTMLMeterElement defined", "typeof HTMLMeterElement === 'function'")
}

// Native Function.prototype.toString: every DOM constructor/method/accessor and
// shim host function reports `[native code]`.
func TestNativeToString(t *testing.T) {
	rt := newBundledRT(t)

	exact := map[string]string{
		"document.createElement.toString()":      "function createElement() { [native code] }",
		"HTMLDivElement.toString()":              "function HTMLDivElement() { [native code] }",
		"Function.prototype.toString.toString()": "function toString() { [native code] }",
		"setTimeout.toString()":                  "function setTimeout() { [native code] }",
		"Object.getOwnPropertyDescriptor(Element.prototype,'tagName').get.toString()": "function get tagName() { [native code] }",
	}
	for expr, want := range exact {
		if got := evalString(t, rt, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}

	// Called as Function.prototype.toString.call(fn) too (BotGuard's usual form).
	containsNative := []string{
		"Function.prototype.toString.call(document.createElement)",
		"document.createElement('canvas').getContext.toString()",
		"document.addEventListener.toString()",
		"navigator.javaEnabled.toString()",
		"Math.random.toString()",
		"crypto.getRandomValues.toString()",
		"EventTarget.prototype.addEventListener.toString()",
		"Date.prototype.getTimezoneOffset.toString()",
		"WebGLRenderingContext.prototype.getParameter.toString()",
	}
	for _, expr := range containsNative {
		if got := evalString(t, rt, expr); !strings.Contains(got, "[native code]") {
			t.Errorf("%s = %q, want it to contain [native code]", expr, got)
		}
	}

	// Do not globally fake toString: page/user functions must still report their
	// real source. bgutils-js and challenge JS are page script, not native.
	if got := evalString(t, rt, "(function pageFn(){ return 41 + 1; }).toString()"); strings.Contains(got, "[native code]") {
		t.Errorf("non-native fn falsely reports native: %q", got)
	}
}

// Direct construction of a DOM interface throws "Illegal constructor"; the few
// genuinely-constructable ones (EventTarget, Event) do not.
func TestIllegalConstructor(t *testing.T) {
	rt := newBundledRT(t)
	throws := []string{"HTMLDivElement", "HTMLElement", "Element", "Node", "Document", "HTMLCanvasElement", "VideoTrack"}
	for _, ctor := range throws {
		expr := "(() => { try { new " + ctor + "(); return false; } catch(e){ return e instanceof TypeError && /Illegal constructor/.test(e.message); } })()"
		evalTrue(t, rt, "new "+ctor+" throws", expr)
	}
	evalTrue(t, rt, "new EventTarget() ok", "(() => { try { return new EventTarget() instanceof EventTarget; } catch(e){ return false; } })()")
	evalTrue(t, rt, "new Event() ok", "(() => { const e = new Event('x'); return e instanceof Event && e.type === 'x'; })()")
}

// Date timezone coherence: getTimezoneOffset matches the profile, and the local
// getters are derived from UTC+offset so the whole Date surface agrees. Default
// profile is America/Phoenix, UTC-7 year-round.
func TestDateTimezoneCoherence(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"getTimezoneOffset == 420": "new Date().getTimezoneOffset() === 420",
		// noon UTC on 2021-01-01 is 05:00 local at UTC-7.
		"local hours shifted":  "new Date(Date.UTC(2021,0,1,12,0,0)).getHours() === 5",
		"utc hours intact":     "new Date(Date.UTC(2021,0,1,12,0,0)).getUTCHours() === 12",
		"date fields coherent": "(() => { const d=new Date(Date.UTC(2021,0,1,12,0,0)); return d.getFullYear()===2021 && d.getMonth()===0 && d.getDate()===1; })()",
		// Crossing midnight backward: 02:00 UTC -> 19:00 previous day local at UTC-7.
		"midnight rollback": "(() => { const d=new Date(Date.UTC(2021,0,2,2,0,0)); return d.getHours()===19 && d.getDate()===1; })()",
		// Non-DST: summer and winter dates report the same offset. New York would
		// differ, which is the incoherence this default avoids.
		"non-DST summer==winter": "new Date(Date.UTC(2021,6,1,12,0,0)).getTimezoneOffset() === new Date(Date.UTC(2021,0,1,12,0,0)).getTimezoneOffset()",
		"Intl timezone name":     "Intl.DateTimeFormat().resolvedOptions().timeZone === 'America/Phoenix'",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}

	// A different profile re-pins the offset coherently (Europe/Berlin, +60).
	if _, err := rt.Call(context.Background(), "__wxApplyProfile", map[string]any{
		"timezone": "Europe/Berlin", "utcOffsetMinutes": 60,
	}); err != nil {
		t.Fatalf("apply Berlin profile: %v", err)
	}
	evalTrue(t, rt, "Berlin offset", "new Date().getTimezoneOffset() === -60")
	evalTrue(t, rt, "Berlin local hours", "new Date(Date.UTC(2021,0,1,12,0,0)).getHours() === 13")
}

// Canvas + WebGL: an instanceof-correct surface with plausible WebGL parameters
// and a stable data URL.
func TestCanvasAndWebGL(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"2d context type":   "document.createElement('canvas').getContext('2d') instanceof CanvasRenderingContext2D",
		"2d canvas backref": "(() => { const c=document.createElement('canvas'); return c.getContext('2d').canvas === c; })()",
		"2d methods":        "(() => { const x=document.createElement('canvas').getContext('2d'); return typeof x.fillRect==='function' && typeof x.fillText==='function' && typeof x.getImageData==='function'; })()",
		"measureText":       "document.createElement('canvas').getContext('2d').measureText('hello').width > 0",
		"getImageData":      "(() => { const d=document.createElement('canvas').getContext('2d').getImageData(0,0,2,2); return d instanceof ImageData && d.data.length===16; })()",
		"toDataURL png":     "document.createElement('canvas').toDataURL().indexOf('data:image/png') === 0",
		"webgl type":        "document.createElement('canvas').getContext('webgl') instanceof WebGLRenderingContext",
		"webgl2 type":       "document.createElement('canvas').getContext('webgl2') instanceof WebGL2RenderingContext",
		"webgl vendor":      "document.createElement('canvas').getContext('webgl').getParameter(0x1F00) === 'WebKit'",
		"webgl unmasked":    "(() => { const gl=document.createElement('canvas').getContext('webgl'); const ext=gl.getExtension('WEBGL_debug_renderer_info'); return ext!=null && gl.getParameter(ext.UNMASKED_RENDERER_WEBGL).indexOf('ANGLE')===0; })()",
		"webgl extensions":  "document.createElement('canvas').getContext('webgl').getSupportedExtensions().indexOf('WEBGL_debug_renderer_info') >= 0",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// The broad platform-interface battery is present and native-looking. Live
// probes have included IDBVersionChangeEvent, StylePropertyMap, and
// CanvasCaptureMediaStreamTrack. Events inherit from Event, and the legacy
// element factories (Image/Audio/Option) match createElement behavior.
// MediaRecorderErrorEvent is absent from the Chrome 149 snapshot.
func TestPlatformBattery(t *testing.T) {
	rt := newBundledRT(t)
	present := []string{
		"IDBVersionChangeEvent", "StylePropertyMap", // earlier live-named gaps
		"CanvasCaptureMediaStreamTrack", "MediaStreamTrack", // CanvasCapture... was the latest live probe
		"MediaRecorder", "MediaSource", "AudioContext", "AnalyserNode", "OscillatorNode",
		"IDBDatabase", "IDBKeyRange", "CSSStyleSheet", "WebGLBuffer", "MutationObserver",
		"ResizeObserver", "IntersectionObserver", "ReadableStream", "XMLHttpRequest",
		"DOMException", "Range", "Notification", "RTCPeerConnection", "Animation", "FileReader",
	}
	for _, name := range present {
		evalTrue(t, rt, name+" present+native",
			"typeof window."+name+" === 'function' && Function.prototype.toString.call(window."+name+").indexOf('[native code]') >= 0")
	}
	// Event-family interfaces are constructable and chain to Event.
	evalTrue(t, rt, "IDBVersionChangeEvent chains Event", "new IDBVersionChangeEvent('versionchange') instanceof Event")
	evalTrue(t, rt, "AnimationEvent chains Event", "new AnimationEvent('x') instanceof Event")
	// Legacy element factories stay instanceof-coherent.
	evalTrue(t, rt, "new Image()", "new Image() instanceof HTMLImageElement && new Image(2,3).width === 2")
	evalTrue(t, rt, "new Audio()", "new Audio() instanceof HTMLAudioElement")
	evalTrue(t, rt, "new Option()", "new Option() instanceof HTMLOptionElement")
	// The battery must not overwrite the more specific chains defined above.
	evalTrue(t, rt, "MouseEvent chain intact", "new MouseEvent('click') instanceof UIEvent && new MouseEvent('click') instanceof Event")
	evalTrue(t, rt, "HTMLDivElement intact", "document.createElement('div') instanceof HTMLDivElement")

	// Where a global singleton exists, it must be instanceof its interface.
	coherence := map[string]string{
		"crypto instanceof Crypto":           "crypto instanceof Crypto",
		"crypto.subtle instanceof Subtle":    "crypto.subtle instanceof SubtleCrypto",
		"performance instanceof Performance": "performance instanceof Performance",
		"performance instanceof EventTarget": "performance instanceof EventTarget",
		"performance.now works":              "typeof performance.now() === 'number'",
		"localStorage instanceof Storage":    "localStorage instanceof Storage",
		"history instanceof History":         "history instanceof History",
		"navigator.plugins is PluginArray":   "navigator.plugins instanceof PluginArray",
	}
	for name, expr := range coherence {
		evalTrue(t, rt, name, expr)
	}

	// SVG element interfaces: present, chained, and createElementNS-coherent (the
	// trap named window.SVGSetElement). Value types are presence-only.
	svgNS := "'http://www.w3.org/2000/svg'"
	svg := map[string]string{
		"SVGSetElement present":    "typeof SVGSetElement === 'function'",
		"SVGAngle present":         "typeof SVGAngle === 'function'",
		"SVGPreserveAspectRatio":   "typeof SVGPreserveAspectRatio === 'function'",
		"createNS set coherent":    "document.createElementNS(" + svgNS + ",'set') instanceof SVGSetElement && document.createElementNS(" + svgNS + ",'set') instanceof SVGElement",
		"createNS circle coherent": "document.createElementNS(" + svgNS + ",'circle') instanceof SVGCircleElement && document.createElementNS(" + svgNS + ",'circle') instanceof SVGGeometryElement",
	}
	for name, expr := range svg {
		evalTrue(t, rt, name, expr)
	}
}

// APIs named by the Proxy discovery trap are present and typed.
func TestPhase0ProbedInterfaces(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"VideoTrack":                   "typeof VideoTrack === 'function'",
		"Request":                      "typeof Request === 'function'",
		"window.length":                "window.length === 0",
		"HTMLVideoElement videoTracks": "document.createElement('video').videoTracks instanceof VideoTrackList",
		"media canPlayType":            "document.createElement('video').canPlayType('video/mp4') === 'probably'",
		// Keys match discovery paths so constructor and property probes use the
		// same names.
		"window.PictureInPictureWindow": "typeof window.PictureInPictureWindow === 'function'",
		// Protected Audience APIs.
		"navigator.leaveAdInterestGroup": "typeof navigator.leaveAdInterestGroup === 'function'",
		"navigator.joinAdInterestGroup":  "typeof navigator.joinAdInterestGroup === 'function'",
		"navigator.runAdAuction":         "typeof navigator.runAdAuction === 'function'",
		// Navigator properties backed by their corresponding interfaces.
		"navigator.mediaDevices":                          "navigator.mediaDevices instanceof MediaDevices",
		"navigator.connection":                            "navigator.connection instanceof NetworkInformation",
		"navigator.keyboard":                              "navigator.keyboard instanceof Keyboard",
		"navigator.userActivation":                        "navigator.userActivation instanceof UserActivation",
		"window.Observable":                               "typeof window.Observable === 'function'",
		"window.PressureRecord":                           "typeof window.PressureRecord === 'function'",
		"window.XRInputSource":                            "typeof window.XRInputSource === 'function'",
		"window.IdentityCredentialError":                  "typeof window.IdentityCredentialError === 'function'",
		"window.SpeechSynthesisEvent":                     "typeof window.SpeechSynthesisEvent === 'function'",
		"window.WindowControlsOverlayGeometryChangeEvent": "typeof window.WindowControlsOverlayGeometryChangeEvent === 'function'",
		// XRVisibilityMaskChangeEvent extends Event and is constructable.
		"window.XRVisibilityMaskChangeEvent": "typeof window.XRVisibilityMaskChangeEvent === 'function' && new window.XRVisibilityMaskChangeEvent('x') instanceof Event",
		"window.CrashReportContext":          "typeof window.CrashReportContext === 'function'",
		"navigator.windowControlsOverlay":    "navigator.windowControlsOverlay instanceof WindowControlsOverlay",
		// Delegated Ink API.
		"window.Ink": "typeof window.Ink === 'function'",
		// InkPresenter is internal because the Chrome snapshot does not expose it
		// on window. navigator.ink.requestPresenter still returns an instance.
		"window.InkPresenter hidden":     "typeof window.InkPresenter === 'undefined'",
		"navigator.ink":                  "navigator.ink instanceof Ink",
		"navigator.ink.requestPresenter": "typeof navigator.ink.requestPresenter === 'function'",
		// Prototype-sensitive no-constructor interfaces from the Chrome snapshot.
		"CSSFunctionRule chains CSSGroupingRule":    "typeof window.CSSFunctionRule === 'function' && Object.getPrototypeOf(window.CSSFunctionRule) === window.CSSGroupingRule",
		"SharedStorageAppendMethod chains modifier": "typeof window.SharedStorageAppendMethod === 'function' && Object.getPrototypeOf(window.SharedStorageAppendMethod) === window.SharedStorageModifierMethod",
		"new CSSFunctionRule throws":                "(() => { try { new window.CSSFunctionRule(); return false; } catch(e){ return /Illegal constructor/.test(e.message); } })()",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// TestWebSpeechBattery verifies constructor behavior, inheritance, and aliases
// that the presence-only shim audit does not cover.
func TestWebSpeechBattery(t *testing.T) {
	rt := newBundledRT(t)

	// EventTarget-constructible: `new` works and chains to EventTarget.
	for _, n := range []string{"SpeechRecognition", "SpeechSynthesisUtterance"} {
		evalTrue(t, rt, "new "+n+" instanceof EventTarget",
			"(() => { try { return new "+n+"() instanceof EventTarget; } catch(e){ return false; } })()")
	}
	// webkit-prefixed names share their unprefixed constructor object.
	for alias, base := range map[string]string{
		"webkitSpeechRecognition":      "SpeechRecognition",
		"webkitSpeechRecognitionError": "SpeechRecognitionErrorEvent",
		"webkitSpeechRecognitionEvent": "SpeechRecognitionEvent",
		"webkitSpeechGrammar":          "SpeechGrammar",
		"webkitSpeechGrammarList":      "SpeechGrammarList",
	} {
		evalTrue(t, rt, alias+" aliases "+base, "window."+alias+" === window."+base)
	}

	// Plain constructors do not inherit from EventTarget.
	for _, n := range []string{"SpeechGrammarList", "SpeechRecognitionPhrase"} {
		evalTrue(t, rt, "new "+n+" is plain",
			"(() => { const g = new "+n+"(); return g instanceof Object && !(g instanceof EventTarget); })()")
	}

	// Event subclasses: recognition events extend Event; the synthesis error
	// event extends SpeechSynthesisEvent (which itself extends Event).
	evalTrue(t, rt, "SpeechRecognitionEvent extends Event", "new SpeechRecognitionEvent('x') instanceof Event")
	evalTrue(t, rt, "SpeechRecognitionErrorEvent extends Event", "new SpeechRecognitionErrorEvent('x') instanceof Event")
	evalTrue(t, rt, "SpeechSynthesisErrorEvent extends SpeechSynthesisEvent",
		"(() => { const e = new SpeechSynthesisErrorEvent('x'); return e instanceof SpeechSynthesisEvent && e instanceof Event; })()")

	// These interface objects throw on direct construction.
	for _, n := range []string{"SpeechGrammar", "SpeechSynthesisVoice", "SpeechSynthesis"} {
		evalTrue(t, rt, "new "+n+" throws",
			"(() => { try { new "+n+"(); return false; } catch(e){ return e instanceof TypeError && /Illegal constructor/.test(e.message); } })()")
	}

	// Chrome 149 does not expose the result interfaces as window globals.
	for _, n := range []string{"SpeechRecognitionAlternative", "SpeechRecognitionResult", "SpeechRecognitionResultList"} {
		evalTrue(t, rt, n+" absent", "typeof window."+n+" === 'undefined'")
	}
}

// TestModernNavigatorSurface verifies the navigator interfaces added from the
// Chrome 149 snapshot.
func TestModernNavigatorSurface(t *testing.T) {
	rt := newBundledRT(t)

	instances := map[string]string{
		"navigator.gpu instanceof GPU":                              "navigator.gpu instanceof GPU",
		"navigator.usb instanceof USB":                              "navigator.usb instanceof USB",
		"navigator.serial instanceof Serial":                        "navigator.serial instanceof Serial",
		"navigator.bluetooth instanceof Bluetooth":                  "navigator.bluetooth instanceof Bluetooth",
		"navigator.xr instanceof XRSystem":                          "navigator.xr instanceof XRSystem",
		"navigator.credentials instanceof CredentialsContainer":     "navigator.credentials instanceof CredentialsContainer",
		"navigator.permissions instanceof Permissions":              "navigator.permissions instanceof Permissions",
		"navigator.storage instanceof StorageManager":               "navigator.storage instanceof StorageManager",
		"navigator.locks instanceof LockManager":                    "navigator.locks instanceof LockManager",
		"navigator.mediaSession instanceof MediaSession":            "navigator.mediaSession instanceof MediaSession",
		"navigator.wakeLock instanceof WakeLock":                    "navigator.wakeLock instanceof WakeLock",
		"navigator.serviceWorker instanceof ServiceWorkerContainer": "navigator.serviceWorker instanceof ServiceWorkerContainer",
	}
	for name, expr := range instances {
		evalTrue(t, rt, name, expr)
	}

	// Device, service-worker, and connection instances inherit from EventTarget.
	for _, p := range []string{"usb", "hid", "serial", "bluetooth", "xr", "serviceWorker", "connection"} {
		evalTrue(t, rt, "navigator."+p+" is EventTarget",
			"navigator."+p+" instanceof EventTarget && typeof navigator."+p+".addEventListener === 'function'")
	}

	// Promise-returning members preserve the EventTarget-derived result types.
	awaitTrue(t, rt, "permissions.query() -> PermissionStatus EventTarget",
		"navigator.permissions.query({name:'geolocation'})",
		"v instanceof PermissionStatus && v instanceof EventTarget && typeof v.addEventListener === 'function'")
	awaitTrue(t, rt, "getBattery() -> BatteryManager EventTarget",
		"navigator.getBattery()",
		"v instanceof BatteryManager && v instanceof EventTarget && typeof v.addEventListener === 'function'")
	awaitTrue(t, rt, "serviceWorker.ready -> ServiceWorkerRegistration EventTarget",
		"navigator.serviceWorker.ready",
		"v instanceof ServiceWorkerRegistration && v instanceof EventTarget && typeof v.addEventListener === 'function'")

	// Interface-object prototype chains match the snapshot.
	for _, tc := range []struct{ child, parent string }{
		{"ServiceWorkerContainer", "EventTarget"}, {"ServiceWorkerRegistration", "EventTarget"},
		{"ServiceWorker", "EventTarget"}, {"PermissionStatus", "EventTarget"},
		{"BatteryManager", "EventTarget"}, {"NetworkInformation", "EventTarget"},
		{"XRLayer", "EventTarget"}, {"FederatedCredential", "Credential"},
		{"PasswordCredential", "Credential"}, {"PublicKeyCredential", "Credential"},
		{"XRWebGLLayer", "XRLayer"},
	} {
		evalTrue(t, rt, tc.child+" chains "+tc.parent,
			"typeof window."+tc.child+" === 'function' && Object.getPrototypeOf(window."+tc.child+") === window."+tc.parent)
	}
	evalTrue(t, rt, "PublicKeyCredential is constructable",
		"(() => { try { Reflect.construct(function(){}, [], PublicKeyCredential); return true; } catch(_){ return false; } })()")
	evalTrue(t, rt, "XRWebGLLayer is constructable",
		"(() => { try { Reflect.construct(function(){}, [], XRWebGLLayer); return true; } catch(_){ return false; } })()")

	// Methods are present and native-looking.
	for _, m := range []string{"vibrate", "getBattery", "getGamepads", "share", "canShare", "setAppBadge", "requestMIDIAccess"} {
		evalTrue(t, rt, "navigator."+m+" native",
			"typeof navigator."+m+" === 'function' && Function.prototype.toString.call(navigator."+m+").includes('[native code]')")
	}
	evalTrue(t, rt, "vibrate returns false", "navigator.vibrate(200) === false")
	evalTrue(t, rt, "getGamepads has 4 slots", "navigator.getGamepads().length === 4")

	// Backing window interface objects: present, illegal to construct; the
	// EventTarget-derived ones chain to EventTarget, the plain ones do not.
	for _, c := range []string{"GPU", "USB", "Bluetooth", "XRSystem", "StorageManager", "CredentialsContainer", "MediaSession", "WakeLock"} {
		evalTrue(t, rt, "window."+c+" present", "typeof window."+c+" === 'function'")
		evalTrue(t, rt, "new "+c+" throws",
			"(() => { try { new "+c+"(); return false; } catch(e){ return e instanceof TypeError && /Illegal constructor/.test(e.message); } })()")
	}
	evalTrue(t, rt, "USB chains EventTarget", "Object.getPrototypeOf(USB) === EventTarget")
	evalTrue(t, rt, "GPU is plain (not EventTarget)", "Object.getPrototypeOf(GPU) !== EventTarget")
}

// TestNavigatorStubCallbacks verifies Lock callback values and asynchronous
// geolocation errors.
func TestNavigatorStubCallbacks(t *testing.T) {
	rt := newBundledRT(t)

	// locks.request passes the callback a Lock with the requested name and mode.
	awaitTrue(t, rt, "locks.request grants Lock(name,mode)",
		"navigator.locks.request('res', l => (l instanceof Lock) + ':' + l.name + ':' + l.mode)",
		"v === 'true:res:exclusive'")
	awaitTrue(t, rt, "locks.request honors shared mode",
		"navigator.locks.request('r2', { mode: 'shared' }, l => l.mode)",
		"v === 'shared'")

	// The geolocation error callback does not run before the call returns.
	evalTrue(t, rt, "geolocation denial not synchronous",
		"(() => { let fired = false; navigator.geolocation.getCurrentPosition(() => {}, () => { fired = true; }); return fired === false; })()")
	// It receives a code-1 PositionError after the queued job runs.
	awaitTrue(t, rt, "geolocation denial fires async with code 1",
		"new Promise(res => navigator.geolocation.getCurrentPosition(() => {}, e => res(e)))",
		"v && v.code === 1")
}

// TestNavigatorInterfaceShape verifies that navigator interface members live on
// their prototypes while instance state remains per-object.
func TestNavigatorInterfaceShape(t *testing.T) {
	rt := newBundledRT(t)

	// Persistent sub-interface instances have no own properties.
	for _, p := range []string{
		"gpu", "usb", "hid", "serial", "bluetooth", "xr", "credentials",
		"geolocation", "permissions", "serviceWorker", "storage", "locks",
		"mediaCapabilities", "mediaSession", "presentation", "wakeLock",
		"devicePosture", "virtualKeyboard", "storageBuckets", "scheduling",
		"mediaDevices", "connection", "keyboard", "userActivation",
		"windowControlsOverlay", "ink",
	} {
		evalTrue(t, rt, "navigator."+p+" is a bare instance",
			"Object.getOwnPropertyNames(navigator."+p+").length === 0")
	}

	// Operations live on the interface prototype (inherited, not own), as in Chrome.
	evalTrue(t, rt, "gpu.requestAdapter on prototype",
		"!navigator.gpu.hasOwnProperty('requestAdapter') && GPU.prototype.hasOwnProperty('requestAdapter') && typeof navigator.gpu.requestAdapter === 'function'")
	evalTrue(t, rt, "usb.getDevices on prototype",
		"!navigator.usb.hasOwnProperty('getDevices') && USB.prototype.hasOwnProperty('getDevices')")
	evalTrue(t, rt, "prototype method stays native",
		"Function.prototype.toString.call(navigator.gpu.requestAdapter).includes('[native code]')")

	// Attributes are prototype accessors; on* handlers still round-trip per instance.
	evalTrue(t, rt, "connection.effectiveType via prototype accessor",
		"navigator.connection.effectiveType === '4g' && !navigator.connection.hasOwnProperty('effectiveType')")
	evalTrue(t, rt, "usb.onconnect round-trips without becoming an own prop",
		"(() => { const f = () => {}; navigator.usb.onconnect = f; return navigator.usb.onconnect === f && !navigator.usb.hasOwnProperty('onconnect'); })()")

	// instanceof and EventTarget inheritance remain intact.
	evalTrue(t, rt, "gpu instanceof GPU", "navigator.gpu instanceof GPU")
	evalTrue(t, rt, "usb is EventTarget with addEventListener",
		"navigator.usb instanceof EventTarget && typeof navigator.usb.addEventListener === 'function'")
	evalTrue(t, rt, "no own enumerable members under for-in",
		"(() => { for (const k in navigator.gpu) if (Object.prototype.hasOwnProperty.call(navigator.gpu, k)) return false; return true; })()")

	// Transient interface objects also have no own properties.
	awaitTrue(t, rt, "getBattery() -> bare BatteryManager",
		"navigator.getBattery()",
		"Object.getOwnPropertyNames(v).length === 0 && v instanceof BatteryManager && v.charging === true")
	awaitTrue(t, rt, "permissions.query() -> bare PermissionStatus",
		"navigator.permissions.query({name:'geolocation'})",
		"Object.getOwnPropertyNames(v).length === 0 && v instanceof PermissionStatus && v.state === 'prompt'")
	awaitTrue(t, rt, "locks.request -> bare Lock",
		"navigator.locks.request('res', l => Object.getOwnPropertyNames(l).length === 0 && l.name === 'res')",
		"v === true")
}

// TestProbedInterfaceAdditions verifies interfaces added after discovery runs.
func TestProbedInterfaceAdditions(t *testing.T) {
	rt := newBundledRT(t)

	// PressureObserver is constructible without EventTarget inheritance.
	evalTrue(t, rt, "PressureObserver present", "typeof window.PressureObserver === 'function'")
	evalTrue(t, rt, "PressureObserver constructible",
		"(() => { try { return (new PressureObserver(() => {})) instanceof PressureObserver; } catch (_) { return false; } })()")
	evalTrue(t, rt, "PressureObserver is plain (not EventTarget)",
		"Object.getPrototypeOf(PressureObserver) !== EventTarget && !((new PressureObserver(() => {})) instanceof EventTarget)")

	// CloseWatcher: present, EventTarget-constructible.
	evalTrue(t, rt, "CloseWatcher present", "typeof window.CloseWatcher === 'function'")
	evalTrue(t, rt, "CloseWatcher EventTarget-constructible",
		"(() => { try { return (new CloseWatcher()) instanceof EventTarget; } catch (_) { return false; } })()")
	evalTrue(t, rt, "CloseWatcher chains EventTarget", "Object.getPrototypeOf(CloseWatcher) === EventTarget")

	// WebXR layers are present, illegal to construct, and correctly chained.
	evalTrue(t, rt, "XRCompositionLayer chains XRLayer",
		"typeof window.XRCompositionLayer === 'function' && Object.getPrototypeOf(window.XRCompositionLayer) === window.XRLayer")
	evalTrue(t, rt, "XRLayer chains EventTarget", "Object.getPrototypeOf(window.XRLayer) === EventTarget")
	for _, n := range []string{"XRCylinderLayer", "XRProjectionLayer", "XRQuadLayer", "XREquirectLayer", "XRCubeLayer"} {
		evalTrue(t, rt, n+" chains XRCompositionLayer",
			"typeof window."+n+" === 'function' && Object.getPrototypeOf(window."+n+") === window.XRCompositionLayer")
		evalTrue(t, rt, "new "+n+" throws Illegal constructor",
			"(() => { try { new window."+n+"(); return false; } catch (e) { return e instanceof TypeError && /Illegal constructor/.test(e.message); } })()")
	}
	// The snapshot's constructability probe still detects [[Construct]].
	evalTrue(t, rt, "XRCylinderLayer constructable-probe",
		"(() => { try { Reflect.construct(function(){}, [], XRCylinderLayer); return true; } catch (_) { return false; } })()")

	// Navigation API family: events extend Event; Navigation/NavigationHistoryEntry
	// are EventTarget-derived (illegal ctor); the rest are plain illegal-ctor.
	for _, n := range []string{"NavigateEvent", "NavigationCurrentEntryChangeEvent"} {
		evalTrue(t, rt, n+" extends Event", "new "+n+"('x') instanceof Event")
	}
	for _, n := range []string{"Navigation", "NavigationHistoryEntry"} {
		evalTrue(t, rt, n+" EventTarget-derived illegal-ctor",
			"typeof window."+n+" === 'function' && Object.getPrototypeOf(window."+n+") === EventTarget && (() => { try { new window."+n+"(); return false; } catch (e) { return /Illegal constructor/.test(e.message); } })()")
	}
	for _, n := range []string{"NavigationDestination", "NavigationTransition", "NavigationActivation", "NavigationPrecommitController", "NavigationPreloadManager"} {
		evalTrue(t, rt, n+" present, plain illegal-ctor",
			"typeof window."+n+" === 'function' && (() => { try { new window."+n+"(); return false; } catch (e) { return e instanceof TypeError && /Illegal constructor/.test(e.message); } })()")
	}

	// Trusted Types: window.trustedTypes is the factory value; the policy chain works.
	evalTrue(t, rt, "trustedTypes is a TrustedTypePolicyFactory",
		"typeof window.trustedTypes === 'object' && window.trustedTypes instanceof TrustedTypePolicyFactory")
	evalTrue(t, rt, "trustedTypes.createPolicy works",
		"(() => { const p = trustedTypes.createPolicy('p'); return p instanceof TrustedTypePolicy && p.name === 'p' && p.createHTML('<b>') === '<b>'; })()")
	for _, n := range []string{"TrustedTypePolicyFactory", "TrustedTypePolicy", "TrustedHTML", "TrustedScript", "TrustedScriptURL"} {
		evalTrue(t, rt, n+" present, illegal-ctor",
			"typeof window."+n+" === 'function' && (() => { try { new window."+n+"(); return false; } catch (e) { return /Illegal constructor/.test(e.message); } })()")
	}

	// FedCM: IdentityProvider (plain illegal-ctor); IdentityCredential chains Credential.
	evalTrue(t, rt, "IdentityProvider present, illegal-ctor",
		"typeof window.IdentityProvider === 'function' && (() => { try { new window.IdentityProvider(); return false; } catch (e) { return /Illegal constructor/.test(e.message); } })()")
	evalTrue(t, rt, "IdentityCredential chains Credential",
		"typeof window.IdentityCredential === 'function' && Object.getPrototypeOf(window.IdentityCredential) === window.Credential")

	// Additional interfaces found by shim discovery.
	evalTrue(t, rt, "CSSPositionValue chains CSSStyleValue",
		"typeof window.CSSPositionValue === 'function' && Object.getPrototypeOf(window.CSSPositionValue) === window.CSSStyleValue")
	evalTrue(t, rt, "FetchLaterResult present, illegal-ctor",
		"typeof window.FetchLaterResult === 'function' && (() => { try { new window.FetchLaterResult(); return false; } catch (e) { return /Illegal constructor/.test(e.message); } })()")
	evalTrue(t, rt, "webkitMediaStream aliases MediaStream",
		"window.webkitMediaStream === window.MediaStream && (new webkitMediaStream()) instanceof MediaStream")

	// Additional discovery results.
	evalTrue(t, rt, "GPUQueue present, illegal-ctor",
		"typeof window.GPUQueue === 'function' && (() => { try { new window.GPUQueue(); return false; } catch (e) { return /Illegal constructor/.test(e.message); } })()")
	evalTrue(t, rt, "TextTrackCue EventTarget-derived illegal-ctor",
		"typeof window.TextTrackCue === 'function' && Object.getPrototypeOf(window.TextTrackCue) === EventTarget && (() => { try { new window.TextTrackCue(); return false; } catch (e) { return /Illegal constructor/.test(e.message); } })()")
	evalTrue(t, rt, "crashReport is a CrashReportContext value",
		"typeof window.crashReport === 'object' && window.crashReport instanceof CrashReportContext")
}

// TestSingletonInterfaceShape verifies prototype-resident members on browser
// singleton objects.
func TestSingletonInterfaceShape(t *testing.T) {
	rt := newBundledRT(t)

	for _, tc := range []struct{ expr, name string }{
		{"navigator", "navigator"}, {"crypto", "crypto"}, {"crypto.subtle", "crypto.subtle"},
		{"performance", "performance"}, {"screen", "screen"}, {"location", "location"},
	} {
		evalTrue(t, rt, tc.name+" is a bare instance", "Object.getOwnPropertyNames("+tc.expr+").length === 0")
	}

	// Members resolve through the prototype, values intact, instanceof holds.
	evalTrue(t, rt, "navigator.userAgent via prototype",
		"navigator.userAgent.length > 0 && !navigator.hasOwnProperty('userAgent') && Navigator.prototype.hasOwnProperty('userAgent') && navigator instanceof Navigator")
	evalTrue(t, rt, "crypto.getRandomValues via prototype",
		"typeof crypto.getRandomValues === 'function' && !crypto.hasOwnProperty('getRandomValues') && Crypto.prototype.hasOwnProperty('getRandomValues') && crypto instanceof Crypto")
	evalTrue(t, rt, "crypto.subtle is SubtleCrypto",
		"crypto.subtle instanceof SubtleCrypto && typeof crypto.subtle.digest === 'function'")
	evalTrue(t, rt, "performance.now() via prototype",
		"typeof performance.now() === 'number' && !performance.hasOwnProperty('now') && performance instanceof Performance")
	evalTrue(t, rt, "screen.width via prototype",
		"screen.width > 0 && !screen.hasOwnProperty('width') && screen instanceof Screen")
	evalTrue(t, rt, "location.href via prototype + stringifier",
		"location.href.length > 0 && !location.hasOwnProperty('href') && String(location) === location.href && location instanceof Location")

	// getRandomValues continues to use the host CSPRNG.
	evalTrue(t, rt, "getRandomValues still fills",
		"(() => { const a = new Uint8Array(8); return crypto.getRandomValues(a) === a && a.length === 8; })()")
}

// TestDocumentFonts verifies the [SameObject] FontFaceSet exposed by
// document.fonts without a corresponding window global.
func TestDocumentFonts(t *testing.T) {
	rt := newBundledRT(t)

	evalTrue(t, rt, "document.fonts present", "typeof document.fonts === 'object' && document.fonts !== null")
	evalTrue(t, rt, "document.fonts is [SameObject]", "document.fonts === document.fonts")
	evalTrue(t, rt, "document.fonts is an EventTarget",
		"document.fonts instanceof EventTarget && typeof document.fonts.addEventListener === 'function'")
	evalTrue(t, rt, "document.fonts.constructor is FontFaceSet", "document.fonts.constructor.name === 'FontFaceSet'")
	// window.FontFaceSet remains hidden, and the
	// backing _fonts field is not externally observable.
	evalTrue(t, rt, "window.FontFaceSet hidden", "typeof window.FontFaceSet === 'undefined'")
	evalTrue(t, rt, "document._fonts not observable", "document._fonts === undefined && !('_fonts' in document)")
	// The setlike methods and attributes are available.
	evalTrue(t, rt, "fonts status/size/check",
		"document.fonts.status === 'loaded' && document.fonts.size === 0 && document.fonts.check() === true")
	awaitTrue(t, rt, "fonts.ready resolves to the set", "document.fonts.ready", "v === document.fonts")
}

// TestGetBatteryCached verifies that getBattery returns one promise and manager
// per document.
func TestGetBatteryCached(t *testing.T) {
	rt := newBundledRT(t)
	evalTrue(t, rt, "getBattery returns the cached promise", "navigator.getBattery() === navigator.getBattery()")
	awaitTrue(t, rt, "getBattery resolves to a stable BatteryManager",
		"Promise.all([navigator.getBattery(), navigator.getBattery()])",
		"v[0] === v[1] && v[0] instanceof BatteryManager && v[0].charging === true")
}

// TestStaleUserAgentDataNoThrow verifies that an older NavigatorUAData instance
// survives applying a profile without userAgentData.
func TestStaleUserAgentDataNoThrow(t *testing.T) {
	rt := newBundledRT(t)
	mustEval(t, rt, "globalThis.__wxStaleUAD = navigator.userAgentData;")
	if _, err := rt.Call(context.Background(), "__wxApplyProfile", map[string]any{"userAgentData": nil}); err != nil {
		t.Fatalf("re-apply userAgentData-less profile: %v", err)
	}
	evalTrue(t, rt, "re-profiled navigator has no userAgentData", "navigator.userAgentData === undefined")
	awaitTrue(t, rt, "stale getHighEntropyValues resolves without throwing",
		"__wxStaleUAD.getHighEntropyValues(['uaFullVersion','fullVersionList'])",
		"typeof v.uaFullVersion === 'string' && v.uaFullVersion.length > 0 && Array.isArray(v.fullVersionList)")
}

// The browser-object singletons are real instances (so `navigator instanceof
// Navigator` etc.), and the identity reads coherently from the profile.
func TestBrowserObjectInstances(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"navigator instanceof Navigator":   "navigator instanceof Navigator",
		"screen instanceof Screen":         "screen instanceof Screen",
		"location instanceof Location":     "location instanceof Location",
		"window instanceof Window":         "window instanceof Window",
		"window instanceof EventTarget":    "window instanceof EventTarget",
		"UA is Chrome":                     "/Chrome\\/\\d+/.test(navigator.userAgent)",
		"platform Win32":                   "navigator.platform === 'Win32'",
		"webdriver false":                  "navigator.webdriver === false",
		"languages frozen array":           "Array.isArray(navigator.languages) && Object.isFrozen(navigator.languages)",
		"userAgentData brands":             "navigator.userAgentData.brands.some(b => b.brand.indexOf('Chrom') >= 0)",
		"screen size":                      "screen.width === 1920 && screen.height === 1080",
		"document is HTMLDocument":         "document instanceof HTMLDocument && document instanceof Document",
		"document.body is HTMLBodyElement": "document.body instanceof HTMLBodyElement",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// Fetch interfaces behave (feature-detected by some probes; behavior-real).
func TestFetchInterfaces(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"Request method upper":   "new Request('https://x/', {method:'post'}).method === 'POST'",
		"Request headers type":   "new Request('u').headers instanceof Headers",
		"Headers get/set":        "(() => { const h=new Headers(); h.set('X-A','1'); return h.get('x-a')==='1' && h.has('X-A'); })()",
		"Response ok":            "new Response('body',{status:204}).ok === true",
		"AbortController signal": "(() => { const c=new AbortController(); const a=c.signal.aborted; c.abort(); return a===false && c.signal.aborted===true; })()",
		"URLSearchParams":        "new URLSearchParams('a=1&b=2').get('b') === '2'",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}
