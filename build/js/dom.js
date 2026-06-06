/*
 * WaxSeal DOM-fidelity core: a contained, separately-tested DOM adapter layer
 * for the QuickJS-on-wazero BotGuard VM.
 *
 * The minimal hand-shim could get a websafe fallback token, but presence-only
 * DOM stubs caused inconsistent browser behavior. For example, defining
 * `HTMLDivElement` while `document.createElement('div')` returns a plain object
 * makes `instanceof HTMLDivElement` false. This module favors coherent behavior
 * over a large list of names.
 *
 * This module provides a behaviorally coherent DOM:
 *   - Real constructor prototype chains
 *     (HTMLDivElement -> HTMLElement -> Element -> Node -> EventTarget), so
 *     `document.createElement('div') instanceof HTMLDivElement` is TRUE and the
 *     whole `instanceof` battery agrees with `createElement`.
 *   - Native-looking `Function.prototype.toString`: every DOM constructor and
 *     method reports `function name() { [native code] }`, never its JS source.
 *   - A canvas/WebGL/media surface with believable, coherent values (the usual
 *     fingerprinting probes) rather than `undefined`.
 *   - SVG chains plus the media-track interfaces the discovery trap has seen in
 *     live probes.
 *
 * This is hand-built instead of using `@thetadevcode/jsdom-minimal`: it avoids
 * Node-specific globals under QuickJS, keeps the bundle small, and is testable
 * offline in the same runtime used in production. If live probes expose a gap,
 * the discovery trap should name it.
 *
 * Date/timezone coherence (`Date.prototype.getTimezoneOffset` + the local
 * getters) lives in `installDateTimezone`, applied by shim.js from the active
 * BrowserProfile.
 */

const G = globalThis;

/*
 * Native Function.prototype.toString.
 *
 * Browsers render built-in functions as `function name() { [native code] }`.
 * Our DOM is JS, so by default `el.addEventListener.toString()` would leak the
 * shim's JS source. We replace Function.prototype.toString so
 * any function we mark renders as native; everything else (bgutils-js, page
 * script) falls through to the genuine toString. The replacement reports itself
 * as native too, and `Function.prototype.toString.toString()` stays consistent.
 */

const NATIVE = new WeakMap(); // fn -> displayed name

/** Mark `fn` so its toString reads `function <name>() { [native code] }`. */
export function markNative(fn, name) {
  if (typeof fn === 'function') NATIVE.set(fn, name == null ? fn.name || '' : name);
  return fn;
}

function installNativeToString() {
  const orig = Function.prototype.toString;
  function toString() {
    const name = NATIVE.get(this);
    if (name !== undefined) return 'function ' + name + '() { [native code] }';
    return orig.call(this);
  }
  NATIVE.set(toString, 'toString');
  try { Object.defineProperty(toString, 'name', { value: 'toString', configurable: true }); } catch (_) {}
  try { Object.defineProperty(toString, 'length', { value: 0, configurable: true }); } catch (_) {}
  // Keep the genuine attribute shape (writable, non-enumerable, configurable).
  Function.prototype.toString = toString;
}
installNativeToString();

/**
 * Mark a constructor and its prototype methods/accessors plus own statics native,
 * matching how browsers render the whole interface. Accessor display names use
 * the `get x` / `set x` form V8 emits.
 */
export function markClassNative(Ctor) {
  markNative(Ctor, Ctor.name);
  const visit = (obj, isProto) => {
    for (const key of Object.getOwnPropertyNames(obj)) {
      if (isProto && key === 'constructor') continue;
      const d = Object.getOwnPropertyDescriptor(obj, key);
      if (!d) continue;
      if (typeof d.value === 'function') markNative(d.value, key);
      if (typeof d.get === 'function') markNative(d.get, 'get ' + key);
      if (typeof d.set === 'function') markNative(d.set, 'set ' + key);
    }
  };
  visit(Ctor.prototype, true);
  visit(Ctor, false);
}

/*
 * Core node/element prototype chain.
 *
 * Direct `new` of a DOM interface throws `TypeError: Illegal constructor` in
 * browsers; only `EventTarget`, `Event` and a few others are constructable.
 * Real elements are minted with `Object.create(Ctor.prototype)` (bypassing the
 * throwing constructors) and field-initialized, so `instanceof` is exact while
 * `new HTMLDivElement()` still throws, matching the platform.
 */

function illegal() { throw new TypeError('Illegal constructor'); }

class EventTarget {
  addEventListener(type, cb) {
    if (!this._listeners) this._listeners = Object.create(null);
    (this._listeners[type] || (this._listeners[type] = [])).push(cb);
  }
  removeEventListener(type, cb) {
    const l = this._listeners && this._listeners[type];
    if (l) { const i = l.indexOf(cb); if (i >= 0) l.splice(i, 1); }
  }
  dispatchEvent(_evt) { return true; }
}

class Node extends EventTarget {
  constructor() { super(); illegal(); }
  get nodeType() { return this._nodeType == null ? 1 : this._nodeType; }
  get nodeName() { return this._nodeName || this._tagName || '#node'; }
  get nodeValue() { return this._nodeValue == null ? null : this._nodeValue; }
  set nodeValue(v) { this._nodeValue = v; }
  get ownerDocument() { return this._ownerDocument || null; }
  get parentNode() { return this._parentNode || null; }
  get parentElement() { return this._parentNode || null; }
  get childNodes() { return this._childNodes || (this._childNodes = []); }
  get firstChild() { return this.childNodes[0] || null; }
  get lastChild() { return this.childNodes[this.childNodes.length - 1] || null; }
  get nextSibling() { return null; }
  get previousSibling() { return null; }
  get textContent() { return this._textContent || ''; }
  set textContent(v) { this._textContent = String(v); }
  get isConnected() { return true; }
  hasChildNodes() { return this.childNodes.length > 0; }
  appendChild(child) { this.childNodes.push(child); if (child) child._parentNode = this; return child; }
  removeChild(child) { const i = this.childNodes.indexOf(child); if (i >= 0) this.childNodes.splice(i, 1); return child; }
  insertBefore(child, _ref) { this.childNodes.push(child); if (child) child._parentNode = this; return child; }
  replaceChild(n, _o) { return n; }
  cloneNode(_deep) { const c = Object.create(Object.getPrototypeOf(this)); initElement(c, this._localName || 'div', this._namespaceURI); return c; }
  contains(_n) { return false; }
  getRootNode() { return this._ownerDocument || this; }
  compareDocumentPosition() { return 0; }
}
Node.ELEMENT_NODE = 1; Node.TEXT_NODE = 3; Node.DOCUMENT_NODE = 9;
Node.prototype.ELEMENT_NODE = 1; Node.prototype.TEXT_NODE = 3; Node.prototype.DOCUMENT_NODE = 9;

class Element extends Node {
  constructor() { super(); }
  get tagName() { return this._tagName || ''; }
  get localName() { return this._localName || ''; }
  get namespaceURI() { return this._namespaceURI || 'http://www.w3.org/1999/xhtml'; }
  get id() { return this._attributes.id || ''; }
  set id(v) { this._attributes.id = String(v); }
  get className() { return this._attributes.class || ''; }
  set className(v) { this._attributes.class = String(v); }
  get classList() { return this._classList || (this._classList = makeClassList(this)); }
  get attributes() { return this._attrNodes || (this._attrNodes = makeAttrList(this)); }
  get children() { return this.childNodes.filter((n) => n.nodeType === 1); }
  get childElementCount() { return this.children.length; }
  get innerHTML() { return this._innerHTML || ''; }
  set innerHTML(v) { this._innerHTML = String(v); }
  get outerHTML() { return '<' + (this._localName || '') + '></' + (this._localName || '') + '>'; }
  getAttribute(name) { const v = this._attributes[String(name)]; return v == null ? null : String(v); }
  getAttributeNS(_ns, name) { return this.getAttribute(name); }
  setAttribute(name, value) { this._attributes[String(name)] = String(value); }
  setAttributeNS(_ns, name, value) { this.setAttribute(name, value); }
  removeAttribute(name) { delete this._attributes[String(name)]; }
  hasAttribute(name) { return Object.prototype.hasOwnProperty.call(this._attributes, String(name)); }
  hasAttributes() { return Object.keys(this._attributes).length > 0; }
  getAttributeNames() { return Object.keys(this._attributes); }
  matches(_sel) { return false; }
  closest(_sel) { return null; }
  querySelector(_sel) { return null; }
  querySelectorAll(_sel) { return makeNodeList([]); }
  getElementsByTagName(_t) { return makeNodeList([]); }
  getElementsByClassName(_c) { return makeNodeList([]); }
  getBoundingClientRect() { return new DOMRect(0, 0, this._offsetWidth || 0, this._offsetHeight || 0); }
  getClientRects() { return [this.getBoundingClientRect()]; }
  append() {} prepend() {} after() {} before() {} remove() {}
  insertAdjacentElement(_p, el) { return el; }
  insertAdjacentHTML() {}
  scrollIntoView() {}
  attachShadow() { return null; }
}

class HTMLElement extends Element {
  constructor() { super(); }
  get style() { return this._style || (this._style = makeStyle()); }
  get dataset() { return this._dataset || (this._dataset = {}); }
  get hidden() { return !!this._hidden; }
  set hidden(v) { this._hidden = !!v; }
  get tabIndex() { return this._tabIndex == null ? -1 : this._tabIndex; }
  set tabIndex(v) { this._tabIndex = v | 0; }
  get title() { return this._attributes.title || ''; }
  set title(v) { this._attributes.title = String(v); }
  get lang() { return this._attributes.lang || ''; }
  get dir() { return this._attributes.dir || ''; }
  get innerText() { return this._textContent || ''; }
  set innerText(v) { this._textContent = String(v); }
  get contentEditable() { return 'inherit'; }
  get isContentEditable() { return false; }
  get offsetParent() { return this._ownerDocument ? this._ownerDocument.body : null; }
  get offsetWidth() { return this._offsetWidth || 0; }
  get offsetHeight() { return this._offsetHeight || 0; }
  get offsetTop() { return 0; }
  get offsetLeft() { return 0; }
  get clientWidth() { return this._offsetWidth || 0; }
  get clientHeight() { return this._offsetHeight || 0; }
  get clientTop() { return 0; }
  get clientLeft() { return 0; }
  get scrollWidth() { return this._offsetWidth || 0; }
  get scrollHeight() { return this._offsetHeight || 0; }
  click() {} focus() {} blur() {}
}

/* HTML element interface battery: correct chains first. Each is
 * a real subclass, so `createElement(tag) instanceof HTMLXElement` holds and the
 * prototype walk HTMLXElement -> HTMLElement -> Element -> Node -> EventTarget is
 * exact. */
class HTMLUnknownElement extends HTMLElement {}
class HTMLHtmlElement extends HTMLElement {}
class HTMLHeadElement extends HTMLElement {}
class HTMLBodyElement extends HTMLElement {}
class HTMLDivElement extends HTMLElement {}
class HTMLSpanElement extends HTMLElement {}
class HTMLParagraphElement extends HTMLElement {}
class HTMLAnchorElement extends HTMLElement {
  get href() { return this._attributes.href || ''; }
  set href(v) { this._attributes.href = String(v); }
  get protocol() { return 'https:'; }
  get host() { return 'www.youtube.com'; }
  get hostname() { return 'www.youtube.com'; }
  get pathname() { return '/'; }
  get search() { return ''; }
  get hash() { return ''; }
  toString() { return this.href; }
}
class HTMLImageElement extends HTMLElement {
  get width() { return this._width || 0; } set width(v) { this._width = v | 0; }
  get height() { return this._height || 0; } set height(v) { this._height = v | 0; }
  get naturalWidth() { return this._width || 0; }
  get naturalHeight() { return this._height || 0; }
  get complete() { return true; }
  get src() { return this._attributes.src || ''; } set src(v) { this._attributes.src = String(v); }
}
class HTMLScriptElement extends HTMLElement {
  get src() { return this._attributes.src || ''; } set src(v) { this._attributes.src = String(v); }
  get type() { return this._attributes.type || ''; }
  get async() { return !!this._async; } set async(v) { this._async = !!v; }
}
class HTMLLinkElement extends HTMLElement {}
class HTMLStyleElement extends HTMLElement {}
class HTMLMetaElement extends HTMLElement {}
class HTMLIFrameElement extends HTMLElement {
  get contentWindow() { return G.window || G; }
  get contentDocument() { return G.document || null; }
}
class HTMLInputElement extends HTMLElement {
  get value() { return this._value || ''; } set value(v) { this._value = String(v); }
  get type() { return this._attributes.type || 'text'; }
  get checked() { return !!this._checked; } set checked(v) { this._checked = !!v; }
}
class HTMLButtonElement extends HTMLElement {}
class HTMLFormElement extends HTMLElement {}
class HTMLSelectElement extends HTMLElement {}
class HTMLOptionElement extends HTMLElement {}
class HTMLTextAreaElement extends HTMLElement {}
class HTMLTableElement extends HTMLElement {}
class HTMLUListElement extends HTMLElement {}
class HTMLLIElement extends HTMLElement {}
class HTMLLabelElement extends HTMLElement {}
class HTMLPictureElement extends HTMLElement {}
class HTMLSourceElement extends HTMLElement {}
class HTMLTemplateElement extends HTMLElement {
  get content() { return this._content || (this._content = createDocumentFragment()); }
}
/* The rest of the standard HTML element interface set. BotGuard walks the full
 * battery (for example, it has probed `window.HTMLMeterElement`), and a
 * missing interface breaks `createElement(tag) instanceof HTMLXElement`. These
 * are real subclasses: correct chain, native toString, createElement-mapped. */
class HTMLBRElement extends HTMLElement {}
class HTMLHRElement extends HTMLElement {}
class HTMLPreElement extends HTMLElement {}
class HTMLQuoteElement extends HTMLElement {}
class HTMLDListElement extends HTMLElement {}
class HTMLOListElement extends HTMLElement {}
class HTMLFieldSetElement extends HTMLElement {}
class HTMLLegendElement extends HTMLElement {}
class HTMLDataElement extends HTMLElement {}
class HTMLDataListElement extends HTMLElement {}
class HTMLDetailsElement extends HTMLElement {}
class HTMLDialogElement extends HTMLElement {}
class HTMLEmbedElement extends HTMLElement {}
class HTMLObjectElement extends HTMLElement {}
class HTMLMapElement extends HTMLElement {}
class HTMLAreaElement extends HTMLElement {}
class HTMLBaseElement extends HTMLElement {}
class HTMLTitleElement extends HTMLElement {}
class HTMLMenuElement extends HTMLElement {}
class HTMLMeterElement extends HTMLElement { get value() { return this._value || 0; } get max() { return this._max == null ? 1 : this._max; } }
class HTMLProgressElement extends HTMLElement { get value() { return this._value || 0; } get max() { return this._max == null ? 1 : this._max; } get position() { return -1; } }
class HTMLOutputElement extends HTMLElement {}
class HTMLModElement extends HTMLElement {}
class HTMLHeadingElement extends HTMLElement {}
class HTMLOptGroupElement extends HTMLElement {}
class HTMLSlotElement extends HTMLElement { assignedNodes() { return []; } assignedElements() { return []; } }
class HTMLTimeElement extends HTMLElement {}
class HTMLTrackElement extends HTMLElement {}
class HTMLTableCaptionElement extends HTMLElement {}
class HTMLTableCellElement extends HTMLElement {}
class HTMLTableColElement extends HTMLElement {}
class HTMLTableRowElement extends HTMLElement {}
class HTMLTableSectionElement extends HTMLElement {}
class HTMLMarqueeElement extends HTMLElement {}
class HTMLFontElement extends HTMLElement {}
class HTMLParamElement extends HTMLElement {}
class HTMLFrameElement extends HTMLElement {}
class HTMLFrameSetElement extends HTMLElement {}
class HTMLDirectoryElement extends HTMLElement {}

/* Canvas and rendering contexts. */
class HTMLCanvasElement extends HTMLElement {
  get width() { return this._width == null ? 300 : this._width; }
  set width(v) { this._width = v | 0; }
  get height() { return this._height == null ? 150 : this._height; }
  set height(v) { this._height = v | 0; }
  getContext(type, attrs) { return getCanvasContext(this, String(type), attrs); }
  toDataURL(type) { return canvasToDataURL(this, type); }
  toBlob(cb) { if (typeof cb === 'function') cb(new Blob([], { type: 'image/png' })); }
  captureStream() { return new MediaStream(); }
}

class CanvasRenderingContext2D {
  constructor() { illegal(); }
  getContextAttributes() { return { alpha: true, desynchronized: false, colorSpace: 'srgb', willReadFrequently: false }; }
  save() {} restore() {} scale() {} rotate() {} translate() {} transform() {} setTransform() {} resetTransform() {}
  beginPath() {} closePath() {} moveTo() {} lineTo() {} bezierCurveTo() {} quadraticCurveTo() {} arc() {} arcTo() {} ellipse() {} rect() {} roundRect() {}
  fill() {} stroke() {} clip() {} isPointInPath() { return false; } isPointInStroke() { return false; }
  fillRect() {} strokeRect() {} clearRect() {}
  fillText() {} strokeText() {}
  measureText(text) { return new TextMetrics(String(text == null ? '' : text)); }
  createLinearGradient() { return makeGradient(); }
  createRadialGradient() { return makeGradient(); }
  createConicGradient() { return makeGradient(); }
  createPattern() { return null; }
  drawImage() {} putImageData() {}
  getImageData(_x, _y, w, h) { return new ImageData(w | 0 || 1, h | 0 || 1); }
  createImageData(w, h) { return new ImageData((w | 0) || 1, (h | 0) || 1); }
  setLineDash() {} getLineDash() { return []; }
  getTransform() { return new DOMMatrix(); }
}

class WebGLRenderingContext {
  constructor() { illegal(); }
  getParameter(p) { return WEBGL_PARAMS.hasOwnProperty(p) ? WEBGL_PARAMS[p] : null; }
  getExtension(name) {
    if (name === 'WEBGL_debug_renderer_info') return { UNMASKED_VENDOR_WEBGL: 0x9245, UNMASKED_RENDERER_WEBGL: 0x9246 };
    if (WEBGL_EXTENSIONS.indexOf(name) >= 0) return {};
    return null;
  }
  getSupportedExtensions() { return WEBGL_EXTENSIONS.slice(); }
  getContextAttributes() { return { alpha: true, antialias: true, depth: true, desynchronized: false, failIfMajorPerformanceCaveat: false, powerPreference: 'default', premultipliedAlpha: true, preserveDrawingBuffer: false, stencil: false, xrCompatible: false }; }
  getShaderPrecisionFormat() { return { rangeMin: 127, rangeMax: 127, precision: 23 }; }
  createBuffer() { return {}; } createProgram() { return {}; } createShader() { return {}; }
  createTexture() { return {}; } createFramebuffer() { return {}; } createRenderbuffer() { return {}; }
  bindBuffer() {} bufferData() {} bindTexture() {} texParameteri() {} texImage2D() {}
  shaderSource() {} compileShader() {} attachShader() {} linkProgram() {} useProgram() {}
  getAttribLocation() { return 0; } getUniformLocation() { return {}; }
  enableVertexAttribArray() {} vertexAttribPointer() {} drawArrays() {} drawElements() {}
  viewport() {} clearColor() {} clear() {} enable() {} disable() {}
  getError() { return 0; } // NO_ERROR
  readPixels() {}
}
class WebGL2RenderingContext extends WebGLRenderingContext {}

/* Media. */
class HTMLMediaElement extends HTMLElement {
  get currentTime() { return 0; } set currentTime(_v) {}
  get duration() { return NaN; }
  get paused() { return true; }
  get muted() { return false; } set muted(_v) {}
  get volume() { return 1; } set volume(_v) {}
  get readyState() { return 0; }
  get networkState() { return 0; }
  get videoTracks() { return this._videoTracks || (this._videoTracks = new VideoTrackList()); }
  get audioTracks() { return this._audioTracks || (this._audioTracks = new AudioTrackList()); }
  get textTracks() { return this._textTracks || (this._textTracks = new TextTrackList()); }
  canPlayType(type) {
    type = String(type || '');
    if (/mp4|h264|avc1|mpeg|aac/i.test(type)) return 'probably';
    if (/webm|ogg|vorbis|opus|vp9|vp8/i.test(type)) return 'maybe';
    return '';
  }
  load() {} play() { return Promise.resolve(); } pause() {} addTextTrack() { return new TextTrack(); }
}
class HTMLVideoElement extends HTMLMediaElement {
  get videoWidth() { return this._videoWidth || 0; }
  get videoHeight() { return this._videoHeight || 0; }
  getVideoPlaybackQuality() { return { creationTime: 0, droppedVideoFrames: 0, totalVideoFrames: 0 }; }
}
class HTMLAudioElement extends HTMLMediaElement {}

/* SVG chain plus media-track interfaces. */
class SVGElement extends Element {
  constructor() { super(); }
  get style() { return this._style || (this._style = makeStyle()); }
  get ownerSVGElement() { return null; }
}
class SVGGraphicsElement extends SVGElement {}
class SVGSVGElement extends SVGGraphicsElement {
  createSVGRect() { return new DOMRect(0, 0, 0, 0); }
  createSVGPoint() { return { x: 0, y: 0, matrixTransform() { return this; } }; }
  createSVGMatrix() { return new DOMMatrix(); }
  getCurrentTime() { return 0; }
}
class SVGGElement extends SVGGraphicsElement {}
class SVGPathElement extends SVGGraphicsElement {}
class SVGRectElement extends SVGGraphicsElement {}
class SVGTextElement extends SVGGraphicsElement {}
class SVGImageElement extends SVGGraphicsElement {}
class SVGUseElement extends SVGGraphicsElement {}
class SVGViewElement extends SVGElement {}
/* The broader SVG element set; live probes have included window.SVGSetElement.
 * Real chains plus createElementNS mappings keep presence and instanceof
 * coherent for element interfaces. SVG value types such as SVGAngle,
 * SVGLength, and SVGMatrix are not elements, so they go in the presence battery
 * instead. */
class SVGGeometryElement extends SVGGraphicsElement {}
class SVGCircleElement extends SVGGeometryElement {}
class SVGEllipseElement extends SVGGeometryElement {}
class SVGLineElement extends SVGGeometryElement {}
class SVGPolygonElement extends SVGGeometryElement {}
class SVGPolylineElement extends SVGGeometryElement {}
class SVGDefsElement extends SVGGraphicsElement {}
class SVGGradientElement extends SVGElement {}
class SVGLinearGradientElement extends SVGGradientElement {}
class SVGRadialGradientElement extends SVGGradientElement {}
class SVGStopElement extends SVGElement {}
class SVGSymbolElement extends SVGGraphicsElement {}
class SVGMarkerElement extends SVGElement {}
class SVGPatternElement extends SVGElement {}
class SVGMaskElement extends SVGElement {}
class SVGFilterElement extends SVGElement {}
class SVGClipPathElement extends SVGElement {}
class SVGTextContentElement extends SVGGraphicsElement {}
class SVGTextPositioningElement extends SVGTextContentElement {}
class SVGTSpanElement extends SVGTextPositioningElement {}
class SVGTextPathElement extends SVGTextContentElement {}
class SVGAElement extends SVGGraphicsElement {}
class SVGStyleElement extends SVGElement {}
class SVGTitleElement extends SVGElement {}
class SVGDescElement extends SVGElement {}
class SVGMetadataElement extends SVGElement {}
class SVGSwitchElement extends SVGGraphicsElement {}
class SVGForeignObjectElement extends SVGGraphicsElement {}
class SVGScriptElement extends SVGElement {}
class SVGAnimationElement extends SVGElement {}
class SVGSetElement extends SVGAnimationElement {}
class SVGAnimateElement extends SVGAnimationElement {}
class SVGAnimateMotionElement extends SVGAnimationElement {}
class SVGAnimateTransformElement extends SVGAnimationElement {}

const SVG_INTERFACES = {
  'svg': SVGSVGElement, 'g': SVGGElement, 'path': SVGPathElement,
  'rect': SVGRectElement, 'text': SVGTextElement, 'image': SVGImageElement,
  'use': SVGUseElement, 'view': SVGViewElement,
  'circle': SVGCircleElement, 'ellipse': SVGEllipseElement, 'line': SVGLineElement,
  'polygon': SVGPolygonElement, 'polyline': SVGPolylineElement, 'defs': SVGDefsElement,
  'lineargradient': SVGLinearGradientElement, 'radialgradient': SVGRadialGradientElement,
  'stop': SVGStopElement, 'symbol': SVGSymbolElement, 'marker': SVGMarkerElement,
  'pattern': SVGPatternElement, 'mask': SVGMaskElement, 'filter': SVGFilterElement,
  'clippath': SVGClipPathElement, 'tspan': SVGTSpanElement, 'textpath': SVGTextPathElement,
  'a': SVGAElement, 'style': SVGStyleElement, 'title': SVGTitleElement, 'desc': SVGDescElement,
  'metadata': SVGMetadataElement, 'switch': SVGSwitchElement, 'foreignobject': SVGForeignObjectElement,
  'script': SVGScriptElement, 'set': SVGSetElement, 'animate': SVGAnimateElement,
  'animatemotion': SVGAnimateMotionElement, 'animatetransform': SVGAnimateTransformElement
};

class VideoTrack { constructor() { illegal(); } get id() { return ''; } get kind() { return ''; } get label() { return ''; } get language() { return ''; } get selected() { return false; } }
class AudioTrack { constructor() { illegal(); } get id() { return ''; } get kind() { return ''; } get label() { return ''; } get language() { return ''; } get enabled() { return true; } }
class TextTrack extends EventTarget { get kind() { return 'subtitles'; } get mode() { return 'disabled'; } addCue() {} removeCue() {} }
class VideoTrackList extends EventTarget { get length() { return 0; } getTrackById() { return null; } }
class AudioTrackList extends EventTarget { get length() { return 0; } getTrackById() { return null; } }
class TextTrackList extends EventTarget { get length() { return 0; } getTrackById() { return null; } }
class MediaError { get code() { return 0; } get message() { return ''; } }
class TimeRanges { get length() { return 0; } start() { return 0; } end() { return 0; } }
// Chrome exposes FontFaceSet through document.fonts but not as a window global.
class FontFaceSet extends EventTarget {
  get size() { return 0; }
  get status() { return 'loaded'; }
  get ready() { return Promise.resolve(this); }
  check() { return true; }
  load() { return Promise.resolve([]); }
  add() { return this; }
  delete() { return false; }
  has() { return false; }
  clear() {}
  forEach() {}
}
class MediaStream extends EventTarget { get active() { return false; } get id() { return ''; } getTracks() { return []; } getVideoTracks() { return []; } getAudioTracks() { return []; } }

/* Events. */
class Event {
  constructor(type, init) { init = init || {}; this.type = String(type == null ? '' : type); this.bubbles = !!init.bubbles; this.cancelable = !!init.cancelable; this.composed = !!init.composed; this.defaultPrevented = false; this.timeStamp = (G.performance && G.performance.now) ? G.performance.now() : 0; this.target = null; this.currentTarget = null; }
  preventDefault() { this.defaultPrevented = true; }
  stopPropagation() {} stopImmediatePropagation() {}
  composedPath() { return []; }
}
Event.NONE = 0; Event.CAPTURING_PHASE = 1; Event.AT_TARGET = 2; Event.BUBBLING_PHASE = 3;
class CustomEvent extends Event { constructor(type, init) { super(type, init); this.detail = (init && init.detail) != null ? init.detail : null; } }
class UIEvent extends Event { constructor(type, init) { super(type, init); this.detail = (init && init.detail) || 0; this.view = (init && init.view) || null; } }
class MouseEvent extends UIEvent {}
class KeyboardEvent extends UIEvent {}
class FocusEvent extends UIEvent {}
class PointerEvent extends MouseEvent {}
class WheelEvent extends MouseEvent {}
class ErrorEvent extends Event {}
class MessageEvent extends Event { constructor(type, init) { super(type, init); this.data = (init && init.data) != null ? init.data : null; } }

/* Geometry and other value types. */
class DOMRect {
  constructor(x, y, w, h) { this.x = +x || 0; this.y = +y || 0; this.width = +w || 0; this.height = +h || 0; }
  get top() { return this.y; } get left() { return this.x; }
  get right() { return this.x + this.width; } get bottom() { return this.y + this.height; }
  toJSON() { return { x: this.x, y: this.y, width: this.width, height: this.height, top: this.top, right: this.right, bottom: this.bottom, left: this.left }; }
}
class DOMRectReadOnly extends DOMRect {}
class DOMMatrix {
  constructor() { this.a = 1; this.b = 0; this.c = 0; this.d = 1; this.e = 0; this.f = 0; this.is2D = true; this.isIdentity = true; }
  multiply() { return new DOMMatrix(); } translate() { return new DOMMatrix(); } scale() { return new DOMMatrix(); } inverse() { return new DOMMatrix(); }
}
class DOMPoint { constructor(x, y, z, w) { this.x = +x || 0; this.y = +y || 0; this.z = +z || 0; this.w = w == null ? 1 : +w; } }
class TextMetrics {
  constructor(text) {
    // A coherent, deterministic width estimate (no rasterizer): ~7px/char, with
    // the ascent/descent boxes browsers expose. Stable across runs by design.
    const w = text.length * 7;
    this.width = w;
    this.actualBoundingBoxLeft = 0; this.actualBoundingBoxRight = w;
    this.actualBoundingBoxAscent = 8; this.actualBoundingBoxDescent = 2;
    this.fontBoundingBoxAscent = 9; this.fontBoundingBoxDescent = 2;
    this.emHeightAscent = 9; this.emHeightDescent = 2;
    this.hangingBaseline = 7; this.alphabeticBaseline = 0; this.ideographicBaseline = -2;
  }
}
class ImageData {
  constructor(w, h) { this.width = w | 0; this.height = h | 0; this.data = new Uint8ClampedArray(this.width * this.height * 4); this.colorSpace = 'srgb'; }
}
class Blob {
  constructor(parts, opts) { this._parts = parts || []; this.type = (opts && opts.type) || ''; this.size = 0; }
  slice() { return new Blob([], { type: this.type }); }
  text() { return Promise.resolve(''); }
  arrayBuffer() { return Promise.resolve(new ArrayBuffer(0)); }
}
class File extends Blob { constructor(parts, name, opts) { super(parts, opts); this.name = String(name); this.lastModified = 0; } }

/* Fetch interfaces, feature-detected by some probes. */
class Headers {
  constructor(init) { this._m = Object.create(null); if (init) { if (typeof init.forEach === 'function' && !Array.isArray(init)) init.forEach((v, k) => this.append(k, v)); else if (Array.isArray(init)) init.forEach((p) => this.append(p[0], p[1])); else for (const k in init) this.append(k, init[k]); } }
  append(k, v) { k = String(k).toLowerCase(); this._m[k] = this._m[k] ? this._m[k] + ', ' + v : String(v); }
  set(k, v) { this._m[String(k).toLowerCase()] = String(v); }
  get(k) { const v = this._m[String(k).toLowerCase()]; return v == null ? null : v; }
  has(k) { return String(k).toLowerCase() in this._m; }
  delete(k) { delete this._m[String(k).toLowerCase()]; }
  forEach(cb, thisArg) { for (const k in this._m) cb.call(thisArg, this._m[k], k, this); }
  keys() { return Object.keys(this._m)[Symbol.iterator](); }
}
class Request {
  constructor(input, init) { init = init || {}; this.url = typeof input === 'object' && input ? input.url : String(input); this.method = (init.method || 'GET').toUpperCase(); this.headers = new Headers(init.headers); this.credentials = init.credentials || 'same-origin'; this.mode = init.mode || 'cors'; this.cache = init.cache || 'default'; this.redirect = init.redirect || 'follow'; this.referrer = init.referrer || 'about:client'; }
  clone() { return new Request(this.url, this); }
}
class Response {
  constructor(body, init) { init = init || {}; this._body = body; this.status = init.status == null ? 200 : init.status; this.ok = this.status >= 200 && this.status < 300; this.statusText = init.statusText || ''; this.headers = new Headers(init.headers); this.type = 'basic'; this.url = ''; this.redirected = false; this.bodyUsed = false; }
  clone() { return new Response(this._body, this); }
  text() { return Promise.resolve(String(this._body == null ? '' : this._body)); }
  json() { return Promise.resolve(JSON.parse(this._body)); }
  arrayBuffer() { return Promise.resolve(new ArrayBuffer(0)); }
  blob() { return Promise.resolve(new Blob([this._body])); }
}
class AbortController {
  constructor() { this.signal = new AbortSignal(); }
  abort(reason) { this.signal._aborted = true; this.signal.reason = reason; this.signal.dispatchEvent(new Event('abort')); }
}
class AbortSignal extends EventTarget { get aborted() { return !!this._aborted; } throwIfAborted() { if (this._aborted) throw new Event('AbortError'); } }
class URLSearchParams {
  constructor(init) { this._p = []; if (typeof init === 'string') { init.replace(/^\?/, '').split('&').forEach((kv) => { if (!kv) return; const i = kv.indexOf('='); this._p.push(i < 0 ? [kv, ''] : [decodeURIComponent(kv.slice(0, i)), decodeURIComponent(kv.slice(i + 1))]); }); } else if (init) for (const k in init) this._p.push([k, String(init[k])]); }
  get(k) { const e = this._p.find((p) => p[0] === k); return e ? e[1] : null; }
  getAll(k) { return this._p.filter((p) => p[0] === k).map((p) => p[1]); }
  has(k) { return this._p.some((p) => p[0] === k); }
  set(k, v) { this.delete(k); this._p.push([k, String(v)]); }
  append(k, v) { this._p.push([k, String(v)]); }
  delete(k) { this._p = this._p.filter((p) => p[0] !== k); }
  toString() { return this._p.map((p) => encodeURIComponent(p[0]) + '=' + encodeURIComponent(p[1])).join('&'); }
  forEach(cb) { this._p.forEach((p) => cb(p[1], p[0], this)); }
}

/* Document and DocumentFragment. */
class DocumentFragment extends Node { constructor() { super(); } }
class CharacterData extends Node {}
class Text extends CharacterData { get nodeType() { return 3; } get nodeName() { return '#text'; } }
class Comment extends CharacterData { get nodeType() { return 8; } get nodeName() { return '#comment'; } }
class DOMTokenList {}
class NamedNodeMap {}
class NodeList {}
class HTMLCollection {}

class Document extends Node {
  constructor() { super(); }
  get nodeType() { return 9; }
  get nodeName() { return '#document'; }
  createElement(tag) { return createElement(tag); }
  createElementNS(ns, tag) { return createElement(tag, ns); }
  createDocumentFragment() { return createDocumentFragment(); }
  createTextNode(data) { const t = Object.create(Text.prototype); t._nodeValue = String(data); t._childNodes = []; t._ownerDocument = this; return t; }
  createComment(data) { const c = Object.create(Comment.prototype); c._nodeValue = String(data); c._childNodes = []; c._ownerDocument = this; return c; }
  createEvent(_t) { const e = Object.create(Event.prototype); e.type = ''; e.initEvent = function (type) { e.type = String(type); }; return e; }
  getElementById(_id) { return null; }
  getElementsByTagName(_t) { return makeNodeList([]); }
  getElementsByClassName(_c) { return makeNodeList([]); }
  getElementsByName(_n) { return makeNodeList([]); }
  querySelector(_s) { return null; }
  querySelectorAll(_s) { return makeNodeList([]); }
  addEventListener() {} removeEventListener() {} dispatchEvent() { return true; }
  get documentElement() { return this._documentElement; }
  get head() { return this._head; }
  get body() { return this._body; }
  // document.fonts is a lazily created [SameObject] value.
  get fonts() { return this._fonts || (this._fonts = new FontFaceSet()); }
  get defaultView() { return G.window || G; }
  get location() { return G.location; }
  get cookie() { return this._cookie || ''; }
  set cookie(v) { this._cookie = String(v); }
  get readyState() { return 'complete'; }
  get visibilityState() { return 'visible'; }
  get hidden() { return false; }
  get title() { return this._title || ''; }
  set title(v) { this._title = String(v); }
  get referrer() { return ''; }
  get URL() { return G.location ? G.location.href : 'https://www.youtube.com/'; }
  get documentURI() { return this.URL; }
  get characterSet() { return 'UTF-8'; }
  get charset() { return 'UTF-8'; }
  get compatMode() { return 'CSS1Compat'; }
  get contentType() { return 'text/html'; }
  get currentScript() { return null; }
  get activeElement() { return this._body; }
  hasFocus() { return true; }
  elementFromPoint() { return null; }
}
class HTMLDocument extends Document {}

/*
 * Window + the browser-object interfaces. The data (userAgent, screen size,
 * timezone, ...) is profile-derived and assigned by shim.js onto instances of
 * these classes, so `navigator instanceof Navigator`, `screen instanceof Screen`
 * etc. all hold.
 */
class Window extends EventTarget {}
class Navigator {}
class NavigatorUAData {}
class Screen {}
class Location {}
class History { get length() { return 1; } get state() { return null; } back() {} forward() {} go() {} pushState() {} replaceState() {} }
class Performance extends EventTarget {}
// Chrome exposes PictureInPictureWindow as a global EventTarget constructor.
class PictureInPictureWindow extends EventTarget {
  get width() { return 0; } get height() { return 0; }
  get onresize() { return null; } set onresize(_v) {}
}
class Storage { get length() { return 0; } getItem() { return null; } setItem() {} removeItem() {} clear() {} key() { return null; } }
// Crypto is a real class so the `crypto` singleton can be `instanceof Crypto`
// (shim.js builds it via Object.create(Crypto.prototype)), not a bare presence
// stub that would make `crypto instanceof Crypto` false.
class Crypto {}
class SubtleCrypto {}
class CryptoKey {}
class Plugin {}
class PluginArray { get length() { return 0; } item() { return null; } namedItem() { return null; } }
class MimeType {}
class MimeTypeArray { get length() { return 0; } item() { return null; } namedItem() { return null; } }
class CSSStyleDeclaration {}

/* Element-creation registry and helpers. */
const ELEMENT_INTERFACES = {
  'html': HTMLHtmlElement, 'head': HTMLHeadElement, 'body': HTMLBodyElement,
  'div': HTMLDivElement, 'span': HTMLSpanElement, 'p': HTMLParagraphElement,
  'a': HTMLAnchorElement, 'img': HTMLImageElement, 'script': HTMLScriptElement,
  'link': HTMLLinkElement, 'style': HTMLStyleElement, 'meta': HTMLMetaElement,
  'iframe': HTMLIFrameElement, 'input': HTMLInputElement, 'button': HTMLButtonElement,
  'form': HTMLFormElement, 'select': HTMLSelectElement, 'option': HTMLOptionElement,
  'textarea': HTMLTextAreaElement, 'table': HTMLTableElement, 'ul': HTMLUListElement,
  'li': HTMLLIElement, 'label': HTMLLabelElement, 'picture': HTMLPictureElement,
  'source': HTMLSourceElement, 'template': HTMLTemplateElement,
  'canvas': HTMLCanvasElement, 'video': HTMLVideoElement, 'audio': HTMLAudioElement,
  // The rest of the standard battery, all real subclasses.
  'br': HTMLBRElement, 'hr': HTMLHRElement, 'pre': HTMLPreElement,
  'q': HTMLQuoteElement, 'blockquote': HTMLQuoteElement, 'dl': HTMLDListElement,
  'ol': HTMLOListElement, 'fieldset': HTMLFieldSetElement, 'legend': HTMLLegendElement,
  'data': HTMLDataElement, 'datalist': HTMLDataListElement, 'details': HTMLDetailsElement,
  'dialog': HTMLDialogElement, 'embed': HTMLEmbedElement, 'object': HTMLObjectElement,
  'map': HTMLMapElement, 'area': HTMLAreaElement, 'base': HTMLBaseElement,
  'title': HTMLTitleElement, 'menu': HTMLMenuElement, 'meter': HTMLMeterElement,
  'progress': HTMLProgressElement, 'output': HTMLOutputElement, 'ins': HTMLModElement,
  'del': HTMLModElement, 'optgroup': HTMLOptGroupElement, 'slot': HTMLSlotElement,
  'time': HTMLTimeElement, 'track': HTMLTrackElement, 'caption': HTMLTableCaptionElement,
  'td': HTMLTableCellElement, 'th': HTMLTableCellElement, 'col': HTMLTableColElement,
  'colgroup': HTMLTableColElement, 'tr': HTMLTableRowElement, 'thead': HTMLTableSectionElement,
  'tbody': HTMLTableSectionElement, 'tfoot': HTMLTableSectionElement, 'marquee': HTMLMarqueeElement,
  'font': HTMLFontElement, 'param': HTMLParamElement, 'frame': HTMLFrameElement,
  'frameset': HTMLFrameSetElement, 'dir': HTMLDirectoryElement,
  'h1': HTMLHeadingElement, 'h2': HTMLHeadingElement, 'h3': HTMLHeadingElement,
  'h4': HTMLHeadingElement, 'h5': HTMLHeadingElement, 'h6': HTMLHeadingElement
};

let currentDocument = null;

function initElement(el, tag, ns) {
  const local = String(tag == null ? '' : tag).toLowerCase();
  el._localName = local;
  el._tagName = ns && ns.indexOf('svg') >= 0 ? local : local.toUpperCase();
  el._namespaceURI = ns || 'http://www.w3.org/1999/xhtml';
  el._attributes = Object.create(null);
  el._childNodes = [];
  el._listeners = Object.create(null);
  el._ownerDocument = currentDocument;
  return el;
}

function createElement(tag, ns) {
  let Ctor, t = String(tag == null ? '' : tag).toLowerCase();
  if (ns && ns.indexOf('svg') >= 0) Ctor = SVG_INTERFACES[t] || SVGElement;
  else Ctor = ELEMENT_INTERFACES[t] || HTMLUnknownElement;
  const el = Object.create(Ctor.prototype);
  initElement(el, t, ns);
  if (Ctor === HTMLCanvasElement) { el._width = 300; el._height = 150; }
  return el;
}

function createDocumentFragment() { const f = Object.create(DocumentFragment.prototype); f._childNodes = []; f._ownerDocument = currentDocument; return f; }

function makeStyle() {
  // CSSStyleDeclaration-ish: stores set props, returns '' for unknown, supports
  // setProperty/getPropertyValue + cssText. Arbitrary camelCase props are stored.
  const store = Object.create(null);
  return {
    setProperty(k, v) { store[k] = String(v); },
    getPropertyValue(k) { return store[k] == null ? '' : store[k]; },
    removeProperty(k) { const v = store[k]; delete store[k]; return v == null ? '' : v; },
    get cssText() { return Object.keys(store).map((k) => k + ': ' + store[k]).join('; '); },
    set cssText(_v) {},
    item() { return ''; }, get length() { return Object.keys(store).length; }
  };
}

function makeClassList(el) {
  const set = () => new Set((el._attributes.class || '').split(/\s+/).filter(Boolean));
  return {
    add(...c) { const s = set(); c.forEach((x) => s.add(x)); el._attributes.class = [...s].join(' '); },
    remove(...c) { const s = set(); c.forEach((x) => s.delete(x)); el._attributes.class = [...s].join(' '); },
    toggle(c) { const s = set(); if (s.has(c)) { s.delete(c); } else { s.add(c); } el._attributes.class = [...s].join(' '); return s.has(c); },
    contains(c) { return set().has(c); },
    replace(a, b) { const s = set(); if (s.has(a)) { s.delete(a); s.add(b); el._attributes.class = [...s].join(' '); return true; } return false; },
    get length() { return set().size; },
    item(i) { return [...set()][i] || null; },
    toString() { return el._attributes.class || ''; }
  };
}

function makeAttrList(el) { return { get length() { return Object.keys(el._attributes).length; }, getNamedItem(n) { return el.hasAttribute(n) ? { name: n, value: el.getAttribute(n) } : null; }, item(i) { const k = Object.keys(el._attributes)[i]; return k == null ? null : { name: k, value: el._attributes[k] }; } }; }

function makeNodeList(arr) { const l = arr.slice(); l.item = (i) => l[i] || null; return l; }

function makeGradient() { return { addColorStop() {} }; }

function getCanvasContext(canvas, type, _attrs) {
  type = type.toLowerCase();
  if (type === '2d') { if (!canvas._ctx2d) { canvas._ctx2d = Object.create(CanvasRenderingContext2D.prototype); canvas._ctx2d.canvas = canvas; canvas._ctx2d.fillStyle = '#000000'; canvas._ctx2d.strokeStyle = '#000000'; canvas._ctx2d.font = '10px sans-serif'; canvas._ctx2d.globalAlpha = 1; canvas._ctx2d.lineWidth = 1; canvas._ctx2d.textBaseline = 'alphabetic'; canvas._ctx2d.textAlign = 'start'; } return canvas._ctx2d; }
  if (type === 'webgl' || type === 'experimental-webgl') { if (!canvas._ctxGL) { canvas._ctxGL = Object.create(WebGLRenderingContext.prototype); canvas._ctxGL.canvas = canvas; canvas._ctxGL.drawingBufferWidth = canvas.width; canvas._ctxGL.drawingBufferHeight = canvas.height; } return canvas._ctxGL; }
  if (type === 'webgl2') { if (!canvas._ctxGL2) { canvas._ctxGL2 = Object.create(WebGL2RenderingContext.prototype); canvas._ctxGL2.canvas = canvas; canvas._ctxGL2.drawingBufferWidth = canvas.width; canvas._ctxGL2.drawingBufferHeight = canvas.height; } return canvas._ctxGL2; }
  return null;
}

// A deterministic 1x1 transparent PNG data URL. Without a rasterizer the canvas
// fingerprint cannot be content-unique; a stable browser-shaped value is the
// contained choice. The discovery trap will flag it if BotGuard requires a
// content-derived hash.
const PNG_1x1 = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';
function canvasToDataURL(_canvas, type) { return type && String(type).indexOf('jpeg') >= 0 ? PNG_1x1.replace('image/png', 'image/jpeg') : PNG_1x1; }

// Chrome-on-Windows-ish WebGL parameters (NVIDIA/ANGLE). Keyed by GL enum.
const WEBGL_PARAMS = {
  0x1F00: 'WebKit',                                  // VENDOR
  0x1F01: 'WebKit WebGL',                            // RENDERER
  0x1F02: 'WebGL 1.0 (OpenGL ES 2.0 Chromium)',      // VERSION
  0x8B8C: 'WebGL GLSL ES 1.0 (OpenGL ES GLSL ES 1.0 Chromium)', // SHADING_LANGUAGE_VERSION
  0x9245: 'Google Inc. (NVIDIA)',                    // UNMASKED_VENDOR_WEBGL
  0x9246: 'ANGLE (NVIDIA, NVIDIA GeForce GTX 1060 Direct3D11 vs_5_0 ps_5_0, D3D11)', // UNMASKED_RENDERER_WEBGL
  0x0D33: 16384,  // MAX_TEXTURE_SIZE
  0x851C: 16384,  // MAX_CUBE_MAP_TEXTURE_SIZE
  0x84E8: 16384,  // MAX_RENDERBUFFER_SIZE
  0x8869: 16,     // MAX_VERTEX_ATTRIBS
  0x8DFB: 1024,   // MAX_FRAGMENT_UNIFORM_VECTORS
  0x8DFC: 30,     // MAX_VARYING_VECTORS
  0x8DFA: 4096,   // MAX_VERTEX_UNIFORM_VECTORS
  0x8872: 16,     // MAX_TEXTURE_IMAGE_UNITS
  0x8B4D: 32,     // MAX_COMBINED_TEXTURE_IMAGE_UNITS
  0x8B4C: 16,     // MAX_VERTEX_TEXTURE_IMAGE_UNITS
  0x0D3A: [32767, 32767], // MAX_VIEWPORT_DIMS
  0x846E: [1, 1] // ALIASED_POINT_SIZE_RANGE
};
const WEBGL_EXTENSIONS = [
  'ANGLE_instanced_arrays', 'EXT_blend_minmax', 'EXT_color_buffer_half_float',
  'EXT_float_blend', 'EXT_frag_depth', 'EXT_shader_texture_lod', 'EXT_texture_filter_anisotropic',
  'OES_element_index_uint', 'OES_standard_derivatives', 'OES_texture_float', 'OES_texture_float_linear',
  'OES_texture_half_float', 'OES_texture_half_float_linear', 'OES_vertex_array_object',
  'WEBGL_color_buffer_float', 'WEBGL_compressed_texture_s3tc', 'WEBGL_debug_renderer_info',
  'WEBGL_debug_shaders', 'WEBGL_depth_texture', 'WEBGL_lose_context', 'WEBGL_multi_draw'
];

/* All interface constructors to publish on globalThis and mark native. Order
 * does not affect chains, which are defined above. */
const INTERFACES = {
  EventTarget, Node, Element, HTMLElement, HTMLUnknownElement, HTMLHtmlElement,
  HTMLHeadElement, HTMLBodyElement, HTMLDivElement, HTMLSpanElement, HTMLParagraphElement,
  HTMLAnchorElement, HTMLImageElement, HTMLScriptElement, HTMLLinkElement, HTMLStyleElement,
  HTMLMetaElement, HTMLIFrameElement, HTMLInputElement, HTMLButtonElement, HTMLFormElement,
  HTMLSelectElement, HTMLOptionElement, HTMLTextAreaElement, HTMLTableElement, HTMLUListElement,
  HTMLLIElement, HTMLLabelElement, HTMLPictureElement, HTMLSourceElement, HTMLTemplateElement,
  HTMLBRElement, HTMLHRElement, HTMLPreElement, HTMLQuoteElement, HTMLDListElement, HTMLOListElement,
  HTMLFieldSetElement, HTMLLegendElement, HTMLDataElement, HTMLDataListElement, HTMLDetailsElement,
  HTMLDialogElement, HTMLEmbedElement, HTMLObjectElement, HTMLMapElement, HTMLAreaElement,
  HTMLBaseElement, HTMLTitleElement, HTMLMenuElement, HTMLMeterElement, HTMLProgressElement,
  HTMLOutputElement, HTMLModElement, HTMLHeadingElement, HTMLOptGroupElement, HTMLSlotElement,
  HTMLTimeElement, HTMLTrackElement, HTMLTableCaptionElement, HTMLTableCellElement, HTMLTableColElement,
  HTMLTableRowElement, HTMLTableSectionElement, HTMLMarqueeElement, HTMLFontElement, HTMLParamElement,
  HTMLFrameElement, HTMLFrameSetElement, HTMLDirectoryElement,
  HTMLCanvasElement, HTMLMediaElement, HTMLVideoElement, HTMLAudioElement,
  CanvasRenderingContext2D, WebGLRenderingContext, WebGL2RenderingContext,
  SVGElement, SVGGraphicsElement, SVGSVGElement, SVGGElement, SVGPathElement, SVGRectElement,
  SVGTextElement, SVGImageElement, SVGUseElement, SVGViewElement,
  SVGGeometryElement, SVGCircleElement, SVGEllipseElement, SVGLineElement, SVGPolygonElement,
  SVGPolylineElement, SVGDefsElement, SVGGradientElement, SVGLinearGradientElement,
  SVGRadialGradientElement, SVGStopElement, SVGSymbolElement, SVGMarkerElement, SVGPatternElement,
  SVGMaskElement, SVGFilterElement, SVGClipPathElement, SVGTextContentElement, SVGTextPositioningElement,
  SVGTSpanElement, SVGTextPathElement, SVGAElement, SVGStyleElement, SVGTitleElement, SVGDescElement,
  SVGMetadataElement, SVGSwitchElement, SVGForeignObjectElement, SVGScriptElement, SVGAnimationElement,
  SVGSetElement, SVGAnimateElement, SVGAnimateMotionElement, SVGAnimateTransformElement,
  Crypto, SubtleCrypto, CryptoKey,
  VideoTrack, AudioTrack, TextTrack, VideoTrackList, AudioTrackList, TextTrackList,
  MediaError, TimeRanges, MediaStream, FontFaceSet,
  Event, CustomEvent, UIEvent, MouseEvent, KeyboardEvent, FocusEvent, PointerEvent,
  WheelEvent, ErrorEvent, MessageEvent,
  DOMRect, DOMRectReadOnly, DOMMatrix, DOMPoint, TextMetrics, ImageData, Blob, File,
  Headers, Request, Response, AbortController, AbortSignal, URLSearchParams,
  Document, HTMLDocument, DocumentFragment, CharacterData, Text, Comment,
  DOMTokenList, NamedNodeMap, NodeList, HTMLCollection,
  Window, Navigator, NavigatorUAData, Screen, Location, History,
  Performance, PictureInPictureWindow, Storage, Plugin, PluginArray, MimeType, MimeTypeArray, CSSStyleDeclaration
};

/** Publish all interfaces on `target` (globalThis) and mark them native. */
export function installInterfaces(target) {
  for (const name in INTERFACES) {
    const Ctor = INTERFACES[name];
    markClassNative(Ctor);
    try { Object.defineProperty(target, name, { value: Ctor, configurable: true, writable: true }); } catch (_) {}
  }
}

/** Build the singleton `document` (an HTMLDocument), with html/head/body wired. */
export function createDocument() {
  const doc = Object.create(HTMLDocument.prototype);
  doc._childNodes = [];
  doc._listeners = Object.create(null);
  currentDocument = doc;
  doc._ownerDocument = null;
  doc._documentElement = createElement('html');
  doc._head = createElement('head');
  doc._body = createElement('body');
  doc._documentElement.appendChild(doc._head);
  doc._documentElement.appendChild(doc._body);
  doc.appendChild(doc._documentElement);
  return doc;
}

/**
 * Best-effort Window identity: make `Window` real and (guardedly) reparent
 * globalThis onto Window.prototype so `window instanceof Window` holds. Builtins
 * stay reachable because they are own properties of globalThis and
 * Window.prototype chains to the original global prototype. Reverts on any sign
 * of breakage.
 */
export function installWindow(target) {
  try {
    const origProto = Object.getPrototypeOf(target); // normally Object.prototype
    // Reparent globalThis onto the Window chain (Window -> EventTarget -> ... ->
    // Object.prototype). Window's chain already bottoms out at Object.prototype,
    // so global builtins (own props of globalThis) and Object.prototype methods
    // stay reachable, while `window instanceof Window` and `instanceof
    // EventTarget` both hold (matching the platform).
    Object.setPrototypeOf(target, Window.prototype);
    // Revert on any doubt: builtins must still resolve and the original global
    // prototype must remain in the chain.
    if (typeof target.Object !== 'function' || typeof target.hasOwnProperty !== 'function' ||
        !origProto.isPrototypeOf(target)) {
      Object.setPrototypeOf(target, origProto);
    }
  } catch (_) { /* leave globalThis as-is if the engine forbids reparenting */ }
}

/** Native-mark an ad-hoc function (for shim.js host/window functions). */
export function asNative(fn, name) { return markNative(fn, name); }

/**
 * Install coherent timezone on Date: getTimezoneOffset returns the profile's
 * offset, and the local getters are derived from UTC + offset so the whole Date
 * surface agrees.
 *
 * @param {number} offsetMinutes  signed UTC offset in minutes (e.g. -300 = UTC-5)
 */
export function installDateTimezone(offsetMinutes) {
  const off = Number(offsetMinutes) || 0;
  const DP = Date.prototype;
  const getTime = DP.getTime;
  const OFFSET_MS = off * 60000;
  // A Date whose UTC fields equal the local fields of `d` at this offset.
  const shifted = (d) => new Date(getTime.call(d) + OFFSET_MS);

  const set = (name, fn) => { markNative(fn, name); try { Object.defineProperty(DP, name, { value: fn, configurable: true, writable: true }); } catch (_) { DP[name] = fn; } };

  set('getTimezoneOffset', function getTimezoneOffset() { return -off; });
  set('getFullYear', function getFullYear() { return shifted(this).getUTCFullYear(); });
  set('getMonth', function getMonth() { return shifted(this).getUTCMonth(); });
  set('getDate', function getDate() { return shifted(this).getUTCDate(); });
  set('getDay', function getDay() { return shifted(this).getUTCDay(); });
  set('getHours', function getHours() { return shifted(this).getUTCHours(); });
  set('getMinutes', function getMinutes() { return shifted(this).getUTCMinutes(); });
  set('getSeconds', function getSeconds() { return shifted(this).getUTCSeconds(); });
  set('getMilliseconds', function getMilliseconds() { return shifted(this).getUTCMilliseconds(); });
}

/*
 * Broad platform-interface presence battery.
 *
 * BotGuard samples a large set of standard window interfaces. A missing one
 * (for example IDBVersionChangeEvent, MediaRecorderErrorEvent, or
 * StylePropertyMap) can downgrade attestation from integrity to fallback. Unlike
 * DOM element interfaces, these are not createElement-coherence-sensitive:
 * BotGuard checks presence, native-looking toString, and for events the Event
 * chain. Native-looking constructors are enough here without creating the
 * instanceof contradictions that element stubs can introduce.
 *
 * The installer skips names already defined by more specific chains above or by
 * the engine, so it is idempotent.
 */

// Event subtypes: real `extends Event` classes.
const EVENT_BATTERY = ('AnimationEvent AnimationPlaybackEvent BeforeInstallPromptEvent BeforeUnloadEvent ' +
  'BlobEvent ClipboardEvent CloseEvent CompositionEvent ContentVisibilityAutoStateChangeEvent ' +
  'DeviceMotionEvent DeviceOrientationEvent DragEvent FontFaceSetLoadEvent FormDataEvent GamepadEvent ' +
  'HashChangeEvent IDBVersionChangeEvent InputEvent MediaEncryptedEvent MediaQueryListEvent ' +
  'MediaStreamTrackEvent OfflineAudioCompletionEvent PageTransitionEvent ' +
  'PaymentRequestUpdateEvent PopStateEvent ProgressEvent PromiseRejectionEvent RTCDataChannelEvent ' +
  'RTCPeerConnectionIceEvent SecurityPolicyViolationEvent StorageEvent SubmitEvent ToggleEvent ' +
  'TouchEvent TrackEvent TransitionEvent WebGLContextEvent ' +
  // Chrome 144 added XRVisibilityMaskChangeEvent to the WebXR surface.
  'SpeechSynthesisEvent WindowControlsOverlayGeometryChangeEvent XRVisibilityMaskChangeEvent ' +
  // SpeechSynthesisErrorEvent has a different parent and is installed below.
  'SpeechRecognitionEvent SpeechRecognitionErrorEvent ' +
  // WebUSB and WebHID connection events.
  'USBConnectionEvent HIDConnectionEvent HIDInputReportEvent ' +
  // Navigation API events.
  'NavigateEvent NavigationCurrentEntryChangeEvent').split(' ');

// Other standard window interfaces: present native constructors (most platform
// interfaces throw "Illegal constructor" on direct new, which is what we render).
const PRESENCE_BATTERY = (
  // IndexedDB
  'IDBFactory IDBDatabase IDBObjectStore IDBIndex IDBCursor IDBCursorWithValue IDBKeyRange IDBRequest IDBOpenDBRequest IDBTransaction ' +
  // Media / WebAudio / MSE / EME
  'MediaRecorder MediaSource SourceBuffer SourceBufferList MediaStreamTrack CanvasCaptureMediaStreamTrack MediaDevices MediaDeviceInfo MediaCapabilities ' +
  'MediaKeys MediaKeySession MediaKeySystemAccess MediaKeyStatusMap RemotePlayback ' +
  'AudioContext BaseAudioContext OfflineAudioContext AudioNode AudioParam AudioBuffer AudioBufferSourceNode ' +
  'AudioDestinationNode AudioListener AnalyserNode GainNode BiquadFilterNode OscillatorNode DynamicsCompressorNode ' +
  'ConvolverNode DelayNode PannerNode StereoPannerNode WaveShaperNode ChannelMergerNode ChannelSplitterNode ' +
  'ConstantSourceNode IIRFilterNode PeriodicWave AudioWorklet AudioWorkletNode ScriptProcessorNode ' +
  // CSS / Typed OM
  'CSSStyleSheet StyleSheet StyleSheetList MediaList CSSRule CSSRuleList CSSStyleRule CSSMediaRule CSSImportRule ' +
  'CSSKeyframeRule CSSKeyframesRule CSSFontFaceRule CSSSupportsRule CSSNamespaceRule CSSPageRule ' +
  'StylePropertyMap StylePropertyMapReadOnly CSSStyleValue CSSUnitValue CSSKeywordValue CSSMathValue ' +
  'CSSNumericValue CSSTransformValue CSSTransformComponent CSSPerspective CSSImageValue CSSUnparsedValue ' +
  // FontFaceSet is available only through document.fonts.
  'FontFace ' +
  // WebGL objects
  'WebGLBuffer WebGLProgram WebGLShader WebGLTexture WebGLFramebuffer WebGLRenderbuffer WebGLUniformLocation ' +
  'WebGLActiveInfo WebGLShaderPrecisionFormat WebGLVertexArrayObject WebGLQuery WebGLSampler WebGLSync WebGLTransformFeedback ' +
  // Workers / messaging / channels
  // Service worker interfaces are EventTarget-derived and installed below.
  'Worker SharedWorker MessageChannel MessagePort ' +
  'BroadcastChannel Worklet ' +
  // Observers
  'MutationObserver MutationRecord ResizeObserver ResizeObserverEntry ResizeObserverSize IntersectionObserver ' +
  'IntersectionObserverEntry PerformanceObserver PerformanceObserverEntryList ReportingObserver ' +
  // Streams
  'ReadableStream WritableStream TransformStream ReadableStreamDefaultReader ReadableStreamBYOBReader ' +
  'ReadableStreamDefaultController WritableStreamDefaultWriter ByteLengthQueuingStrategy CountQueuingStrategy ' +
  // Crypto / encoding / files / forms / net
  'SubtleCrypto CryptoKey Crypto FileReader FileList FormData WebSocket EventSource XMLHttpRequest XMLHttpRequestUpload ' +
  'XMLHttpRequestEventTarget TextEncoderStream TextDecoderStream CompressionStream DecompressionStream ' +
  // DOM / docs / ranges / shadow
  'DOMException DOMImplementation DOMParser XMLSerializer DOMStringList DOMStringMap DOMTokenList Attr ' +
  'CharacterData CDATASection ProcessingInstruction DocumentType Range StaticRange Selection NodeIterator TreeWalker ' +
  'ShadowRoot CustomElementRegistry XPathEvaluator XPathExpression XPathResult ' +
  // Performance
  'Performance PerformanceEntry PerformanceMark PerformanceMeasure PerformanceNavigationTiming PerformanceResourceTiming ' +
  'PerformancePaintTiming PerformanceServerTiming PerformanceEventTiming PerformanceLongTaskTiming PerformanceTiming PerformanceNavigation ' +
  // Animation
  'Animation AnimationEffect KeyframeEffect AnimationTimeline DocumentTimeline ' +
  // Device / perms / misc
  // EventTarget-derived navigator results are installed below.
  'Notification Permissions Clipboard ClipboardItem Geolocation GeolocationPosition GeolocationCoordinates ' +
  'GeolocationPositionError Gamepad GamepadButton VisualViewport BarProp External ' +
  'Touch TouchList ImageBitmap ImageBitmapRenderingContext Path2D OffscreenCanvas OffscreenCanvasRenderingContext2D ' +
  'IdleDeadline Image Audio Option ' +
  // Additional browser interfaces modeled by the shim.
  'Observable PressureRecord XRInputSource IdentityCredentialError Keyboard UserActivation Ink InkPresenter ' +
  'CrashReportContext WindowControlsOverlay ' +
  // RTC
  'RTCPeerConnection RTCDataChannel RTCSessionDescription RTCIceCandidate RTCRtpSender RTCRtpReceiver RTCRtpTransceiver ' +
  // SVG value types (not elements; presence is correct) plus the animated
  // wrappers and filter-effect element set.
  'SVGAngle SVGLength SVGLengthList SVGNumber SVGNumberList SVGPoint SVGPointList SVGRect SVGMatrix ' +
  'SVGTransform SVGTransformList SVGPreserveAspectRatio SVGStringList SVGUnitTypes ' +
  'SVGAnimatedAngle SVGAnimatedBoolean SVGAnimatedEnumeration SVGAnimatedInteger SVGAnimatedLength ' +
  'SVGAnimatedLengthList SVGAnimatedNumber SVGAnimatedNumberList SVGAnimatedPreserveAspectRatio ' +
  'SVGAnimatedRect SVGAnimatedString SVGAnimatedTransformList SVGComponentTransferFunctionElement ' +
  'SVGFEBlendElement SVGFEColorMatrixElement SVGFEComponentTransferElement SVGFECompositeElement ' +
  'SVGFEConvolveMatrixElement SVGFEDiffuseLightingElement SVGFEDisplacementMapElement SVGFEDistantLightElement ' +
  'SVGFEDropShadowElement SVGFEFloodElement SVGFEFuncAElement SVGFEFuncBElement SVGFEFuncGElement ' +
  'SVGFEFuncRElement SVGFEGaussianBlurElement SVGFEImageElement SVGFEMergeElement SVGFEMergeNodeElement ' +
  'SVGFEMorphologyElement SVGFEOffsetElement SVGFEPointLightElement SVGFESpecularLightingElement ' +
  'SVGFESpotLightElement SVGFETileElement SVGFETurbulenceElement ' +
  // File System Access API.
  'FileSystemHandle FileSystemFileHandle FileSystemDirectoryHandle FileSystemWritableFileStream ' +
  'DataTransfer DataTransferItem DataTransferItemList ' +
  'PointerEvent ScreenOrientation MediaQueryList Highlight HighlightRegistry ' +
  // Plain hardware, storage, credential, and media-session interfaces.
  'GPU GPUAdapter GPUCanvasContext GPUDeviceLostInfo USBDevice BluetoothRemoteGATTServer ' +
  'CredentialsContainer Credential ' +
  'StorageManager LockManager Lock StorageBucketManager StorageBucket Scheduling ' +
  'MediaSession MediaMetadata Presentation WakeLock GamepadHapticActuator ' +
  // Web Speech interfaces that throw on direct construction. Chrome 149 does
  // not expose the SpeechRecognition result interfaces as window globals.
  'SpeechGrammar SpeechSynthesisVoice SpeechSynthesis ' +
  // Navigation API no-constructor interfaces (no parent in the snapshot).
  'NavigationDestination NavigationTransition NavigationActivation NavigationPrecommitController NavigationPreloadManager ' +
  // Trusted Types interfaces and the FedCM IdentityProvider.
  'TrustedTypePolicyFactory TrustedTypePolicy TrustedHTML TrustedScript TrustedScriptURL IdentityProvider ' +
  // Additional no-constructor interfaces observed by shim discovery.
  'FetchLaterResult GPUQueue'
).split(' ');

function rename(fn, name) { try { Object.defineProperty(fn, 'name', { value: name, configurable: true }); } catch (_) {} return fn; }

// Constructible interfaces grouped by prototype parent.
const EVENTTARGET_CONSTRUCTIBLE_BATTERY = 'SpeechRecognition SpeechSynthesisUtterance CloseWatcher'.split(' ');
const PLAIN_CONSTRUCTIBLE_BATTERY = 'SpeechGrammarList SpeechRecognitionPhrase PressureObserver'.split(' ');
// webkit-prefixed names share their unprefixed constructor object.
const WEBKIT_ALIASES = {
  webkitSpeechRecognition: 'SpeechRecognition',
  webkitSpeechRecognitionError: 'SpeechRecognitionErrorEvent',
  webkitSpeechRecognitionEvent: 'SpeechRecognitionEvent',
  webkitSpeechGrammar: 'SpeechGrammar',
  webkitSpeechGrammarList: 'SpeechGrammarList',
  webkitMediaStream: 'MediaStream',
};
// These interfaces throw on direct construction but inherit from EventTarget.
const EVENTTARGET_PRESENCE_BATTERY = ('GPUDevice USB HID HIDDevice Serial SerialPort Bluetooth BluetoothDevice ' +
  'XRSystem XRSession XRLayer PresentationRequest PresentationConnection WakeLockSentinel DevicePosture ' +
  'VirtualKeyboard NavigatorManagedData ServiceWorker ServiceWorkerContainer ServiceWorkerRegistration ' +
  'PermissionStatus BatteryManager NetworkInformation ' +
  // Navigation API and text-track base interfaces.
  'Navigation NavigationHistoryEntry TextTrackCue').split(' ');
// No-constructor interfaces with named parents. Parents must be installed first.
const DERIVED_PRESENCE_BATTERY = [
  ['CSSGroupingRule', 'CSSRule'],
  ['CSSFunctionRule', 'CSSGroupingRule'],
  ['CSSPositionValue', 'CSSStyleValue'],
  ['SharedStorageModifierMethod', null],
  ['SharedStorageAppendMethod', 'SharedStorageModifierMethod'],
  ['SharedStorageSetMethod', 'SharedStorageModifierMethod'],
  ['SharedStorageDeleteMethod', 'SharedStorageModifierMethod'],
  ['SharedStorageClearMethod', 'SharedStorageModifierMethod'],
  // Credential and WebXR subclasses are constructable in the Chrome snapshot.
  ['FederatedCredential', 'Credential'],
  ['PasswordCredential', 'Credential'],
  ['PublicKeyCredential', 'Credential'],
  ['IdentityCredential', 'Credential'],
  ['XRWebGLLayer', 'XRLayer'],
  // WebXR layer hierarchy.
  ['XRCompositionLayer', 'XRLayer'],
  ['XRCylinderLayer', 'XRCompositionLayer'],
  ['XRProjectionLayer', 'XRCompositionLayer'],
  ['XRQuadLayer', 'XRCompositionLayer'],
  ['XREquirectLayer', 'XRCompositionLayer'],
  ['XRCubeLayer', 'XRCompositionLayer'],
];

export function installPlatformBattery(target) {
  const define = (name, C) => { try { Object.defineProperty(target, name, { value: C, configurable: true, writable: true }); } catch (_) {} };
  for (const name of EVENT_BATTERY) {
    if (typeof target[name] !== 'undefined') continue; // do not overwrite a more specific def
    const C = class extends Event {};
    rename(C, name); markNative(C, name); define(name, C);
  }
  // Install after its SpeechSynthesisEvent parent.
  if (typeof target.SpeechSynthesisErrorEvent === 'undefined' && typeof target.SpeechSynthesisEvent === 'function') {
    const C = class extends target.SpeechSynthesisEvent {};
    rename(C, 'SpeechSynthesisErrorEvent'); markNative(C, 'SpeechSynthesisErrorEvent'); define('SpeechSynthesisErrorEvent', C);
  }
  for (const name of EVENTTARGET_CONSTRUCTIBLE_BATTERY) {
    if (typeof target[name] !== 'undefined') continue;
    const C = class extends EventTarget {};
    rename(C, name); markNative(C, name); define(name, C);
  }
  for (const name of PLAIN_CONSTRUCTIBLE_BATTERY) {
    if (typeof target[name] !== 'undefined') continue;
    const C = class {};
    rename(C, name); markNative(C, name); define(name, C);
  }
  for (const name of EVENTTARGET_PRESENCE_BATTERY) {
    if (typeof target[name] !== 'undefined') continue;
    const C = class extends EventTarget { constructor() { super(); illegal(); } };
    rename(C, name); markNative(C, name); define(name, C);
  }
  for (const name of PRESENCE_BATTERY) {
    if (typeof target[name] !== 'undefined') continue;
    // Image/Audio/Option are legacy element factories: `new Image()` is an
    // HTMLImageElement. Wire those to createElement so instanceof stays coherent.
    let C;
    if (name === 'Image') { C = function Image(w, h) { const el = createElement('img'); if (w != null) el._width = w | 0; if (h != null) el._height = h | 0; return el; }; C.prototype = HTMLImageElement.prototype; }
    else if (name === 'Audio') { C = function Audio() { return createElement('audio'); }; C.prototype = HTMLAudioElement.prototype; }
    else if (name === 'Option') { C = function Option() { return createElement('option'); }; C.prototype = HTMLOptionElement.prototype; }
    else { C = function () { illegal(); }; }
    rename(C, name); markNative(C, name); define(name, C);
  }
  // Install no-constructor interfaces after their named parents.
  for (const [name, parent] of DERIVED_PRESENCE_BATTERY) {
    if (typeof target[name] !== 'undefined') continue;
    const Base = parent ? target[parent] : null;
    const C = (typeof Base === 'function')
      ? class extends Base { constructor() { super(); illegal(); } }
      : function () { illegal(); };
    rename(C, name); markNative(C, name); define(name, C);
  }
  // Install aliases last so their base constructors already exist.
  for (const alias in WEBKIT_ALIASES) {
    const base = target[WEBKIT_ALIASES[alias]];
    if (typeof target[alias] === 'undefined' && typeof base !== 'undefined') define(alias, base);
  }
}

// Install the interface battery onto globalThis at import time so shim.js can
// build `document` and mint elements immediately.
installInterfaces(G);
installPlatformBattery(G);
