// GENERATED - do not edit. Source: build/js/{shim,entrypoint}.js + bgutils-js@3.2.0.
// Rebuild: make jsbundle (esbuild@0.25.12, target es2020 IIFE).
(() => {
  var __defProp = Object.defineProperty;
  var __export = (target, all) => {
    for (var name in all)
      __defProp(target, name, { get: all[name], enumerable: true });
  };

  // dom.js
  var G = globalThis;
  var NATIVE = /* @__PURE__ */ new WeakMap();
  function markNative(fn, name) {
    if (typeof fn === "function") NATIVE.set(fn, name == null ? fn.name || "" : name);
    return fn;
  }
  function installNativeToString() {
    const orig = Function.prototype.toString;
    function toString() {
      const name = NATIVE.get(this);
      if (name !== void 0) return "function " + name + "() { [native code] }";
      return orig.call(this);
    }
    NATIVE.set(toString, "toString");
    try {
      Object.defineProperty(toString, "name", { value: "toString", configurable: true });
    } catch (_) {
    }
    try {
      Object.defineProperty(toString, "length", { value: 0, configurable: true });
    } catch (_) {
    }
    Function.prototype.toString = toString;
  }
  installNativeToString();
  function markClassNative(Ctor) {
    markNative(Ctor, Ctor.name);
    const visit = (obj, isProto) => {
      for (const key of Object.getOwnPropertyNames(obj)) {
        if (isProto && key === "constructor") continue;
        const d = Object.getOwnPropertyDescriptor(obj, key);
        if (!d) continue;
        if (typeof d.value === "function") markNative(d.value, key);
        if (typeof d.get === "function") markNative(d.get, "get " + key);
        if (typeof d.set === "function") markNative(d.set, "set " + key);
      }
    };
    visit(Ctor.prototype, true);
    visit(Ctor, false);
  }
  function illegal() {
    throw new TypeError("Illegal constructor");
  }
  var EventTarget = class {
    addEventListener(type, cb) {
      if (!this._listeners) this._listeners = /* @__PURE__ */ Object.create(null);
      (this._listeners[type] || (this._listeners[type] = [])).push(cb);
    }
    removeEventListener(type, cb) {
      const l = this._listeners && this._listeners[type];
      if (l) {
        const i = l.indexOf(cb);
        if (i >= 0) l.splice(i, 1);
      }
    }
    dispatchEvent(_evt) {
      return true;
    }
  };
  var Node = class extends EventTarget {
    constructor() {
      super();
      illegal();
    }
    get nodeType() {
      return this._nodeType == null ? 1 : this._nodeType;
    }
    get nodeName() {
      return this._nodeName || this._tagName || "#node";
    }
    get nodeValue() {
      return this._nodeValue == null ? null : this._nodeValue;
    }
    set nodeValue(v) {
      this._nodeValue = v;
    }
    get ownerDocument() {
      return this._ownerDocument || null;
    }
    get parentNode() {
      return this._parentNode || null;
    }
    get parentElement() {
      return this._parentNode || null;
    }
    get childNodes() {
      return this._childNodes || (this._childNodes = []);
    }
    get firstChild() {
      return this.childNodes[0] || null;
    }
    get lastChild() {
      return this.childNodes[this.childNodes.length - 1] || null;
    }
    get nextSibling() {
      return null;
    }
    get previousSibling() {
      return null;
    }
    get textContent() {
      return this._textContent || "";
    }
    set textContent(v) {
      this._textContent = String(v);
    }
    get isConnected() {
      return true;
    }
    hasChildNodes() {
      return this.childNodes.length > 0;
    }
    appendChild(child) {
      this.childNodes.push(child);
      if (child) child._parentNode = this;
      return child;
    }
    removeChild(child) {
      const i = this.childNodes.indexOf(child);
      if (i >= 0) this.childNodes.splice(i, 1);
      return child;
    }
    insertBefore(child, _ref) {
      this.childNodes.push(child);
      if (child) child._parentNode = this;
      return child;
    }
    replaceChild(n, _o) {
      return n;
    }
    cloneNode(_deep) {
      const c = Object.create(Object.getPrototypeOf(this));
      initElement(c, this._localName || "div", this._namespaceURI);
      return c;
    }
    contains(_n) {
      return false;
    }
    getRootNode() {
      return this._ownerDocument || this;
    }
    compareDocumentPosition() {
      return 0;
    }
  };
  Node.ELEMENT_NODE = 1;
  Node.TEXT_NODE = 3;
  Node.DOCUMENT_NODE = 9;
  Node.prototype.ELEMENT_NODE = 1;
  Node.prototype.TEXT_NODE = 3;
  Node.prototype.DOCUMENT_NODE = 9;
  var Element = class extends Node {
    constructor() {
      super();
    }
    get tagName() {
      return this._tagName || "";
    }
    get localName() {
      return this._localName || "";
    }
    get namespaceURI() {
      return this._namespaceURI || "http://www.w3.org/1999/xhtml";
    }
    get id() {
      return this._attributes.id || "";
    }
    set id(v) {
      this._attributes.id = String(v);
    }
    get className() {
      return this._attributes.class || "";
    }
    set className(v) {
      this._attributes.class = String(v);
    }
    get classList() {
      return this._classList || (this._classList = makeClassList(this));
    }
    get attributes() {
      return this._attrNodes || (this._attrNodes = makeAttrList(this));
    }
    get children() {
      return this.childNodes.filter((n) => n.nodeType === 1);
    }
    get childElementCount() {
      return this.children.length;
    }
    get innerHTML() {
      return this._innerHTML || "";
    }
    set innerHTML(v) {
      this._innerHTML = String(v);
    }
    get outerHTML() {
      return "<" + (this._localName || "") + "></" + (this._localName || "") + ">";
    }
    getAttribute(name) {
      const v = this._attributes[String(name)];
      return v == null ? null : String(v);
    }
    getAttributeNS(_ns, name) {
      return this.getAttribute(name);
    }
    setAttribute(name, value) {
      this._attributes[String(name)] = String(value);
    }
    setAttributeNS(_ns, name, value) {
      this.setAttribute(name, value);
    }
    removeAttribute(name) {
      delete this._attributes[String(name)];
    }
    hasAttribute(name) {
      return Object.prototype.hasOwnProperty.call(this._attributes, String(name));
    }
    hasAttributes() {
      return Object.keys(this._attributes).length > 0;
    }
    getAttributeNames() {
      return Object.keys(this._attributes);
    }
    matches(_sel) {
      return false;
    }
    closest(_sel) {
      return null;
    }
    querySelector(_sel) {
      return null;
    }
    querySelectorAll(_sel) {
      return makeNodeList([]);
    }
    getElementsByTagName(_t) {
      return makeNodeList([]);
    }
    getElementsByClassName(_c) {
      return makeNodeList([]);
    }
    getBoundingClientRect() {
      return new DOMRect(0, 0, this._offsetWidth || 0, this._offsetHeight || 0);
    }
    getClientRects() {
      return [this.getBoundingClientRect()];
    }
    append() {
    }
    prepend() {
    }
    after() {
    }
    before() {
    }
    remove() {
    }
    insertAdjacentElement(_p, el) {
      return el;
    }
    insertAdjacentHTML() {
    }
    scrollIntoView() {
    }
    attachShadow() {
      return null;
    }
  };
  var HTMLElement = class extends Element {
    constructor() {
      super();
    }
    get style() {
      return this._style || (this._style = makeStyle());
    }
    get dataset() {
      return this._dataset || (this._dataset = {});
    }
    get hidden() {
      return !!this._hidden;
    }
    set hidden(v) {
      this._hidden = !!v;
    }
    get tabIndex() {
      return this._tabIndex == null ? -1 : this._tabIndex;
    }
    set tabIndex(v) {
      this._tabIndex = v | 0;
    }
    get title() {
      return this._attributes.title || "";
    }
    set title(v) {
      this._attributes.title = String(v);
    }
    get lang() {
      return this._attributes.lang || "";
    }
    get dir() {
      return this._attributes.dir || "";
    }
    get innerText() {
      return this._textContent || "";
    }
    set innerText(v) {
      this._textContent = String(v);
    }
    get contentEditable() {
      return "inherit";
    }
    get isContentEditable() {
      return false;
    }
    get offsetParent() {
      return this._ownerDocument ? this._ownerDocument.body : null;
    }
    get offsetWidth() {
      return this._offsetWidth || 0;
    }
    get offsetHeight() {
      return this._offsetHeight || 0;
    }
    get offsetTop() {
      return 0;
    }
    get offsetLeft() {
      return 0;
    }
    get clientWidth() {
      return this._offsetWidth || 0;
    }
    get clientHeight() {
      return this._offsetHeight || 0;
    }
    get clientTop() {
      return 0;
    }
    get clientLeft() {
      return 0;
    }
    get scrollWidth() {
      return this._offsetWidth || 0;
    }
    get scrollHeight() {
      return this._offsetHeight || 0;
    }
    click() {
    }
    focus() {
    }
    blur() {
    }
  };
  var HTMLUnknownElement = class extends HTMLElement {
  };
  var HTMLHtmlElement = class extends HTMLElement {
  };
  var HTMLHeadElement = class extends HTMLElement {
  };
  var HTMLBodyElement = class extends HTMLElement {
  };
  var HTMLDivElement = class extends HTMLElement {
  };
  var HTMLSpanElement = class extends HTMLElement {
  };
  var HTMLParagraphElement = class extends HTMLElement {
  };
  var HTMLAnchorElement = class extends HTMLElement {
    get href() {
      return this._attributes.href || "";
    }
    set href(v) {
      this._attributes.href = String(v);
    }
    get protocol() {
      return "https:";
    }
    get host() {
      return "www.youtube.com";
    }
    get hostname() {
      return "www.youtube.com";
    }
    get pathname() {
      return "/";
    }
    get search() {
      return "";
    }
    get hash() {
      return "";
    }
    toString() {
      return this.href;
    }
  };
  var HTMLImageElement = class extends HTMLElement {
    get width() {
      return this._width || 0;
    }
    set width(v) {
      this._width = v | 0;
    }
    get height() {
      return this._height || 0;
    }
    set height(v) {
      this._height = v | 0;
    }
    get naturalWidth() {
      return this._width || 0;
    }
    get naturalHeight() {
      return this._height || 0;
    }
    get complete() {
      return true;
    }
    get src() {
      return this._attributes.src || "";
    }
    set src(v) {
      this._attributes.src = String(v);
    }
  };
  var HTMLScriptElement = class extends HTMLElement {
    get src() {
      return this._attributes.src || "";
    }
    set src(v) {
      this._attributes.src = String(v);
    }
    get type() {
      return this._attributes.type || "";
    }
    get async() {
      return !!this._async;
    }
    set async(v) {
      this._async = !!v;
    }
  };
  var HTMLLinkElement = class extends HTMLElement {
  };
  var HTMLStyleElement = class extends HTMLElement {
  };
  var HTMLMetaElement = class extends HTMLElement {
  };
  var HTMLIFrameElement = class extends HTMLElement {
    get contentWindow() {
      return G.window || G;
    }
    get contentDocument() {
      return G.document || null;
    }
  };
  var HTMLInputElement = class extends HTMLElement {
    get value() {
      return this._value || "";
    }
    set value(v) {
      this._value = String(v);
    }
    get type() {
      return this._attributes.type || "text";
    }
    get checked() {
      return !!this._checked;
    }
    set checked(v) {
      this._checked = !!v;
    }
  };
  var HTMLButtonElement = class extends HTMLElement {
  };
  var HTMLFormElement = class extends HTMLElement {
  };
  var HTMLSelectElement = class extends HTMLElement {
  };
  var HTMLOptionElement = class extends HTMLElement {
  };
  var HTMLTextAreaElement = class extends HTMLElement {
  };
  var HTMLTableElement = class extends HTMLElement {
  };
  var HTMLUListElement = class extends HTMLElement {
  };
  var HTMLLIElement = class extends HTMLElement {
  };
  var HTMLLabelElement = class extends HTMLElement {
  };
  var HTMLPictureElement = class extends HTMLElement {
  };
  var HTMLSourceElement = class extends HTMLElement {
  };
  var HTMLTemplateElement = class extends HTMLElement {
    get content() {
      return this._content || (this._content = createDocumentFragment());
    }
  };
  var HTMLBRElement = class extends HTMLElement {
  };
  var HTMLHRElement = class extends HTMLElement {
  };
  var HTMLPreElement = class extends HTMLElement {
  };
  var HTMLQuoteElement = class extends HTMLElement {
  };
  var HTMLDListElement = class extends HTMLElement {
  };
  var HTMLOListElement = class extends HTMLElement {
  };
  var HTMLFieldSetElement = class extends HTMLElement {
  };
  var HTMLLegendElement = class extends HTMLElement {
  };
  var HTMLDataElement = class extends HTMLElement {
  };
  var HTMLDataListElement = class extends HTMLElement {
  };
  var HTMLDetailsElement = class extends HTMLElement {
  };
  var HTMLDialogElement = class extends HTMLElement {
  };
  var HTMLEmbedElement = class extends HTMLElement {
  };
  var HTMLObjectElement = class extends HTMLElement {
  };
  var HTMLMapElement = class extends HTMLElement {
  };
  var HTMLAreaElement = class extends HTMLElement {
  };
  var HTMLBaseElement = class extends HTMLElement {
  };
  var HTMLTitleElement = class extends HTMLElement {
  };
  var HTMLMenuElement = class extends HTMLElement {
  };
  var HTMLMeterElement = class extends HTMLElement {
    get value() {
      return this._value || 0;
    }
    get max() {
      return this._max == null ? 1 : this._max;
    }
  };
  var HTMLProgressElement = class extends HTMLElement {
    get value() {
      return this._value || 0;
    }
    get max() {
      return this._max == null ? 1 : this._max;
    }
    get position() {
      return -1;
    }
  };
  var HTMLOutputElement = class extends HTMLElement {
  };
  var HTMLModElement = class extends HTMLElement {
  };
  var HTMLHeadingElement = class extends HTMLElement {
  };
  var HTMLOptGroupElement = class extends HTMLElement {
  };
  var HTMLSlotElement = class extends HTMLElement {
    assignedNodes() {
      return [];
    }
    assignedElements() {
      return [];
    }
  };
  var HTMLTimeElement = class extends HTMLElement {
  };
  var HTMLTrackElement = class extends HTMLElement {
  };
  var HTMLTableCaptionElement = class extends HTMLElement {
  };
  var HTMLTableCellElement = class extends HTMLElement {
  };
  var HTMLTableColElement = class extends HTMLElement {
  };
  var HTMLTableRowElement = class extends HTMLElement {
  };
  var HTMLTableSectionElement = class extends HTMLElement {
  };
  var HTMLMarqueeElement = class extends HTMLElement {
  };
  var HTMLFontElement = class extends HTMLElement {
  };
  var HTMLParamElement = class extends HTMLElement {
  };
  var HTMLFrameElement = class extends HTMLElement {
  };
  var HTMLFrameSetElement = class extends HTMLElement {
  };
  var HTMLDirectoryElement = class extends HTMLElement {
  };
  var HTMLCanvasElement = class extends HTMLElement {
    get width() {
      return this._width == null ? 300 : this._width;
    }
    set width(v) {
      this._width = v | 0;
    }
    get height() {
      return this._height == null ? 150 : this._height;
    }
    set height(v) {
      this._height = v | 0;
    }
    getContext(type, attrs) {
      return getCanvasContext(this, String(type), attrs);
    }
    toDataURL(type) {
      return canvasToDataURL(this, type);
    }
    toBlob(cb) {
      if (typeof cb === "function") cb(new Blob([], { type: "image/png" }));
    }
    captureStream() {
      return new MediaStream();
    }
  };
  var CanvasRenderingContext2D = class {
    constructor() {
      illegal();
    }
    getContextAttributes() {
      return { alpha: true, desynchronized: false, colorSpace: "srgb", willReadFrequently: false };
    }
    save() {
    }
    restore() {
    }
    scale() {
    }
    rotate() {
    }
    translate() {
    }
    transform() {
    }
    setTransform() {
    }
    resetTransform() {
    }
    beginPath() {
    }
    closePath() {
    }
    moveTo() {
    }
    lineTo() {
    }
    bezierCurveTo() {
    }
    quadraticCurveTo() {
    }
    arc() {
    }
    arcTo() {
    }
    ellipse() {
    }
    rect() {
    }
    roundRect() {
    }
    fill() {
    }
    stroke() {
    }
    clip() {
    }
    isPointInPath() {
      return false;
    }
    isPointInStroke() {
      return false;
    }
    fillRect() {
    }
    strokeRect() {
    }
    clearRect() {
    }
    fillText() {
    }
    strokeText() {
    }
    measureText(text) {
      return new TextMetrics(String(text == null ? "" : text));
    }
    createLinearGradient() {
      return makeGradient();
    }
    createRadialGradient() {
      return makeGradient();
    }
    createConicGradient() {
      return makeGradient();
    }
    createPattern() {
      return null;
    }
    drawImage() {
    }
    putImageData() {
    }
    getImageData(_x, _y, w, h) {
      return new ImageData(w | 0 || 1, h | 0 || 1);
    }
    createImageData(w, h) {
      return new ImageData(w | 0 || 1, h | 0 || 1);
    }
    setLineDash() {
    }
    getLineDash() {
      return [];
    }
    getTransform() {
      return new DOMMatrix();
    }
  };
  var WebGLRenderingContext = class {
    constructor() {
      illegal();
    }
    getParameter(p) {
      return WEBGL_PARAMS.hasOwnProperty(p) ? WEBGL_PARAMS[p] : null;
    }
    getExtension(name) {
      if (name === "WEBGL_debug_renderer_info") return { UNMASKED_VENDOR_WEBGL: 37445, UNMASKED_RENDERER_WEBGL: 37446 };
      if (WEBGL_EXTENSIONS.indexOf(name) >= 0) return {};
      return null;
    }
    getSupportedExtensions() {
      return WEBGL_EXTENSIONS.slice();
    }
    getContextAttributes() {
      return { alpha: true, antialias: true, depth: true, desynchronized: false, failIfMajorPerformanceCaveat: false, powerPreference: "default", premultipliedAlpha: true, preserveDrawingBuffer: false, stencil: false, xrCompatible: false };
    }
    getShaderPrecisionFormat() {
      return { rangeMin: 127, rangeMax: 127, precision: 23 };
    }
    createBuffer() {
      return {};
    }
    createProgram() {
      return {};
    }
    createShader() {
      return {};
    }
    createTexture() {
      return {};
    }
    createFramebuffer() {
      return {};
    }
    createRenderbuffer() {
      return {};
    }
    bindBuffer() {
    }
    bufferData() {
    }
    bindTexture() {
    }
    texParameteri() {
    }
    texImage2D() {
    }
    shaderSource() {
    }
    compileShader() {
    }
    attachShader() {
    }
    linkProgram() {
    }
    useProgram() {
    }
    getAttribLocation() {
      return 0;
    }
    getUniformLocation() {
      return {};
    }
    enableVertexAttribArray() {
    }
    vertexAttribPointer() {
    }
    drawArrays() {
    }
    drawElements() {
    }
    viewport() {
    }
    clearColor() {
    }
    clear() {
    }
    enable() {
    }
    disable() {
    }
    getError() {
      return 0;
    }
    // NO_ERROR
    readPixels() {
    }
  };
  var WebGL2RenderingContext = class extends WebGLRenderingContext {
  };
  var HTMLMediaElement = class extends HTMLElement {
    get currentTime() {
      return 0;
    }
    set currentTime(_v) {
    }
    get duration() {
      return NaN;
    }
    get paused() {
      return true;
    }
    get muted() {
      return false;
    }
    set muted(_v) {
    }
    get volume() {
      return 1;
    }
    set volume(_v) {
    }
    get readyState() {
      return 0;
    }
    get networkState() {
      return 0;
    }
    get videoTracks() {
      return this._videoTracks || (this._videoTracks = new VideoTrackList());
    }
    get audioTracks() {
      return this._audioTracks || (this._audioTracks = new AudioTrackList());
    }
    get textTracks() {
      return this._textTracks || (this._textTracks = new TextTrackList());
    }
    canPlayType(type) {
      type = String(type || "");
      if (/mp4|h264|avc1|mpeg|aac/i.test(type)) return "probably";
      if (/webm|ogg|vorbis|opus|vp9|vp8/i.test(type)) return "maybe";
      return "";
    }
    load() {
    }
    play() {
      return Promise.resolve();
    }
    pause() {
    }
    addTextTrack() {
      return new TextTrack();
    }
  };
  var HTMLVideoElement = class extends HTMLMediaElement {
    get videoWidth() {
      return this._videoWidth || 0;
    }
    get videoHeight() {
      return this._videoHeight || 0;
    }
    getVideoPlaybackQuality() {
      return { creationTime: 0, droppedVideoFrames: 0, totalVideoFrames: 0 };
    }
  };
  var HTMLAudioElement = class extends HTMLMediaElement {
  };
  var SVGElement = class extends Element {
    constructor() {
      super();
    }
    get style() {
      return this._style || (this._style = makeStyle());
    }
    get ownerSVGElement() {
      return null;
    }
  };
  var SVGGraphicsElement = class extends SVGElement {
  };
  var SVGSVGElement = class extends SVGGraphicsElement {
    createSVGRect() {
      return new DOMRect(0, 0, 0, 0);
    }
    createSVGPoint() {
      return { x: 0, y: 0, matrixTransform() {
        return this;
      } };
    }
    createSVGMatrix() {
      return new DOMMatrix();
    }
    getCurrentTime() {
      return 0;
    }
  };
  var SVGGElement = class extends SVGGraphicsElement {
  };
  var SVGPathElement = class extends SVGGraphicsElement {
  };
  var SVGRectElement = class extends SVGGraphicsElement {
  };
  var SVGTextElement = class extends SVGGraphicsElement {
  };
  var SVGImageElement = class extends SVGGraphicsElement {
  };
  var SVGUseElement = class extends SVGGraphicsElement {
  };
  var SVGViewElement = class extends SVGElement {
  };
  var SVGGeometryElement = class extends SVGGraphicsElement {
  };
  var SVGCircleElement = class extends SVGGeometryElement {
  };
  var SVGEllipseElement = class extends SVGGeometryElement {
  };
  var SVGLineElement = class extends SVGGeometryElement {
  };
  var SVGPolygonElement = class extends SVGGeometryElement {
  };
  var SVGPolylineElement = class extends SVGGeometryElement {
  };
  var SVGDefsElement = class extends SVGGraphicsElement {
  };
  var SVGGradientElement = class extends SVGElement {
  };
  var SVGLinearGradientElement = class extends SVGGradientElement {
  };
  var SVGRadialGradientElement = class extends SVGGradientElement {
  };
  var SVGStopElement = class extends SVGElement {
  };
  var SVGSymbolElement = class extends SVGGraphicsElement {
  };
  var SVGMarkerElement = class extends SVGElement {
  };
  var SVGPatternElement = class extends SVGElement {
  };
  var SVGMaskElement = class extends SVGElement {
  };
  var SVGFilterElement = class extends SVGElement {
  };
  var SVGClipPathElement = class extends SVGElement {
  };
  var SVGTextContentElement = class extends SVGGraphicsElement {
  };
  var SVGTextPositioningElement = class extends SVGTextContentElement {
  };
  var SVGTSpanElement = class extends SVGTextPositioningElement {
  };
  var SVGTextPathElement = class extends SVGTextContentElement {
  };
  var SVGAElement = class extends SVGGraphicsElement {
  };
  var SVGStyleElement = class extends SVGElement {
  };
  var SVGTitleElement = class extends SVGElement {
  };
  var SVGDescElement = class extends SVGElement {
  };
  var SVGMetadataElement = class extends SVGElement {
  };
  var SVGSwitchElement = class extends SVGGraphicsElement {
  };
  var SVGForeignObjectElement = class extends SVGGraphicsElement {
  };
  var SVGScriptElement = class extends SVGElement {
  };
  var SVGAnimationElement = class extends SVGElement {
  };
  var SVGSetElement = class extends SVGAnimationElement {
  };
  var SVGAnimateElement = class extends SVGAnimationElement {
  };
  var SVGAnimateMotionElement = class extends SVGAnimationElement {
  };
  var SVGAnimateTransformElement = class extends SVGAnimationElement {
  };
  var SVG_INTERFACES = {
    "svg": SVGSVGElement,
    "g": SVGGElement,
    "path": SVGPathElement,
    "rect": SVGRectElement,
    "text": SVGTextElement,
    "image": SVGImageElement,
    "use": SVGUseElement,
    "view": SVGViewElement,
    "circle": SVGCircleElement,
    "ellipse": SVGEllipseElement,
    "line": SVGLineElement,
    "polygon": SVGPolygonElement,
    "polyline": SVGPolylineElement,
    "defs": SVGDefsElement,
    "lineargradient": SVGLinearGradientElement,
    "radialgradient": SVGRadialGradientElement,
    "stop": SVGStopElement,
    "symbol": SVGSymbolElement,
    "marker": SVGMarkerElement,
    "pattern": SVGPatternElement,
    "mask": SVGMaskElement,
    "filter": SVGFilterElement,
    "clippath": SVGClipPathElement,
    "tspan": SVGTSpanElement,
    "textpath": SVGTextPathElement,
    "a": SVGAElement,
    "style": SVGStyleElement,
    "title": SVGTitleElement,
    "desc": SVGDescElement,
    "metadata": SVGMetadataElement,
    "switch": SVGSwitchElement,
    "foreignobject": SVGForeignObjectElement,
    "script": SVGScriptElement,
    "set": SVGSetElement,
    "animate": SVGAnimateElement,
    "animatemotion": SVGAnimateMotionElement,
    "animatetransform": SVGAnimateTransformElement
  };
  var VideoTrack = class {
    constructor() {
      illegal();
    }
    get id() {
      return "";
    }
    get kind() {
      return "";
    }
    get label() {
      return "";
    }
    get language() {
      return "";
    }
    get selected() {
      return false;
    }
  };
  var AudioTrack = class {
    constructor() {
      illegal();
    }
    get id() {
      return "";
    }
    get kind() {
      return "";
    }
    get label() {
      return "";
    }
    get language() {
      return "";
    }
    get enabled() {
      return true;
    }
  };
  var TextTrack = class extends EventTarget {
    get kind() {
      return "subtitles";
    }
    get mode() {
      return "disabled";
    }
    addCue() {
    }
    removeCue() {
    }
  };
  var VideoTrackList = class extends EventTarget {
    get length() {
      return 0;
    }
    getTrackById() {
      return null;
    }
  };
  var AudioTrackList = class extends EventTarget {
    get length() {
      return 0;
    }
    getTrackById() {
      return null;
    }
  };
  var TextTrackList = class extends EventTarget {
    get length() {
      return 0;
    }
    getTrackById() {
      return null;
    }
  };
  var MediaError = class {
    get code() {
      return 0;
    }
    get message() {
      return "";
    }
  };
  var TimeRanges = class {
    get length() {
      return 0;
    }
    start() {
      return 0;
    }
    end() {
      return 0;
    }
  };
  var FontFaceSet = class extends EventTarget {
    get size() {
      return 0;
    }
    get status() {
      return "loaded";
    }
    get ready() {
      return Promise.resolve(this);
    }
    check() {
      return true;
    }
    load() {
      return Promise.resolve([]);
    }
    add() {
      return this;
    }
    delete() {
      return false;
    }
    has() {
      return false;
    }
    clear() {
    }
    forEach() {
    }
  };
  var MediaStream = class extends EventTarget {
    get active() {
      return false;
    }
    get id() {
      return "";
    }
    getTracks() {
      return [];
    }
    getVideoTracks() {
      return [];
    }
    getAudioTracks() {
      return [];
    }
  };
  var Event = class {
    constructor(type, init) {
      init = init || {};
      this.type = String(type == null ? "" : type);
      this.bubbles = !!init.bubbles;
      this.cancelable = !!init.cancelable;
      this.composed = !!init.composed;
      this.defaultPrevented = false;
      this.timeStamp = G.performance && G.performance.now ? G.performance.now() : 0;
      this.target = null;
      this.currentTarget = null;
    }
    preventDefault() {
      this.defaultPrevented = true;
    }
    stopPropagation() {
    }
    stopImmediatePropagation() {
    }
    composedPath() {
      return [];
    }
  };
  Event.NONE = 0;
  Event.CAPTURING_PHASE = 1;
  Event.AT_TARGET = 2;
  Event.BUBBLING_PHASE = 3;
  var CustomEvent = class extends Event {
    constructor(type, init) {
      super(type, init);
      this.detail = (init && init.detail) != null ? init.detail : null;
    }
  };
  var UIEvent = class extends Event {
    constructor(type, init) {
      super(type, init);
      this.detail = init && init.detail || 0;
      this.view = init && init.view || null;
    }
  };
  var MouseEvent = class extends UIEvent {
  };
  var KeyboardEvent = class extends UIEvent {
  };
  var FocusEvent = class extends UIEvent {
  };
  var PointerEvent = class extends MouseEvent {
  };
  var WheelEvent = class extends MouseEvent {
  };
  var ErrorEvent = class extends Event {
  };
  var MessageEvent = class extends Event {
    constructor(type, init) {
      super(type, init);
      this.data = (init && init.data) != null ? init.data : null;
    }
  };
  var DOMRect = class {
    constructor(x, y, w, h) {
      this.x = +x || 0;
      this.y = +y || 0;
      this.width = +w || 0;
      this.height = +h || 0;
    }
    get top() {
      return this.y;
    }
    get left() {
      return this.x;
    }
    get right() {
      return this.x + this.width;
    }
    get bottom() {
      return this.y + this.height;
    }
    toJSON() {
      return { x: this.x, y: this.y, width: this.width, height: this.height, top: this.top, right: this.right, bottom: this.bottom, left: this.left };
    }
  };
  var DOMRectReadOnly = class extends DOMRect {
  };
  var DOMMatrix = class _DOMMatrix {
    constructor() {
      this.a = 1;
      this.b = 0;
      this.c = 0;
      this.d = 1;
      this.e = 0;
      this.f = 0;
      this.is2D = true;
      this.isIdentity = true;
    }
    multiply() {
      return new _DOMMatrix();
    }
    translate() {
      return new _DOMMatrix();
    }
    scale() {
      return new _DOMMatrix();
    }
    inverse() {
      return new _DOMMatrix();
    }
  };
  var DOMPoint = class {
    constructor(x, y, z, w) {
      this.x = +x || 0;
      this.y = +y || 0;
      this.z = +z || 0;
      this.w = w == null ? 1 : +w;
    }
  };
  var TextMetrics = class {
    constructor(text) {
      const w = text.length * 7;
      this.width = w;
      this.actualBoundingBoxLeft = 0;
      this.actualBoundingBoxRight = w;
      this.actualBoundingBoxAscent = 8;
      this.actualBoundingBoxDescent = 2;
      this.fontBoundingBoxAscent = 9;
      this.fontBoundingBoxDescent = 2;
      this.emHeightAscent = 9;
      this.emHeightDescent = 2;
      this.hangingBaseline = 7;
      this.alphabeticBaseline = 0;
      this.ideographicBaseline = -2;
    }
  };
  var ImageData = class {
    constructor(w, h) {
      this.width = w | 0;
      this.height = h | 0;
      this.data = new Uint8ClampedArray(this.width * this.height * 4);
      this.colorSpace = "srgb";
    }
  };
  var Blob = class _Blob {
    constructor(parts, opts) {
      this._parts = parts || [];
      this.type = opts && opts.type || "";
      this.size = 0;
    }
    slice() {
      return new _Blob([], { type: this.type });
    }
    text() {
      return Promise.resolve("");
    }
    arrayBuffer() {
      return Promise.resolve(new ArrayBuffer(0));
    }
  };
  var File = class extends Blob {
    constructor(parts, name, opts) {
      super(parts, opts);
      this.name = String(name);
      this.lastModified = 0;
    }
  };
  var Headers = class {
    constructor(init) {
      this._m = /* @__PURE__ */ Object.create(null);
      if (init) {
        if (typeof init.forEach === "function" && !Array.isArray(init)) init.forEach((v, k) => this.append(k, v));
        else if (Array.isArray(init)) init.forEach((p) => this.append(p[0], p[1]));
        else for (const k in init) this.append(k, init[k]);
      }
    }
    append(k, v) {
      k = String(k).toLowerCase();
      this._m[k] = this._m[k] ? this._m[k] + ", " + v : String(v);
    }
    set(k, v) {
      this._m[String(k).toLowerCase()] = String(v);
    }
    get(k) {
      const v = this._m[String(k).toLowerCase()];
      return v == null ? null : v;
    }
    has(k) {
      return String(k).toLowerCase() in this._m;
    }
    delete(k) {
      delete this._m[String(k).toLowerCase()];
    }
    forEach(cb, thisArg) {
      for (const k in this._m) cb.call(thisArg, this._m[k], k, this);
    }
    keys() {
      return Object.keys(this._m)[Symbol.iterator]();
    }
  };
  var Request = class _Request {
    constructor(input, init) {
      init = init || {};
      this.url = typeof input === "object" && input ? input.url : String(input);
      this.method = (init.method || "GET").toUpperCase();
      this.headers = new Headers(init.headers);
      this.credentials = init.credentials || "same-origin";
      this.mode = init.mode || "cors";
      this.cache = init.cache || "default";
      this.redirect = init.redirect || "follow";
      this.referrer = init.referrer || "about:client";
    }
    clone() {
      return new _Request(this.url, this);
    }
  };
  var Response = class _Response {
    constructor(body, init) {
      init = init || {};
      this._body = body;
      this.status = init.status == null ? 200 : init.status;
      this.ok = this.status >= 200 && this.status < 300;
      this.statusText = init.statusText || "";
      this.headers = new Headers(init.headers);
      this.type = "basic";
      this.url = "";
      this.redirected = false;
      this.bodyUsed = false;
    }
    clone() {
      return new _Response(this._body, this);
    }
    text() {
      return Promise.resolve(String(this._body == null ? "" : this._body));
    }
    json() {
      return Promise.resolve(JSON.parse(this._body));
    }
    arrayBuffer() {
      return Promise.resolve(new ArrayBuffer(0));
    }
    blob() {
      return Promise.resolve(new Blob([this._body]));
    }
  };
  var AbortController = class {
    constructor() {
      this.signal = new AbortSignal();
    }
    abort(reason) {
      this.signal._aborted = true;
      this.signal.reason = reason;
      this.signal.dispatchEvent(new Event("abort"));
    }
  };
  var AbortSignal = class extends EventTarget {
    get aborted() {
      return !!this._aborted;
    }
    throwIfAborted() {
      if (this._aborted) throw new Event("AbortError");
    }
  };
  var URLSearchParams = class {
    constructor(init) {
      this._p = [];
      if (typeof init === "string") {
        init.replace(/^\?/, "").split("&").forEach((kv) => {
          if (!kv) return;
          const i = kv.indexOf("=");
          this._p.push(i < 0 ? [kv, ""] : [decodeURIComponent(kv.slice(0, i)), decodeURIComponent(kv.slice(i + 1))]);
        });
      } else if (init) for (const k in init) this._p.push([k, String(init[k])]);
    }
    get(k) {
      const e = this._p.find((p) => p[0] === k);
      return e ? e[1] : null;
    }
    getAll(k) {
      return this._p.filter((p) => p[0] === k).map((p) => p[1]);
    }
    has(k) {
      return this._p.some((p) => p[0] === k);
    }
    set(k, v) {
      this.delete(k);
      this._p.push([k, String(v)]);
    }
    append(k, v) {
      this._p.push([k, String(v)]);
    }
    delete(k) {
      this._p = this._p.filter((p) => p[0] !== k);
    }
    toString() {
      return this._p.map((p) => encodeURIComponent(p[0]) + "=" + encodeURIComponent(p[1])).join("&");
    }
    forEach(cb) {
      this._p.forEach((p) => cb(p[1], p[0], this));
    }
  };
  var DocumentFragment = class extends Node {
    constructor() {
      super();
    }
  };
  var CharacterData = class extends Node {
  };
  var Text = class extends CharacterData {
    get nodeType() {
      return 3;
    }
    get nodeName() {
      return "#text";
    }
  };
  var Comment = class extends CharacterData {
    get nodeType() {
      return 8;
    }
    get nodeName() {
      return "#comment";
    }
  };
  var DOMTokenList = class {
  };
  var NamedNodeMap = class {
  };
  var NodeList = class {
  };
  var HTMLCollection = class {
  };
  var Document = class extends Node {
    constructor() {
      super();
    }
    get nodeType() {
      return 9;
    }
    get nodeName() {
      return "#document";
    }
    createElement(tag) {
      return createElement(tag);
    }
    createElementNS(ns, tag) {
      return createElement(tag, ns);
    }
    createDocumentFragment() {
      return createDocumentFragment();
    }
    createTextNode(data) {
      const t = Object.create(Text.prototype);
      t._nodeValue = String(data);
      t._childNodes = [];
      t._ownerDocument = this;
      return t;
    }
    createComment(data) {
      const c = Object.create(Comment.prototype);
      c._nodeValue = String(data);
      c._childNodes = [];
      c._ownerDocument = this;
      return c;
    }
    createEvent(_t) {
      const e = Object.create(Event.prototype);
      e.type = "";
      e.initEvent = function(type) {
        e.type = String(type);
      };
      return e;
    }
    getElementById(_id) {
      return null;
    }
    getElementsByTagName(_t) {
      return makeNodeList([]);
    }
    getElementsByClassName(_c) {
      return makeNodeList([]);
    }
    getElementsByName(_n) {
      return makeNodeList([]);
    }
    querySelector(_s) {
      return null;
    }
    querySelectorAll(_s) {
      return makeNodeList([]);
    }
    addEventListener() {
    }
    removeEventListener() {
    }
    dispatchEvent() {
      return true;
    }
    get documentElement() {
      return this._documentElement;
    }
    get head() {
      return this._head;
    }
    get body() {
      return this._body;
    }
    // document.fonts is a lazily created [SameObject] value.
    get fonts() {
      return this._fonts || (this._fonts = new FontFaceSet());
    }
    get defaultView() {
      return G.window || G;
    }
    get location() {
      return G.location;
    }
    get cookie() {
      return this._cookie || "";
    }
    set cookie(v) {
      this._cookie = String(v);
    }
    get readyState() {
      return "complete";
    }
    get visibilityState() {
      return "visible";
    }
    get hidden() {
      return false;
    }
    get title() {
      return this._title || "";
    }
    set title(v) {
      this._title = String(v);
    }
    get referrer() {
      return "";
    }
    get URL() {
      return G.location ? G.location.href : "https://www.youtube.com/";
    }
    get documentURI() {
      return this.URL;
    }
    get characterSet() {
      return "UTF-8";
    }
    get charset() {
      return "UTF-8";
    }
    get compatMode() {
      return "CSS1Compat";
    }
    get contentType() {
      return "text/html";
    }
    get currentScript() {
      return null;
    }
    get activeElement() {
      return this._body;
    }
    hasFocus() {
      return true;
    }
    elementFromPoint() {
      return null;
    }
  };
  var HTMLDocument = class extends Document {
  };
  var Window = class extends EventTarget {
  };
  var Navigator = class {
  };
  var NavigatorUAData = class {
  };
  var Screen = class {
  };
  var Location = class {
  };
  var History = class {
    get length() {
      return 1;
    }
    get state() {
      return null;
    }
    back() {
    }
    forward() {
    }
    go() {
    }
    pushState() {
    }
    replaceState() {
    }
  };
  var Performance = class extends EventTarget {
  };
  var PictureInPictureWindow = class extends EventTarget {
    get width() {
      return 0;
    }
    get height() {
      return 0;
    }
    get onresize() {
      return null;
    }
    set onresize(_v) {
    }
  };
  var Storage = class {
    get length() {
      return 0;
    }
    getItem() {
      return null;
    }
    setItem() {
    }
    removeItem() {
    }
    clear() {
    }
    key() {
      return null;
    }
  };
  var Crypto = class {
  };
  var SubtleCrypto = class {
  };
  var CryptoKey = class {
  };
  var Plugin = class {
  };
  var PluginArray = class {
    get length() {
      return 0;
    }
    item() {
      return null;
    }
    namedItem() {
      return null;
    }
  };
  var MimeType = class {
  };
  var MimeTypeArray = class {
    get length() {
      return 0;
    }
    item() {
      return null;
    }
    namedItem() {
      return null;
    }
  };
  var CSSStyleDeclaration = class {
  };
  var ELEMENT_INTERFACES = {
    "html": HTMLHtmlElement,
    "head": HTMLHeadElement,
    "body": HTMLBodyElement,
    "div": HTMLDivElement,
    "span": HTMLSpanElement,
    "p": HTMLParagraphElement,
    "a": HTMLAnchorElement,
    "img": HTMLImageElement,
    "script": HTMLScriptElement,
    "link": HTMLLinkElement,
    "style": HTMLStyleElement,
    "meta": HTMLMetaElement,
    "iframe": HTMLIFrameElement,
    "input": HTMLInputElement,
    "button": HTMLButtonElement,
    "form": HTMLFormElement,
    "select": HTMLSelectElement,
    "option": HTMLOptionElement,
    "textarea": HTMLTextAreaElement,
    "table": HTMLTableElement,
    "ul": HTMLUListElement,
    "li": HTMLLIElement,
    "label": HTMLLabelElement,
    "picture": HTMLPictureElement,
    "source": HTMLSourceElement,
    "template": HTMLTemplateElement,
    "canvas": HTMLCanvasElement,
    "video": HTMLVideoElement,
    "audio": HTMLAudioElement,
    // The rest of the standard battery, all real subclasses.
    "br": HTMLBRElement,
    "hr": HTMLHRElement,
    "pre": HTMLPreElement,
    "q": HTMLQuoteElement,
    "blockquote": HTMLQuoteElement,
    "dl": HTMLDListElement,
    "ol": HTMLOListElement,
    "fieldset": HTMLFieldSetElement,
    "legend": HTMLLegendElement,
    "data": HTMLDataElement,
    "datalist": HTMLDataListElement,
    "details": HTMLDetailsElement,
    "dialog": HTMLDialogElement,
    "embed": HTMLEmbedElement,
    "object": HTMLObjectElement,
    "map": HTMLMapElement,
    "area": HTMLAreaElement,
    "base": HTMLBaseElement,
    "title": HTMLTitleElement,
    "menu": HTMLMenuElement,
    "meter": HTMLMeterElement,
    "progress": HTMLProgressElement,
    "output": HTMLOutputElement,
    "ins": HTMLModElement,
    "del": HTMLModElement,
    "optgroup": HTMLOptGroupElement,
    "slot": HTMLSlotElement,
    "time": HTMLTimeElement,
    "track": HTMLTrackElement,
    "caption": HTMLTableCaptionElement,
    "td": HTMLTableCellElement,
    "th": HTMLTableCellElement,
    "col": HTMLTableColElement,
    "colgroup": HTMLTableColElement,
    "tr": HTMLTableRowElement,
    "thead": HTMLTableSectionElement,
    "tbody": HTMLTableSectionElement,
    "tfoot": HTMLTableSectionElement,
    "marquee": HTMLMarqueeElement,
    "font": HTMLFontElement,
    "param": HTMLParamElement,
    "frame": HTMLFrameElement,
    "frameset": HTMLFrameSetElement,
    "dir": HTMLDirectoryElement,
    "h1": HTMLHeadingElement,
    "h2": HTMLHeadingElement,
    "h3": HTMLHeadingElement,
    "h4": HTMLHeadingElement,
    "h5": HTMLHeadingElement,
    "h6": HTMLHeadingElement
  };
  var currentDocument = null;
  function initElement(el, tag, ns) {
    const local = String(tag == null ? "" : tag).toLowerCase();
    el._localName = local;
    el._tagName = ns && ns.indexOf("svg") >= 0 ? local : local.toUpperCase();
    el._namespaceURI = ns || "http://www.w3.org/1999/xhtml";
    el._attributes = /* @__PURE__ */ Object.create(null);
    el._childNodes = [];
    el._listeners = /* @__PURE__ */ Object.create(null);
    el._ownerDocument = currentDocument;
    return el;
  }
  function createElement(tag, ns) {
    let Ctor, t = String(tag == null ? "" : tag).toLowerCase();
    if (ns && ns.indexOf("svg") >= 0) Ctor = SVG_INTERFACES[t] || SVGElement;
    else Ctor = ELEMENT_INTERFACES[t] || HTMLUnknownElement;
    const el = Object.create(Ctor.prototype);
    initElement(el, t, ns);
    if (Ctor === HTMLCanvasElement) {
      el._width = 300;
      el._height = 150;
    }
    return el;
  }
  function createDocumentFragment() {
    const f = Object.create(DocumentFragment.prototype);
    f._childNodes = [];
    f._ownerDocument = currentDocument;
    return f;
  }
  function makeStyle() {
    const store = /* @__PURE__ */ Object.create(null);
    return {
      setProperty(k, v) {
        store[k] = String(v);
      },
      getPropertyValue(k) {
        return store[k] == null ? "" : store[k];
      },
      removeProperty(k) {
        const v = store[k];
        delete store[k];
        return v == null ? "" : v;
      },
      get cssText() {
        return Object.keys(store).map((k) => k + ": " + store[k]).join("; ");
      },
      set cssText(_v) {
      },
      item() {
        return "";
      },
      get length() {
        return Object.keys(store).length;
      }
    };
  }
  function makeClassList(el) {
    const set = () => new Set((el._attributes.class || "").split(/\s+/).filter(Boolean));
    return {
      add(...c) {
        const s = set();
        c.forEach((x) => s.add(x));
        el._attributes.class = [...s].join(" ");
      },
      remove(...c) {
        const s = set();
        c.forEach((x) => s.delete(x));
        el._attributes.class = [...s].join(" ");
      },
      toggle(c) {
        const s = set();
        if (s.has(c)) {
          s.delete(c);
        } else {
          s.add(c);
        }
        el._attributes.class = [...s].join(" ");
        return s.has(c);
      },
      contains(c) {
        return set().has(c);
      },
      replace(a, b) {
        const s = set();
        if (s.has(a)) {
          s.delete(a);
          s.add(b);
          el._attributes.class = [...s].join(" ");
          return true;
        }
        return false;
      },
      get length() {
        return set().size;
      },
      item(i) {
        return [...set()][i] || null;
      },
      toString() {
        return el._attributes.class || "";
      }
    };
  }
  function makeAttrList(el) {
    return { get length() {
      return Object.keys(el._attributes).length;
    }, getNamedItem(n) {
      return el.hasAttribute(n) ? { name: n, value: el.getAttribute(n) } : null;
    }, item(i) {
      const k = Object.keys(el._attributes)[i];
      return k == null ? null : { name: k, value: el._attributes[k] };
    } };
  }
  function makeNodeList(arr) {
    const l = arr.slice();
    l.item = (i) => l[i] || null;
    return l;
  }
  function makeGradient() {
    return { addColorStop() {
    } };
  }
  function getCanvasContext(canvas, type, _attrs) {
    type = type.toLowerCase();
    if (type === "2d") {
      if (!canvas._ctx2d) {
        canvas._ctx2d = Object.create(CanvasRenderingContext2D.prototype);
        canvas._ctx2d.canvas = canvas;
        canvas._ctx2d.fillStyle = "#000000";
        canvas._ctx2d.strokeStyle = "#000000";
        canvas._ctx2d.font = "10px sans-serif";
        canvas._ctx2d.globalAlpha = 1;
        canvas._ctx2d.lineWidth = 1;
        canvas._ctx2d.textBaseline = "alphabetic";
        canvas._ctx2d.textAlign = "start";
      }
      return canvas._ctx2d;
    }
    if (type === "webgl" || type === "experimental-webgl") {
      if (!canvas._ctxGL) {
        canvas._ctxGL = Object.create(WebGLRenderingContext.prototype);
        canvas._ctxGL.canvas = canvas;
        canvas._ctxGL.drawingBufferWidth = canvas.width;
        canvas._ctxGL.drawingBufferHeight = canvas.height;
      }
      return canvas._ctxGL;
    }
    if (type === "webgl2") {
      if (!canvas._ctxGL2) {
        canvas._ctxGL2 = Object.create(WebGL2RenderingContext.prototype);
        canvas._ctxGL2.canvas = canvas;
        canvas._ctxGL2.drawingBufferWidth = canvas.width;
        canvas._ctxGL2.drawingBufferHeight = canvas.height;
      }
      return canvas._ctxGL2;
    }
    return null;
  }
  var PNG_1x1 = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==";
  function canvasToDataURL(_canvas, type) {
    return type && String(type).indexOf("jpeg") >= 0 ? PNG_1x1.replace("image/png", "image/jpeg") : PNG_1x1;
  }
  var WEBGL_PARAMS = {
    7936: "WebKit",
    // VENDOR
    7937: "WebKit WebGL",
    // RENDERER
    7938: "WebGL 1.0 (OpenGL ES 2.0 Chromium)",
    // VERSION
    35724: "WebGL GLSL ES 1.0 (OpenGL ES GLSL ES 1.0 Chromium)",
    // SHADING_LANGUAGE_VERSION
    37445: "Google Inc. (NVIDIA)",
    // UNMASKED_VENDOR_WEBGL
    37446: "ANGLE (NVIDIA, NVIDIA GeForce GTX 1060 Direct3D11 vs_5_0 ps_5_0, D3D11)",
    // UNMASKED_RENDERER_WEBGL
    3379: 16384,
    // MAX_TEXTURE_SIZE
    34076: 16384,
    // MAX_CUBE_MAP_TEXTURE_SIZE
    34024: 16384,
    // MAX_RENDERBUFFER_SIZE
    34921: 16,
    // MAX_VERTEX_ATTRIBS
    36347: 1024,
    // MAX_FRAGMENT_UNIFORM_VECTORS
    36348: 30,
    // MAX_VARYING_VECTORS
    36346: 4096,
    // MAX_VERTEX_UNIFORM_VECTORS
    34930: 16,
    // MAX_TEXTURE_IMAGE_UNITS
    35661: 32,
    // MAX_COMBINED_TEXTURE_IMAGE_UNITS
    35660: 16,
    // MAX_VERTEX_TEXTURE_IMAGE_UNITS
    3386: [32767, 32767],
    // MAX_VIEWPORT_DIMS
    33902: [1, 1]
    // ALIASED_POINT_SIZE_RANGE
  };
  var WEBGL_EXTENSIONS = [
    "ANGLE_instanced_arrays",
    "EXT_blend_minmax",
    "EXT_color_buffer_half_float",
    "EXT_float_blend",
    "EXT_frag_depth",
    "EXT_shader_texture_lod",
    "EXT_texture_filter_anisotropic",
    "OES_element_index_uint",
    "OES_standard_derivatives",
    "OES_texture_float",
    "OES_texture_float_linear",
    "OES_texture_half_float",
    "OES_texture_half_float_linear",
    "OES_vertex_array_object",
    "WEBGL_color_buffer_float",
    "WEBGL_compressed_texture_s3tc",
    "WEBGL_debug_renderer_info",
    "WEBGL_debug_shaders",
    "WEBGL_depth_texture",
    "WEBGL_lose_context",
    "WEBGL_multi_draw"
  ];
  var INTERFACES = {
    EventTarget,
    Node,
    Element,
    HTMLElement,
    HTMLUnknownElement,
    HTMLHtmlElement,
    HTMLHeadElement,
    HTMLBodyElement,
    HTMLDivElement,
    HTMLSpanElement,
    HTMLParagraphElement,
    HTMLAnchorElement,
    HTMLImageElement,
    HTMLScriptElement,
    HTMLLinkElement,
    HTMLStyleElement,
    HTMLMetaElement,
    HTMLIFrameElement,
    HTMLInputElement,
    HTMLButtonElement,
    HTMLFormElement,
    HTMLSelectElement,
    HTMLOptionElement,
    HTMLTextAreaElement,
    HTMLTableElement,
    HTMLUListElement,
    HTMLLIElement,
    HTMLLabelElement,
    HTMLPictureElement,
    HTMLSourceElement,
    HTMLTemplateElement,
    HTMLBRElement,
    HTMLHRElement,
    HTMLPreElement,
    HTMLQuoteElement,
    HTMLDListElement,
    HTMLOListElement,
    HTMLFieldSetElement,
    HTMLLegendElement,
    HTMLDataElement,
    HTMLDataListElement,
    HTMLDetailsElement,
    HTMLDialogElement,
    HTMLEmbedElement,
    HTMLObjectElement,
    HTMLMapElement,
    HTMLAreaElement,
    HTMLBaseElement,
    HTMLTitleElement,
    HTMLMenuElement,
    HTMLMeterElement,
    HTMLProgressElement,
    HTMLOutputElement,
    HTMLModElement,
    HTMLHeadingElement,
    HTMLOptGroupElement,
    HTMLSlotElement,
    HTMLTimeElement,
    HTMLTrackElement,
    HTMLTableCaptionElement,
    HTMLTableCellElement,
    HTMLTableColElement,
    HTMLTableRowElement,
    HTMLTableSectionElement,
    HTMLMarqueeElement,
    HTMLFontElement,
    HTMLParamElement,
    HTMLFrameElement,
    HTMLFrameSetElement,
    HTMLDirectoryElement,
    HTMLCanvasElement,
    HTMLMediaElement,
    HTMLVideoElement,
    HTMLAudioElement,
    CanvasRenderingContext2D,
    WebGLRenderingContext,
    WebGL2RenderingContext,
    SVGElement,
    SVGGraphicsElement,
    SVGSVGElement,
    SVGGElement,
    SVGPathElement,
    SVGRectElement,
    SVGTextElement,
    SVGImageElement,
    SVGUseElement,
    SVGViewElement,
    SVGGeometryElement,
    SVGCircleElement,
    SVGEllipseElement,
    SVGLineElement,
    SVGPolygonElement,
    SVGPolylineElement,
    SVGDefsElement,
    SVGGradientElement,
    SVGLinearGradientElement,
    SVGRadialGradientElement,
    SVGStopElement,
    SVGSymbolElement,
    SVGMarkerElement,
    SVGPatternElement,
    SVGMaskElement,
    SVGFilterElement,
    SVGClipPathElement,
    SVGTextContentElement,
    SVGTextPositioningElement,
    SVGTSpanElement,
    SVGTextPathElement,
    SVGAElement,
    SVGStyleElement,
    SVGTitleElement,
    SVGDescElement,
    SVGMetadataElement,
    SVGSwitchElement,
    SVGForeignObjectElement,
    SVGScriptElement,
    SVGAnimationElement,
    SVGSetElement,
    SVGAnimateElement,
    SVGAnimateMotionElement,
    SVGAnimateTransformElement,
    Crypto,
    SubtleCrypto,
    CryptoKey,
    VideoTrack,
    AudioTrack,
    TextTrack,
    VideoTrackList,
    AudioTrackList,
    TextTrackList,
    MediaError,
    TimeRanges,
    MediaStream,
    FontFaceSet,
    Event,
    CustomEvent,
    UIEvent,
    MouseEvent,
    KeyboardEvent,
    FocusEvent,
    PointerEvent,
    WheelEvent,
    ErrorEvent,
    MessageEvent,
    DOMRect,
    DOMRectReadOnly,
    DOMMatrix,
    DOMPoint,
    TextMetrics,
    ImageData,
    Blob,
    File,
    Headers,
    Request,
    Response,
    AbortController,
    AbortSignal,
    URLSearchParams,
    Document,
    HTMLDocument,
    DocumentFragment,
    CharacterData,
    Text,
    Comment,
    DOMTokenList,
    NamedNodeMap,
    NodeList,
    HTMLCollection,
    Window,
    Navigator,
    NavigatorUAData,
    Screen,
    Location,
    History,
    Performance,
    PictureInPictureWindow,
    Storage,
    Plugin,
    PluginArray,
    MimeType,
    MimeTypeArray,
    CSSStyleDeclaration
  };
  function installInterfaces(target) {
    for (const name in INTERFACES) {
      const Ctor = INTERFACES[name];
      markClassNative(Ctor);
      try {
        Object.defineProperty(target, name, { value: Ctor, configurable: true, writable: true });
      } catch (_) {
      }
    }
  }
  function createDocument() {
    const doc = Object.create(HTMLDocument.prototype);
    doc._childNodes = [];
    doc._listeners = /* @__PURE__ */ Object.create(null);
    currentDocument = doc;
    doc._ownerDocument = null;
    doc._documentElement = createElement("html");
    doc._head = createElement("head");
    doc._body = createElement("body");
    doc._documentElement.appendChild(doc._head);
    doc._documentElement.appendChild(doc._body);
    doc.appendChild(doc._documentElement);
    return doc;
  }
  function installWindow(target) {
    try {
      const origProto = Object.getPrototypeOf(target);
      Object.setPrototypeOf(target, Window.prototype);
      if (typeof target.Object !== "function" || typeof target.hasOwnProperty !== "function" || !origProto.isPrototypeOf(target)) {
        Object.setPrototypeOf(target, origProto);
      }
    } catch (_) {
    }
  }
  function asNative(fn, name) {
    return markNative(fn, name);
  }
  function installDateTimezone(offsetMinutes) {
    const off = Number(offsetMinutes) || 0;
    const DP = Date.prototype;
    const getTime = DP.getTime;
    const OFFSET_MS = off * 6e4;
    const shifted = (d) => new Date(getTime.call(d) + OFFSET_MS);
    const set = (name, fn) => {
      markNative(fn, name);
      try {
        Object.defineProperty(DP, name, { value: fn, configurable: true, writable: true });
      } catch (_) {
        DP[name] = fn;
      }
    };
    set("getTimezoneOffset", function getTimezoneOffset() {
      return -off;
    });
    set("getFullYear", function getFullYear() {
      return shifted(this).getUTCFullYear();
    });
    set("getMonth", function getMonth() {
      return shifted(this).getUTCMonth();
    });
    set("getDate", function getDate() {
      return shifted(this).getUTCDate();
    });
    set("getDay", function getDay() {
      return shifted(this).getUTCDay();
    });
    set("getHours", function getHours() {
      return shifted(this).getUTCHours();
    });
    set("getMinutes", function getMinutes() {
      return shifted(this).getUTCMinutes();
    });
    set("getSeconds", function getSeconds() {
      return shifted(this).getUTCSeconds();
    });
    set("getMilliseconds", function getMilliseconds() {
      return shifted(this).getUTCMilliseconds();
    });
  }
  var EVENT_BATTERY = "AnimationEvent AnimationPlaybackEvent BeforeInstallPromptEvent BeforeUnloadEvent BlobEvent ClipboardEvent CloseEvent CompositionEvent ContentVisibilityAutoStateChangeEvent DeviceMotionEvent DeviceOrientationEvent DragEvent FontFaceSetLoadEvent FormDataEvent GamepadEvent HashChangeEvent IDBVersionChangeEvent InputEvent MediaEncryptedEvent MediaQueryListEvent MediaStreamTrackEvent OfflineAudioCompletionEvent PageTransitionEvent PaymentRequestUpdateEvent PopStateEvent ProgressEvent PromiseRejectionEvent RTCDataChannelEvent RTCPeerConnectionIceEvent SecurityPolicyViolationEvent StorageEvent SubmitEvent ToggleEvent TouchEvent TrackEvent TransitionEvent WebGLContextEvent SpeechSynthesisEvent WindowControlsOverlayGeometryChangeEvent XRVisibilityMaskChangeEvent SpeechRecognitionEvent SpeechRecognitionErrorEvent USBConnectionEvent HIDConnectionEvent HIDInputReportEvent NavigateEvent NavigationCurrentEntryChangeEvent".split(" ");
  var PRESENCE_BATTERY = (
    // IndexedDB
    "IDBFactory IDBDatabase IDBObjectStore IDBIndex IDBCursor IDBCursorWithValue IDBKeyRange IDBRequest IDBOpenDBRequest IDBTransaction MediaRecorder MediaSource SourceBuffer SourceBufferList MediaStreamTrack CanvasCaptureMediaStreamTrack MediaDevices MediaDeviceInfo MediaCapabilities MediaKeys MediaKeySession MediaKeySystemAccess MediaKeyStatusMap RemotePlayback AudioContext BaseAudioContext OfflineAudioContext AudioNode AudioParam AudioBuffer AudioBufferSourceNode AudioDestinationNode AudioListener AnalyserNode GainNode BiquadFilterNode OscillatorNode DynamicsCompressorNode ConvolverNode DelayNode PannerNode StereoPannerNode WaveShaperNode ChannelMergerNode ChannelSplitterNode ConstantSourceNode IIRFilterNode PeriodicWave AudioWorklet AudioWorkletNode ScriptProcessorNode CSSStyleSheet StyleSheet StyleSheetList MediaList CSSRule CSSRuleList CSSStyleRule CSSMediaRule CSSImportRule CSSKeyframeRule CSSKeyframesRule CSSFontFaceRule CSSSupportsRule CSSNamespaceRule CSSPageRule StylePropertyMap StylePropertyMapReadOnly CSSStyleValue CSSUnitValue CSSKeywordValue CSSMathValue CSSNumericValue CSSTransformValue CSSTransformComponent CSSPerspective CSSImageValue CSSUnparsedValue FontFace WebGLBuffer WebGLProgram WebGLShader WebGLTexture WebGLFramebuffer WebGLRenderbuffer WebGLUniformLocation WebGLActiveInfo WebGLShaderPrecisionFormat WebGLVertexArrayObject WebGLQuery WebGLSampler WebGLSync WebGLTransformFeedback Worker SharedWorker MessageChannel MessagePort BroadcastChannel Worklet MutationObserver MutationRecord ResizeObserver ResizeObserverEntry ResizeObserverSize IntersectionObserver IntersectionObserverEntry PerformanceObserver PerformanceObserverEntryList ReportingObserver ReadableStream WritableStream TransformStream ReadableStreamDefaultReader ReadableStreamBYOBReader ReadableStreamDefaultController WritableStreamDefaultWriter ByteLengthQueuingStrategy CountQueuingStrategy SubtleCrypto CryptoKey Crypto FileReader FileList FormData WebSocket EventSource XMLHttpRequest XMLHttpRequestUpload XMLHttpRequestEventTarget TextEncoderStream TextDecoderStream CompressionStream DecompressionStream DOMException DOMImplementation DOMParser XMLSerializer DOMStringList DOMStringMap DOMTokenList Attr CharacterData CDATASection ProcessingInstruction DocumentType Range StaticRange Selection NodeIterator TreeWalker ShadowRoot CustomElementRegistry XPathEvaluator XPathExpression XPathResult Performance PerformanceEntry PerformanceMark PerformanceMeasure PerformanceNavigationTiming PerformanceResourceTiming PerformancePaintTiming PerformanceServerTiming PerformanceEventTiming PerformanceLongTaskTiming PerformanceTiming PerformanceNavigation Animation AnimationEffect KeyframeEffect AnimationTimeline DocumentTimeline Notification Permissions Clipboard ClipboardItem Geolocation GeolocationPosition GeolocationCoordinates GeolocationPositionError Gamepad GamepadButton VisualViewport BarProp External Touch TouchList ImageBitmap ImageBitmapRenderingContext Path2D OffscreenCanvas OffscreenCanvasRenderingContext2D IdleDeadline Image Audio Option Observable PressureRecord XRInputSource IdentityCredentialError Keyboard UserActivation Ink InkPresenter CrashReportContext WindowControlsOverlay RTCPeerConnection RTCDataChannel RTCSessionDescription RTCIceCandidate RTCRtpSender RTCRtpReceiver RTCRtpTransceiver SVGAngle SVGLength SVGLengthList SVGNumber SVGNumberList SVGPoint SVGPointList SVGRect SVGMatrix SVGTransform SVGTransformList SVGPreserveAspectRatio SVGStringList SVGUnitTypes SVGAnimatedAngle SVGAnimatedBoolean SVGAnimatedEnumeration SVGAnimatedInteger SVGAnimatedLength SVGAnimatedLengthList SVGAnimatedNumber SVGAnimatedNumberList SVGAnimatedPreserveAspectRatio SVGAnimatedRect SVGAnimatedString SVGAnimatedTransformList SVGComponentTransferFunctionElement SVGFEBlendElement SVGFEColorMatrixElement SVGFEComponentTransferElement SVGFECompositeElement SVGFEConvolveMatrixElement SVGFEDiffuseLightingElement SVGFEDisplacementMapElement SVGFEDistantLightElement SVGFEDropShadowElement SVGFEFloodElement SVGFEFuncAElement SVGFEFuncBElement SVGFEFuncGElement SVGFEFuncRElement SVGFEGaussianBlurElement SVGFEImageElement SVGFEMergeElement SVGFEMergeNodeElement SVGFEMorphologyElement SVGFEOffsetElement SVGFEPointLightElement SVGFESpecularLightingElement SVGFESpotLightElement SVGFETileElement SVGFETurbulenceElement FileSystemHandle FileSystemFileHandle FileSystemDirectoryHandle FileSystemWritableFileStream DataTransfer DataTransferItem DataTransferItemList PointerEvent ScreenOrientation MediaQueryList Highlight HighlightRegistry GPU GPUAdapter GPUCanvasContext GPUDeviceLostInfo USBDevice BluetoothRemoteGATTServer CredentialsContainer Credential StorageManager LockManager Lock StorageBucketManager StorageBucket Scheduling MediaSession MediaMetadata Presentation WakeLock GamepadHapticActuator SpeechGrammar SpeechSynthesisVoice SpeechSynthesis NavigationDestination NavigationTransition NavigationActivation NavigationPrecommitController NavigationPreloadManager TrustedTypePolicyFactory TrustedTypePolicy TrustedHTML TrustedScript TrustedScriptURL IdentityProvider FetchLaterResult GPUQueue".split(" ")
  );
  function rename(fn, name) {
    try {
      Object.defineProperty(fn, "name", { value: name, configurable: true });
    } catch (_) {
    }
    return fn;
  }
  var EVENTTARGET_CONSTRUCTIBLE_BATTERY = "SpeechRecognition SpeechSynthesisUtterance CloseWatcher".split(" ");
  var PLAIN_CONSTRUCTIBLE_BATTERY = "SpeechGrammarList SpeechRecognitionPhrase PressureObserver".split(" ");
  var WEBKIT_ALIASES = {
    webkitSpeechRecognition: "SpeechRecognition",
    webkitSpeechRecognitionError: "SpeechRecognitionErrorEvent",
    webkitSpeechRecognitionEvent: "SpeechRecognitionEvent",
    webkitSpeechGrammar: "SpeechGrammar",
    webkitSpeechGrammarList: "SpeechGrammarList",
    webkitMediaStream: "MediaStream"
  };
  var EVENTTARGET_PRESENCE_BATTERY = "GPUDevice USB HID HIDDevice Serial SerialPort Bluetooth BluetoothDevice XRSystem XRSession XRLayer PresentationRequest PresentationConnection WakeLockSentinel DevicePosture VirtualKeyboard NavigatorManagedData ServiceWorker ServiceWorkerContainer ServiceWorkerRegistration PermissionStatus BatteryManager NetworkInformation Navigation NavigationHistoryEntry TextTrackCue".split(" ");
  var DERIVED_PRESENCE_BATTERY = [
    ["CSSGroupingRule", "CSSRule"],
    ["CSSFunctionRule", "CSSGroupingRule"],
    ["CSSPositionValue", "CSSStyleValue"],
    ["SharedStorageModifierMethod", null],
    ["SharedStorageAppendMethod", "SharedStorageModifierMethod"],
    ["SharedStorageSetMethod", "SharedStorageModifierMethod"],
    ["SharedStorageDeleteMethod", "SharedStorageModifierMethod"],
    ["SharedStorageClearMethod", "SharedStorageModifierMethod"],
    // Credential and WebXR subclasses are constructable in the Chrome snapshot.
    ["FederatedCredential", "Credential"],
    ["PasswordCredential", "Credential"],
    ["PublicKeyCredential", "Credential"],
    ["IdentityCredential", "Credential"],
    ["XRWebGLLayer", "XRLayer"],
    // WebXR layer hierarchy.
    ["XRCompositionLayer", "XRLayer"],
    ["XRCylinderLayer", "XRCompositionLayer"],
    ["XRProjectionLayer", "XRCompositionLayer"],
    ["XRQuadLayer", "XRCompositionLayer"],
    ["XREquirectLayer", "XRCompositionLayer"],
    ["XRCubeLayer", "XRCompositionLayer"]
  ];
  function installPlatformBattery(target) {
    const define = (name, C) => {
      try {
        Object.defineProperty(target, name, { value: C, configurable: true, writable: true });
      } catch (_) {
      }
    };
    for (const name of EVENT_BATTERY) {
      if (typeof target[name] !== "undefined") continue;
      const C = class extends Event {
      };
      rename(C, name);
      markNative(C, name);
      define(name, C);
    }
    if (typeof target.SpeechSynthesisErrorEvent === "undefined" && typeof target.SpeechSynthesisEvent === "function") {
      const C = class extends target.SpeechSynthesisEvent {
      };
      rename(C, "SpeechSynthesisErrorEvent");
      markNative(C, "SpeechSynthesisErrorEvent");
      define("SpeechSynthesisErrorEvent", C);
    }
    for (const name of EVENTTARGET_CONSTRUCTIBLE_BATTERY) {
      if (typeof target[name] !== "undefined") continue;
      const C = class extends EventTarget {
      };
      rename(C, name);
      markNative(C, name);
      define(name, C);
    }
    for (const name of PLAIN_CONSTRUCTIBLE_BATTERY) {
      if (typeof target[name] !== "undefined") continue;
      const C = class {
      };
      rename(C, name);
      markNative(C, name);
      define(name, C);
    }
    for (const name of EVENTTARGET_PRESENCE_BATTERY) {
      if (typeof target[name] !== "undefined") continue;
      const C = class extends EventTarget {
        constructor() {
          super();
          illegal();
        }
      };
      rename(C, name);
      markNative(C, name);
      define(name, C);
    }
    for (const name of PRESENCE_BATTERY) {
      if (typeof target[name] !== "undefined") continue;
      let C;
      if (name === "Image") {
        C = function Image(w, h) {
          const el = createElement("img");
          if (w != null) el._width = w | 0;
          if (h != null) el._height = h | 0;
          return el;
        };
        C.prototype = HTMLImageElement.prototype;
      } else if (name === "Audio") {
        C = function Audio() {
          return createElement("audio");
        };
        C.prototype = HTMLAudioElement.prototype;
      } else if (name === "Option") {
        C = function Option() {
          return createElement("option");
        };
        C.prototype = HTMLOptionElement.prototype;
      } else {
        C = function() {
          illegal();
        };
      }
      rename(C, name);
      markNative(C, name);
      define(name, C);
    }
    for (const [name, parent] of DERIVED_PRESENCE_BATTERY) {
      if (typeof target[name] !== "undefined") continue;
      const Base = parent ? target[parent] : null;
      const C = typeof Base === "function" ? class extends Base {
        constructor() {
          super();
          illegal();
        }
      } : function() {
        illegal();
      };
      rename(C, name);
      markNative(C, name);
      define(name, C);
    }
    for (const alias in WEBKIT_ALIASES) {
      const base = target[WEBKIT_ALIASES[alias]];
      if (typeof target[alias] === "undefined" && typeof base !== "undefined") define(alias, base);
    }
  }
  installInterfaces(G);
  installPlatformBattery(G);

  // shim.js
  (function() {
    "use strict";
    const G3 = globalThis;
    const __wx_console = G3.__wx_console;
    const __wx_random_fill = G3.__wx_random_fill;
    const __wx_random_double = G3.__wx_random_double;
    delete G3.__wx_console;
    delete G3.__wx_random_fill;
    delete G3.__wx_random_double;
    try {
      delete G3.InternalError;
    } catch (_) {
    }
    const defFn = (name, fn) => {
      asNative(fn, name);
      Object.defineProperty(G3, name, { value: fn, configurable: true, writable: true });
      return fn;
    };
    const defHidden2 = (name, value) => Object.defineProperty(G3, name, { value, configurable: true, writable: true, enumerable: false });
    const ifaceStore = /* @__PURE__ */ new WeakMap();
    function iface(Ctor, spec) {
      const proto = Ctor.prototype;
      const state = {};
      for (const name of Object.keys(spec)) {
        const v = spec[name];
        if (typeof v === "function") {
          if (!Object.prototype.hasOwnProperty.call(proto, name))
            Object.defineProperty(proto, name, { value: asNative(v, name), writable: true, enumerable: false, configurable: true });
          continue;
        }
        state[name] = v;
        if (!Object.prototype.hasOwnProperty.call(proto, name))
          Object.defineProperty(proto, name, {
            get: markNative(function() {
              const s = ifaceStore.get(this);
              return s ? s[name] : void 0;
            }, "get " + name),
            set: markNative(function(x) {
              const s = ifaceStore.get(this);
              if (s) s[name] = x;
            }, "set " + name),
            enumerable: false,
            configurable: true
          });
      }
      const inst = Object.create(proto);
      ifaceStore.set(inst, state);
      return inst;
    }
    const mklog = (level) => asNative(function() {
      let s = "";
      for (let i = 0; i < arguments.length; i++) {
        if (i) s += " ";
        const a = arguments[i];
        try {
          s += typeof a === "string" ? a : JSON.stringify(a);
        } catch (_) {
          s += String(a);
        }
      }
      __wx_console(level, s);
    }, "");
    G3.console = {
      log: mklog(0),
      info: mklog(1),
      warn: mklog(2),
      error: mklog(3),
      debug: mklog(4),
      trace: mklog(4),
      dir: mklog(0),
      group: mklog(0),
      groupEnd: () => {
      },
      assert: () => {
      }
    };
    Math.random = asNative(function random() {
      return __wx_random_double();
    }, "random");
    const subtleObj = iface(G3.SubtleCrypto, {
      digest: asNative(function digest() {
        return Promise.resolve(new ArrayBuffer(32));
      }, "digest"),
      generateKey: asNative(function generateKey() {
        return Promise.resolve(Object.create(G3.CryptoKey.prototype));
      }, "generateKey"),
      importKey: asNative(function importKey() {
        return Promise.resolve(Object.create(G3.CryptoKey.prototype));
      }, "importKey"),
      exportKey: asNative(function exportKey() {
        return Promise.resolve(new ArrayBuffer(0));
      }, "exportKey"),
      encrypt: asNative(function encrypt() {
        return Promise.resolve(new ArrayBuffer(0));
      }, "encrypt"),
      decrypt: asNative(function decrypt() {
        return Promise.resolve(new ArrayBuffer(0));
      }, "decrypt"),
      sign: asNative(function sign() {
        return Promise.resolve(new ArrayBuffer(0));
      }, "sign"),
      verify: asNative(function verify() {
        return Promise.resolve(true);
      }, "verify")
    });
    const cryptoObj = iface(G3.Crypto, {
      getRandomValues: asNative(function getRandomValues(arr) {
        if (arr == null || arr.buffer === void 0)
          throw new TypeError("getRandomValues expects an integer TypedArray");
        __wx_random_fill(arr);
        return arr;
      }, "getRandomValues"),
      randomUUID: asNative(function randomUUID() {
        const b = new Uint8Array(16);
        __wx_random_fill(b);
        b[6] = b[6] & 15 | 64;
        b[8] = b[8] & 63 | 128;
        const h = [];
        for (let i = 0; i < 256; i++) h.push((i + 256).toString(16).slice(1));
        return h[b[0]] + h[b[1]] + h[b[2]] + h[b[3]] + "-" + h[b[4]] + h[b[5]] + "-" + h[b[6]] + h[b[7]] + "-" + h[b[8]] + h[b[9]] + "-" + h[b[10]] + h[b[11]] + h[b[12]] + h[b[13]] + h[b[14]] + h[b[15]];
      }, "randomUUID"),
      subtle: subtleObj
    });
    try {
      Object.defineProperty(G3, "crypto", { value: cryptoObj, configurable: true, writable: false });
    } catch (_) {
    }
    if (typeof G3.TextEncoder === "undefined") {
      G3.TextEncoder = class TextEncoder {
        get encoding() {
          return "utf-8";
        }
        encode(str) {
          str = String(str === void 0 ? "" : str);
          const out = [];
          for (let i = 0; i < str.length; i++) {
            let c = str.charCodeAt(i);
            if (c >= 55296 && c <= 56319 && i + 1 < str.length) {
              const c2 = str.charCodeAt(i + 1);
              if (c2 >= 56320 && c2 <= 57343) {
                c = 65536 + (c - 55296 << 10) + (c2 - 56320);
                i++;
              }
            }
            if (c < 128) out.push(c);
            else if (c < 2048) out.push(192 | c >> 6, 128 | c & 63);
            else if (c < 65536) out.push(224 | c >> 12, 128 | c >> 6 & 63, 128 | c & 63);
            else out.push(240 | c >> 18, 128 | c >> 12 & 63, 128 | c >> 6 & 63, 128 | c & 63);
          }
          return new Uint8Array(out);
        }
        encodeInto(str, dest) {
          const enc = this.encode(str);
          const n = Math.min(enc.length, dest.length);
          dest.set(enc.subarray(0, n));
          return { read: str.length, written: n };
        }
      };
    }
    if (typeof G3.TextDecoder === "undefined") {
      G3.TextDecoder = class TextDecoder {
        constructor(label) {
          this._enc = (label || "utf-8").toLowerCase();
        }
        get encoding() {
          return "utf-8";
        }
        decode(input) {
          if (input == null) return "";
          const bytes = input instanceof Uint8Array ? input : new Uint8Array(input.buffer || input);
          let out = "";
          let i = 0;
          while (i < bytes.length) {
            let c = bytes[i++];
            if (c < 128) {
            } else if (c < 224) c = (c & 31) << 6 | bytes[i++] & 63;
            else if (c < 240) c = (c & 15) << 12 | (bytes[i++] & 63) << 6 | bytes[i++] & 63;
            else {
              c = (c & 7) << 18 | (bytes[i++] & 63) << 12 | (bytes[i++] & 63) << 6 | bytes[i++] & 63;
            }
            if (c > 65535) {
              c -= 65536;
              out += String.fromCharCode(55296 + (c >> 10), 56320 + (c & 1023));
            } else {
              out += String.fromCharCode(c);
            }
          }
          return out;
        }
      };
    }
    markClassNative(G3.TextEncoder);
    markClassNative(G3.TextDecoder);
    let timers = [];
    let timerSeq = 1;
    let vnow = 0;
    defFn("setTimeout", function setTimeout2(fn, delay) {
      const id = timerSeq++;
      const args = Array.prototype.slice.call(arguments, 2);
      timers.push({ id, at: vnow + (Number(delay) || 0), fn, args });
      return id;
    });
    defFn("setInterval", function setInterval(fn, delay) {
      return G3.setTimeout.apply(void 0, arguments);
    });
    defFn("clearTimeout", function clearTimeout(id) {
      timers = timers.filter((t) => t.id !== id);
    });
    defFn("clearInterval", function clearInterval(id) {
      timers = timers.filter((t) => t.id !== id);
    });
    G3.queueMicrotask = G3.queueMicrotask || asNative(function queueMicrotask(fn) {
      Promise.resolve().then(fn);
    }, "queueMicrotask");
    defHidden2("__wx_runTimers", function __wx_runTimers() {
      if (timers.length === 0) return false;
      let idx = 0;
      for (let i = 1; i < timers.length; i++)
        if (timers[i].at < timers[idx].at) idx = i;
      const t = timers.splice(idx, 1)[0];
      vnow = t.at;
      try {
        t.fn.apply(void 0, t.args);
      } catch (e) {
        console.error("timer threw: " + e);
      }
      return true;
    });
    const DEFAULT_PROFILE = {
      // America/Phoenix matches the shim's fixed UTC-7 Date offset. esbuild injects
      // the Chrome version from chrome_version.json, which profile.go also embeds.
      userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
      platform: "Win32",
      language: "en-US",
      languages: ["en-US", "en"],
      vendor: "Google Inc.",
      timezone: "America/Phoenix",
      utcOffsetMinutes: -420,
      screen: [1920, 1080],
      userAgentData: {
        brands: [
          { brand: "Google Chrome", version: "149" },
          { brand: "Chromium", version: "149" },
          { brand: "Not)A;Brand", version: "24" }
        ],
        fullVersionList: [
          { brand: "Google Chrome", version: "149.0.7827.3" },
          { brand: "Chromium", version: "149.0.7827.3" },
          { brand: "Not)A;Brand", version: "24.0.0.0" }
        ],
        uaFullVersion: "149.0.7827.3",
        mobile: false,
        platform: "Windows"
      }
    };
    defHidden2("__wxDiscovery", true);
    defHidden2("__wxAutoStub", false);
    const seenProbes = /* @__PURE__ */ new Set();
    defHidden2("__wxGetProbes", () => Array.from(seenProbes).sort());
    defHidden2("__wxClearProbes", () => {
      seenProbes.clear();
    });
    const HIDDEN = /* @__PURE__ */ new Set([
      "runBotguard",
      "newMinter",
      "mint",
      "__wx_runTimers",
      "__wxApplyProfile",
      "__wxDiscovery",
      "__wxAutoStub",
      "__wxGetProbes",
      "__wxClearProbes"
    ]);
    const INTERNAL_INTERFACES = /* @__PURE__ */ new Set([
      "VideoTrack",
      "AudioTrack",
      "VideoTrackList",
      "AudioTrackList",
      "InkPresenter",
      // FontFaceSet remains reachable through document.fonts.
      "FontFaceSet"
    ]);
    const fullyHidden = (k) => typeof k === "string" && (HIDDEN.has(k) || INTERNAL_INTERFACES.has(k));
    const internalField = (k) => typeof k === "string" && k.charCodeAt(0) === 95;
    const ALLOW = /* @__PURE__ */ new Set([
      "then",
      "toJSON",
      "constructor",
      "valueOf",
      "toString",
      Symbol.toPrimitive,
      Symbol.iterator,
      Symbol.toStringTag
    ]);
    function logProbe(path) {
      if (path.endsWith(".")) return;
      if (G3.__wxDiscovery && !seenProbes.has(path)) {
        seenProbes.add(path);
        console.warn("API-DRIFT probe: " + path);
      }
    }
    function universalStub(path) {
      const target = function() {
      };
      return new Proxy(target, {
        get(t, prop) {
          if (prop === "then" || prop === Symbol.iterator || prop === Symbol.asyncIterator)
            return void 0;
          if (prop === Symbol.toPrimitive) return () => 0;
          if (prop === Symbol.toStringTag) return void 0;
          if (prop === "toString" || prop === "valueOf")
            return () => prop === "toString" ? "[object Object]" : 0;
          if (prop === "constructor") return target;
          if (typeof prop === "symbol") return void 0;
          logProbe(path + "." + String(prop));
          return universalStub(path + "." + String(prop));
        },
        apply() {
          return universalStub(path + "()");
        },
        construct() {
          return universalStub("new " + path);
        },
        has() {
          return true;
        }
      });
    }
    function discoveryProxy(target, label) {
      return new Proxy(target, {
        get(t, prop, recv) {
          if (fullyHidden(prop)) return void 0;
          if (internalField(prop) && Object.prototype.hasOwnProperty.call(t, prop)) return void 0;
          if (prop in t || typeof prop === "symbol" || ALLOW.has(prop))
            return Reflect.get(t, prop, t);
          logProbe(label + "." + String(prop));
          return G3.__wxAutoStub ? universalStub(label + "." + String(prop)) : void 0;
        },
        has(t, prop) {
          if (fullyHidden(prop)) return false;
          if (internalField(prop) && Object.prototype.hasOwnProperty.call(t, prop)) return false;
          if (typeof prop === "string" && !ALLOW.has(prop) && !Reflect.has(t, prop))
            logProbe(label + "." + String(prop));
          if (G3.__wxAutoStub && typeof prop === "string") return true;
          return Reflect.has(t, prop);
        },
        getOwnPropertyDescriptor(t, prop) {
          if (fullyHidden(prop)) return void 0;
          const d = Reflect.getOwnPropertyDescriptor(t, prop);
          if (d && internalField(prop)) return void 0;
          if (d) return d;
          if (typeof prop === "string" && !ALLOW.has(prop) && !Reflect.has(t, prop)) {
            logProbe(label + "." + String(prop));
            if (G3.__wxAutoStub)
              return {
                value: universalStub(label + "." + String(prop)),
                writable: true,
                enumerable: false,
                configurable: true
              };
          }
          return void 0;
        },
        // All omitted properties are configurable, which satisfies Proxy invariants.
        ownKeys(t) {
          return Reflect.ownKeys(t).filter((k) => !(fullyHidden(k) || internalField(k)));
        },
        set(t, prop, val, recv) {
          return Reflect.set(t, prop, val, recv);
        }
      });
    }
    const def = (name, value) => Object.defineProperty(G3, name, { value, configurable: true, writable: true });
    function makeMinimalIntl(prof) {
      const locale = prof.language || "en-US";
      const TZ = prof.timezone || "UTC";
      const resolved = (extra) => Object.assign({ locale, calendar: "gregory", numberingSystem: "latn" }, extra);
      const fmtDate = (d) => {
        const p = (n) => (n < 10 ? "0" : "") + n;
        return p(d.getMonth() + 1) + "/" + p(d.getDate()) + "/" + d.getFullYear();
      };
      function DateTimeFormat(_locales, options) {
        const opts = options || {};
        const self = this instanceof DateTimeFormat ? this : Object.create(DateTimeFormat.prototype);
        self.resolvedOptions = asNative(function resolvedOptions() {
          return resolved({ timeZone: opts.timeZone || TZ, year: "numeric", month: "2-digit", day: "2-digit" });
        }, "resolvedOptions");
        self.format = asNative(function format(d) {
          return fmtDate(d == null ? /* @__PURE__ */ new Date() : new Date(d));
        }, "format");
        self.formatToParts = asNative(function formatToParts(d) {
          return [{ type: "literal", value: fmtDate(d == null ? /* @__PURE__ */ new Date() : new Date(d)) }];
        }, "formatToParts");
        return self;
      }
      function NumberFormat(_locales, _options) {
        const self = this instanceof NumberFormat ? this : Object.create(NumberFormat.prototype);
        self.resolvedOptions = asNative(function resolvedOptions() {
          return resolved({ style: "decimal", notation: "standard", minimumIntegerDigits: 1, useGrouping: "auto" });
        }, "resolvedOptions");
        self.format = asNative(function format(n) {
          return String(n);
        }, "format");
        self.formatToParts = asNative(function formatToParts(n) {
          return [{ type: "integer", value: String(n) }];
        }, "formatToParts");
        return self;
      }
      function Collator() {
        const self = this instanceof Collator ? this : Object.create(Collator.prototype);
        self.compare = asNative(function compare(a, b) {
          return String(a) < String(b) ? -1 : String(a) > String(b) ? 1 : 0;
        }, "compare");
        self.resolvedOptions = asNative(function resolvedOptions() {
          return resolved({ usage: "sort", sensitivity: "variant" });
        }, "resolvedOptions");
        return self;
      }
      function RelativeTimeFormat() {
        const self = this instanceof RelativeTimeFormat ? this : Object.create(RelativeTimeFormat.prototype);
        self.format = asNative(function format(n, unit) {
          return n + " " + unit;
        }, "format");
        self.resolvedOptions = asNative(function resolvedOptions() {
          return resolved({ style: "long", numeric: "always" });
        }, "resolvedOptions");
        return self;
      }
      function PluralRules() {
        const self = this instanceof PluralRules ? this : Object.create(PluralRules.prototype);
        self.select = asNative(function select(n) {
          return n === 1 ? "one" : "other";
        }, "select");
        self.resolvedOptions = asNative(function resolvedOptions() {
          return resolved({ type: "cardinal" });
        }, "resolvedOptions");
        return self;
      }
      function ListFormat() {
        const self = this instanceof ListFormat ? this : Object.create(ListFormat.prototype);
        self.format = asNative(function format(list) {
          return (list || []).join(", ");
        }, "format");
        return self;
      }
      function Locale(tag) {
        const self = this instanceof Locale ? this : Object.create(Locale.prototype);
        self.baseName = String(tag || locale);
        self.language = self.baseName.split("-")[0];
        self.region = self.baseName.split("-")[1] || "";
        self.toString = asNative(function toString() {
          return self.baseName;
        }, "toString");
        return self;
      }
      const supportedLocalesOf = asNative(function supportedLocalesOf2(l) {
        return Array.isArray(l) ? l.slice() : l ? [l] : [];
      }, "supportedLocalesOf");
      [DateTimeFormat, NumberFormat, Collator, RelativeTimeFormat, PluralRules, ListFormat].forEach((C) => {
        asNative(C, C.name);
        C.supportedLocalesOf = supportedLocalesOf;
      });
      asNative(Locale, "Locale");
      return {
        DateTimeFormat,
        NumberFormat,
        Collator,
        RelativeTimeFormat,
        PluralRules,
        ListFormat,
        Locale,
        getCanonicalLocales: asNative(function getCanonicalLocales(l) {
          return Array.isArray(l) ? l.slice() : [String(l)];
        }, "getCanonicalLocales"),
        supportedValuesOf: asNative(function supportedValuesOf(key) {
          return key === "timeZone" ? [TZ] : key === "calendar" ? ["gregory"] : [];
        }, "supportedValuesOf")
      };
    }
    function installIntl(prof) {
      let hasReal = false;
      try {
        hasReal = typeof Intl !== "undefined" && !!Intl.DateTimeFormat && !!new Intl.DateTimeFormat().resolvedOptions;
      } catch (_) {
        hasReal = false;
      }
      if (hasReal) {
        const orig = Intl.DateTimeFormat;
        const ResolvedTZ = prof.timezone;
        const wrapped = function(...a) {
          const inst = new orig(...a);
          const ro = inst.resolvedOptions.bind(inst);
          inst.resolvedOptions = () => Object.assign(ro(), { timeZone: ResolvedTZ });
          return inst;
        };
        wrapped.prototype = orig.prototype;
        wrapped.supportedLocalesOf = orig.supportedLocalesOf;
        try {
          Intl.DateTimeFormat = wrapped;
        } catch (_) {
        }
        return;
      }
      def("Intl", makeMinimalIntl(prof));
    }
    installWindow(G3);
    (function installPerformance() {
      let baseNow;
      try {
        baseNow = typeof performance !== "undefined" && typeof performance.now === "function" ? performance.now.bind(performance) : null;
      } catch (_) {
        baseNow = null;
      }
      const t0 = Date.now();
      if (!baseNow) baseNow = () => Date.now() - t0;
      const perf = iface(G3.Performance, {
        timeOrigin: t0,
        now: asNative(function now() {
          return baseNow();
        }, "now"),
        mark: asNative(function mark() {
          return null;
        }, "mark"),
        measure: asNative(function measure() {
          return null;
        }, "measure"),
        clearMarks: asNative(function clearMarks() {
        }, "clearMarks"),
        clearMeasures: asNative(function clearMeasures() {
        }, "clearMeasures"),
        getEntries: asNative(function getEntries() {
          return [];
        }, "getEntries"),
        getEntriesByName: asNative(function getEntriesByName() {
          return [];
        }, "getEntriesByName"),
        getEntriesByType: asNative(function getEntriesByType() {
          return [];
        }, "getEntriesByType"),
        toJSON: asNative(function toJSON() {
          return { timeOrigin: this.timeOrigin };
        }, "toJSON")
      });
      def("performance", perf);
    })();
    let currentProfile = null;
    let batteryPromise = null;
    defHidden2("__wxApplyProfile", function __wxApplyProfile(p) {
      const prof = Object.assign({}, DEFAULT_PROFILE, p || {});
      currentProfile = prof;
      const navSpec = {};
      Object.assign(navSpec, {
        userAgent: prof.userAgent,
        appVersion: prof.userAgent.replace(/^Mozilla\//, ""),
        appName: "Netscape",
        appCodeName: "Mozilla",
        platform: prof.platform,
        product: "Gecko",
        productSub: "20030107",
        vendor: prof.vendor,
        vendorSub: "",
        language: prof.language,
        languages: Object.freeze(prof.languages.slice()),
        onLine: true,
        cookieEnabled: true,
        hardwareConcurrency: 8,
        deviceMemory: 8,
        maxTouchPoints: 0,
        webdriver: false,
        doNotTrack: null,
        pdfViewerEnabled: true,
        userAgentData: prof.userAgentData ? iface(G3.NavigatorUAData, {
          brands: prof.userAgentData.brands.map((b) => Object.assign({}, b)),
          mobile: prof.userAgentData.mobile,
          platform: prof.userAgentData.platform,
          getHighEntropyValues: asNative(function getHighEntropyValues(hints) {
            const uad = currentProfile && currentProfile.userAgentData;
            const brand0 = this.brands && this.brands[0];
            const full = {
              brands: this.brands,
              mobile: this.mobile,
              platform: this.platform,
              platformVersion: "10.0.0",
              architecture: "x86",
              bitness: "64",
              model: "",
              uaFullVersion: uad && uad.uaFullVersion || (brand0 && brand0.version || "0") + ".0.0.0",
              fullVersionList: uad && uad.fullVersionList || this.brands
            };
            const out = { brands: this.brands, mobile: this.mobile, platform: this.platform };
            (hints || []).forEach((h) => {
              if (h in full) out[h] = full[h];
            });
            return Promise.resolve(out);
          }, "getHighEntropyValues"),
          toJSON() {
            return { brands: this.brands, mobile: this.mobile, platform: this.platform };
          }
        }) : void 0,
        javaEnabled: asNative(function javaEnabled() {
          return false;
        }, "javaEnabled"),
        // PluginArray/MimeTypeArray expose `length` as a getter-only accessor; the
        // prototype already returns 0, so create-without-assign (assigning length
        // would throw "no setter for property" in strict mode).
        plugins: Object.create(G3.PluginArray.prototype),
        mimeTypes: Object.create(G3.MimeTypeArray.prototype),
        sendBeacon: asNative(function sendBeacon() {
          return true;
        }, "sendBeacon"),
        clearAppBadge: asNative(function clearAppBadge() {
          return Promise.resolve();
        }, "clearAppBadge"),
        // Protected Audience API surface.
        joinAdInterestGroup: asNative(function joinAdInterestGroup(group, durationSeconds) {
          return Promise.resolve();
        }, "joinAdInterestGroup"),
        leaveAdInterestGroup: asNative(function leaveAdInterestGroup(group) {
          return Promise.resolve();
        }, "leaveAdInterestGroup"),
        clearOriginJoinedAdInterestGroups: asNative(function clearOriginJoinedAdInterestGroups(owner, groupsToKeep) {
          return Promise.resolve();
        }, "clearOriginJoinedAdInterestGroups"),
        updateAdInterestGroups: asNative(function updateAdInterestGroups() {
        }, "updateAdInterestGroups"),
        runAdAuction: asNative(function runAdAuction(config) {
          return Promise.resolve(null);
        }, "runAdAuction"),
        createAuctionNonce: asNative(function createAuctionNonce() {
          return Promise.resolve(G3.crypto && G3.crypto.randomUUID ? G3.crypto.randomUUID() : "");
        }, "createAuctionNonce"),
        getInterestGroupAdAuctionData: asNative(function getInterestGroupAdAuctionData(config) {
          return Promise.resolve({ requestId: "", request: new Uint8Array() });
        }, "getInterestGroupAdAuctionData"),
        deprecatedReplaceInURN: asNative(function deprecatedReplaceInURN(urnOrConfig, replacements) {
          return Promise.resolve();
        }, "deprecatedReplaceInURN"),
        deprecatedURNToURL: asNative(function deprecatedURNToURL(urnOrConfig) {
          return Promise.resolve(null);
        }, "deprecatedURNToURL"),
        canLoadAdAuctionFencedFrame: asNative(function canLoadAdAuctionFencedFrame() {
          return false;
        }, "canLoadAdAuctionFencedFrame"),
        // Use interface instances so navigator properties pass instanceof checks.
        mediaDevices: iface(G3.MediaDevices, {
          ondevicechange: null,
          enumerateDevices: asNative(function enumerateDevices() {
            return Promise.resolve([]);
          }, "enumerateDevices"),
          getSupportedConstraints: asNative(function getSupportedConstraints() {
            return { width: true, height: true, aspectRatio: true, frameRate: true, facingMode: true, deviceId: true, groupId: true };
          }, "getSupportedConstraints"),
          getUserMedia: asNative(function getUserMedia() {
            return Promise.reject(Object.assign(new Error("Permission denied"), { name: "NotAllowedError" }));
          }, "getUserMedia"),
          getDisplayMedia: asNative(function getDisplayMedia() {
            return Promise.reject(Object.assign(new Error("Permission denied"), { name: "NotAllowedError" }));
          }, "getDisplayMedia")
        }),
        connection: iface(G3.NetworkInformation, {
          effectiveType: "4g",
          rtt: 50,
          downlink: 10,
          saveData: false,
          onchange: null
        }),
        keyboard: iface(G3.Keyboard, {
          getLayoutMap: asNative(function getLayoutMap() {
            return Promise.resolve(/* @__PURE__ */ new Map());
          }, "getLayoutMap"),
          lock: asNative(function lock() {
            return Promise.resolve();
          }, "lock"),
          unlock: asNative(function unlock() {
          }, "unlock")
        }),
        userActivation: iface(G3.UserActivation, {
          hasBeenActive: false,
          isActive: false
        }),
        windowControlsOverlay: iface(G3.WindowControlsOverlay, {
          visible: false,
          ongeometrychange: null,
          getTitlebarAreaRect: asNative(function getTitlebarAreaRect() {
            return new G3.DOMRect(0, 0, 0, 0);
          }, "getTitlebarAreaRect")
        }),
        // Use Ink and InkPresenter instances to preserve instanceof behavior.
        ink: iface(G3.Ink, {
          requestPresenter: asNative(function requestPresenter(param) {
            return Promise.resolve(iface(G3.InkPresenter, {
              presentationArea: null,
              updateInkTrailStartPoint: asNative(function updateInkTrailStartPoint(event, style) {
              }, "updateInkTrailStartPoint")
            }));
          }, "requestPresenter")
        })
      });
      const rejectNotAllowed = () => Promise.reject(Object.assign(new Error("Permission denied"), { name: "NotAllowedError" }));
      Object.assign(navSpec, {
        gpu: iface(G3.GPU, {
          requestAdapter: asNative(function requestAdapter() {
            return Promise.resolve(null);
          }, "requestAdapter"),
          getPreferredCanvasFormat: asNative(function getPreferredCanvasFormat() {
            return "bgra8unorm";
          }, "getPreferredCanvasFormat")
        }),
        usb: iface(G3.USB, {
          onconnect: null,
          ondisconnect: null,
          getDevices: asNative(function getDevices() {
            return Promise.resolve([]);
          }, "getDevices"),
          requestDevice: asNative(function requestDevice() {
            return rejectNotAllowed();
          }, "requestDevice")
        }),
        hid: iface(G3.HID, {
          onconnect: null,
          ondisconnect: null,
          getDevices: asNative(function getDevices() {
            return Promise.resolve([]);
          }, "getDevices"),
          requestDevice: asNative(function requestDevice() {
            return Promise.resolve([]);
          }, "requestDevice")
        }),
        serial: iface(G3.Serial, {
          onconnect: null,
          ondisconnect: null,
          getPorts: asNative(function getPorts() {
            return Promise.resolve([]);
          }, "getPorts"),
          requestPort: asNative(function requestPort() {
            return rejectNotAllowed();
          }, "requestPort")
        }),
        bluetooth: iface(G3.Bluetooth, {
          onavailabilitychanged: null,
          getAvailability: asNative(function getAvailability() {
            return Promise.resolve(false);
          }, "getAvailability"),
          getDevices: asNative(function getDevices() {
            return Promise.resolve([]);
          }, "getDevices"),
          requestDevice: asNative(function requestDevice() {
            return rejectNotAllowed();
          }, "requestDevice")
        }),
        xr: iface(G3.XRSystem, {
          ondevicechange: null,
          isSessionSupported: asNative(function isSessionSupported() {
            return Promise.resolve(false);
          }, "isSessionSupported"),
          requestSession: asNative(function requestSession() {
            return rejectNotAllowed();
          }, "requestSession")
        }),
        credentials: iface(G3.CredentialsContainer, {
          get: asNative(function get() {
            return Promise.resolve(null);
          }, "get"),
          store: asNative(function store() {
            return Promise.resolve();
          }, "store"),
          create: asNative(function create2() {
            return Promise.resolve(null);
          }, "create"),
          preventSilentAccess: asNative(function preventSilentAccess() {
            return Promise.resolve();
          }, "preventSilentAccess")
        }),
        geolocation: iface(G3.Geolocation, {
          // Chrome reports geolocation errors asynchronously.
          getCurrentPosition: asNative(function getCurrentPosition(_ok, err) {
            if (typeof err === "function") Promise.resolve().then(function() {
              err(iface(G3.GeolocationPositionError, { code: 1, message: "User denied Geolocation" }));
            });
          }, "getCurrentPosition"),
          watchPosition: asNative(function watchPosition() {
            return 0;
          }, "watchPosition"),
          clearWatch: asNative(function clearWatch() {
          }, "clearWatch")
        }),
        permissions: iface(G3.Permissions, {
          query: asNative(function query(desc) {
            return Promise.resolve(iface(G3.PermissionStatus, { name: desc && desc.name || "", state: "prompt", onchange: null }));
          }, "query")
        }),
        serviceWorker: iface(G3.ServiceWorkerContainer, {
          controller: null,
          oncontrollerchange: null,
          onmessage: null,
          onmessageerror: null,
          ready: Promise.resolve(iface(G3.ServiceWorkerRegistration, {
            installing: null,
            waiting: null,
            active: null,
            scope: "https://www.youtube.com/",
            updateViaCache: "imports",
            unregister: asNative(function unregister() {
              return Promise.resolve(true);
            }, "unregister"),
            update: asNative(function update() {
              return Promise.resolve();
            }, "update")
          })),
          register: asNative(function register() {
            return rejectNotAllowed();
          }, "register"),
          getRegistration: asNative(function getRegistration() {
            return Promise.resolve(void 0);
          }, "getRegistration"),
          getRegistrations: asNative(function getRegistrations() {
            return Promise.resolve([]);
          }, "getRegistrations"),
          startMessages: asNative(function startMessages() {
          }, "startMessages")
        }),
        storage: iface(G3.StorageManager, {
          estimate: asNative(function estimate() {
            return Promise.resolve({ quota: 0, usage: 0, usageDetails: {} });
          }, "estimate"),
          persist: asNative(function persist() {
            return Promise.resolve(false);
          }, "persist"),
          persisted: asNative(function persisted() {
            return Promise.resolve(false);
          }, "persisted"),
          getDirectory: asNative(function getDirectory() {
            return rejectNotAllowed();
          }, "getDirectory")
        }),
        locks: iface(G3.LockManager, {
          // Supply a Lock instance because callbacks commonly inspect its name and mode.
          request: asNative(function request(name, opts, cb) {
            const fn = typeof opts === "function" ? opts : cb;
            const lock = iface(G3.Lock, { name: String(name), mode: opts && typeof opts === "object" && opts.mode === "shared" ? "shared" : "exclusive" });
            return Promise.resolve().then(function() {
              return fn ? fn(lock) : void 0;
            });
          }, "request"),
          query: asNative(function query() {
            return Promise.resolve({ held: [], pending: [] });
          }, "query")
        }),
        mediaCapabilities: iface(G3.MediaCapabilities, {
          decodingInfo: asNative(function decodingInfo() {
            return Promise.resolve({ supported: true, smooth: true, powerEfficient: true });
          }, "decodingInfo"),
          encodingInfo: asNative(function encodingInfo() {
            return Promise.resolve({ supported: true, smooth: true, powerEfficient: true });
          }, "encodingInfo")
        }),
        mediaSession: iface(G3.MediaSession, {
          metadata: null,
          playbackState: "none",
          setActionHandler: asNative(function setActionHandler() {
          }, "setActionHandler"),
          setPositionState: asNative(function setPositionState() {
          }, "setPositionState"),
          setMicrophoneActive: asNative(function setMicrophoneActive() {
          }, "setMicrophoneActive"),
          setCameraActive: asNative(function setCameraActive() {
          }, "setCameraActive")
        }),
        presentation: iface(G3.Presentation, { defaultRequest: null, receiver: null }),
        wakeLock: iface(G3.WakeLock, {
          request: asNative(function request() {
            return rejectNotAllowed();
          }, "request")
        }),
        devicePosture: iface(G3.DevicePosture, { type: "continuous", onchange: null }),
        virtualKeyboard: iface(G3.VirtualKeyboard, {
          boundingRect: new G3.DOMRect(0, 0, 0, 0),
          overlaysContent: false,
          ongeometrychange: null,
          show: asNative(function show() {
          }, "show"),
          hide: asNative(function hide() {
          }, "hide")
        }),
        storageBuckets: iface(G3.StorageBucketManager, {
          open: asNative(function open() {
            return rejectNotAllowed();
          }, "open"),
          keys: asNative(function keys() {
            return Promise.resolve([]);
          }, "keys"),
          delete: asNative(function _delete() {
            return Promise.resolve();
          }, "delete")
        }),
        scheduling: iface(G3.Scheduling, {
          isInputPending: asNative(function isInputPending() {
            return false;
          }, "isInputPending")
        }),
        vibrate: asNative(function vibrate() {
          return false;
        }, "vibrate"),
        getBattery: asNative(function getBattery() {
          if (!batteryPromise) batteryPromise = Promise.resolve(iface(G3.BatteryManager, { charging: true, chargingTime: 0, dischargingTime: Infinity, level: 1, onchargingchange: null, onchargingtimechange: null, ondischargingtimechange: null, onlevelchange: null }));
          return batteryPromise;
        }, "getBattery"),
        getGamepads: asNative(function getGamepads() {
          return [null, null, null, null];
        }, "getGamepads"),
        share: asNative(function share() {
          return rejectNotAllowed();
        }, "share"),
        canShare: asNative(function canShare() {
          return false;
        }, "canShare"),
        setAppBadge: asNative(function setAppBadge() {
          return Promise.resolve();
        }, "setAppBadge"),
        requestMIDIAccess: asNative(function requestMIDIAccess() {
          return rejectNotAllowed();
        }, "requestMIDIAccess"),
        requestMediaKeySystemAccess: asNative(function requestMediaKeySystemAccess() {
          return rejectNotAllowed();
        }, "requestMediaKeySystemAccess"),
        getInstalledRelatedApps: asNative(function getInstalledRelatedApps() {
          return Promise.resolve([]);
        }, "getInstalledRelatedApps")
      });
      const navBase = iface(G3.Navigator, navSpec);
      def("navigator", discoveryProxy(navBase, "navigator"));
      const screenBase = iface(G3.Screen, {
        width: prof.screen[0],
        height: prof.screen[1],
        availWidth: prof.screen[0],
        availHeight: prof.screen[1] - 40,
        colorDepth: 24,
        pixelDepth: 24,
        availLeft: 0,
        availTop: 0,
        orientation: { type: "landscape-primary", angle: 0, onchange: null }
      });
      def("screen", screenBase);
      def("innerWidth", prof.screen[0]);
      def("innerHeight", prof.screen[1] - 120);
      def("outerWidth", prof.screen[0]);
      def("outerHeight", prof.screen[1]);
      def("screenX", 0);
      def("screenY", 0);
      def("screenLeft", 0);
      def("screenTop", 0);
      def("devicePixelRatio", 1);
      const loc = iface(G3.Location, {
        href: "https://www.youtube.com/",
        origin: "https://www.youtube.com",
        protocol: "https:",
        host: "www.youtube.com",
        hostname: "www.youtube.com",
        port: "",
        pathname: "/",
        search: "",
        hash: "",
        replace() {
        },
        assign() {
        },
        reload() {
        },
        toString() {
          return this.href;
        }
      });
      def("location", loc);
      def("origin", loc.origin);
      def("trustedTypes", iface(G3.TrustedTypePolicyFactory, {
        emptyHTML: "",
        emptyScript: "",
        defaultPolicy: null,
        createPolicy: asNative(function createPolicy(name) {
          return iface(G3.TrustedTypePolicy, {
            name: String(name == null ? "" : name),
            createHTML: asNative(function createHTML(s) {
              return String(s);
            }, "createHTML"),
            createScript: asNative(function createScript(s) {
              return String(s);
            }, "createScript"),
            createScriptURL: asNative(function createScriptURL(s) {
              return String(s);
            }, "createScriptURL")
          });
        }, "createPolicy"),
        getAttributeType: asNative(function getAttributeType() {
          return null;
        }, "getAttributeType"),
        getPropertyType: asNative(function getPropertyType() {
          return null;
        }, "getPropertyType"),
        isHTML: asNative(function isHTML() {
          return false;
        }, "isHTML"),
        isScript: asNative(function isScript() {
          return false;
        }, "isScript"),
        isScriptURL: asNative(function isScriptURL() {
          return false;
        }, "isScriptURL")
      }));
      def("crashReport", iface(G3.CrashReportContext, {}));
      const doc = createDocument();
      doc._title = "";
      def("document", discoveryProxy(doc, "document"));
      const win = discoveryProxy(G3, "window");
      def("window", win);
      def("self", win);
      def("top", win);
      def("parent", win);
      def("frames", win);
      def("length", 0);
      def("name", "");
      def("closed", false);
      def("frameElement", null);
      def("status", "");
      def("isSecureContext", true);
      def("crossOriginIsolated", false);
      def("history", Object.create(G3.History.prototype));
      def("localStorage", Object.create(G3.Storage.prototype));
      def("sessionStorage", Object.create(G3.Storage.prototype));
      installDateTimezone(prof.utcOffsetMinutes);
      installIntl(prof);
      defFn("requestAnimationFrame", function requestAnimationFrame(cb) {
        return G3.setTimeout(() => cb(G3.performance ? G3.performance.now() : 0), 16);
      });
      defFn("cancelAnimationFrame", function cancelAnimationFrame(id) {
        return G3.clearTimeout(id);
      });
      defFn("requestIdleCallback", function requestIdleCallback(cb) {
        return G3.setTimeout(() => cb({ didTimeout: false, timeRemaining: () => 50 }), 1);
      });
      defFn("cancelIdleCallback", function cancelIdleCallback(id) {
        return G3.clearTimeout(id);
      });
      defFn("matchMedia", function matchMedia(q) {
        return { matches: false, media: String(q), onchange: null, addListener() {
        }, removeListener() {
        }, addEventListener() {
        }, removeEventListener() {
        }, dispatchEvent() {
          return true;
        } };
      });
      defFn("getComputedStyle", function getComputedStyle() {
        return iface(G3.CSSStyleDeclaration, { getPropertyValue: asNative(function getPropertyValue() {
          return "";
        }, "getPropertyValue") });
      });
      defFn("addEventListener", function addEventListener() {
      });
      defFn("removeEventListener", function removeEventListener() {
      });
      defFn("dispatchEvent", function dispatchEvent() {
        return true;
      });
      defFn("postMessage", function postMessage() {
      });
      defFn("focus", function focus() {
      });
      defFn("blur", function blur() {
      });
      defFn("scroll", function scroll() {
      });
      defFn("scrollTo", function scrollTo() {
      });
      defFn("open", function open() {
        return null;
      });
      defFn("close", function close() {
      });
      defFn("alert", function alert() {
      });
      return currentProfile;
    });
    G3.__wxApplyProfile(null);
  })();

  // node_modules/bgutils-js/dist/core/index.js
  var core_exports = {};
  __export(core_exports, {
    BotGuardClient: () => BotGuardClient,
    Challenge: () => challengeFetcher_exports,
    PoToken: () => webPoClient_exports,
    WebPoMinter: () => WebPoMinter
  });

  // node_modules/bgutils-js/dist/core/challengeFetcher.js
  var challengeFetcher_exports = {};
  __export(challengeFetcher_exports, {
    create: () => create,
    descramble: () => descramble,
    parseChallengeData: () => parseChallengeData
  });

  // node_modules/bgutils-js/dist/utils/constants.js
  var GOOG_BASE_URL = "https://jnn-pa.googleapis.com";
  var YT_BASE_URL = "https://www.youtube.com";
  var GOOG_API_KEY = "AIzaSyDyT5W0Jh49F30Pqqtyfdf7pDLFKLJoAnw";
  var USER_AGENT = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36(KHTML, like Gecko)";

  // node_modules/bgutils-js/dist/utils/helpers.js
  var base64urlCharRegex = /[-_.]/g;
  var base64urlToBase64Map = {
    "-": "+",
    _: "/",
    ".": "="
  };
  var DeferredPromise = class {
    constructor() {
      this.promise = new Promise((resolve, reject) => {
        this.resolve = resolve;
        this.reject = reject;
      });
    }
  };
  var BGError = class extends TypeError {
    constructor(code, message, info) {
      super(message);
      this.name = "BGError";
      this.code = code;
      if (info)
        this.info = info;
    }
  };
  function base64ToU8(base64) {
    let base64Mod;
    if (base64urlCharRegex.test(base64)) {
      base64Mod = base64.replace(base64urlCharRegex, function(match) {
        return base64urlToBase64Map[match];
      });
    } else {
      base64Mod = base64;
    }
    base64Mod = atob(base64Mod);
    return new Uint8Array([...base64Mod].map((char) => char.charCodeAt(0)));
  }
  function u8ToBase64(u8, base64url = false) {
    const result = btoa(String.fromCharCode(...u8));
    if (base64url) {
      return result.replace(/\+/g, "-").replace(/\//g, "_");
    }
    return result;
  }
  function isBrowser() {
    const isBrowser2 = typeof window !== "undefined" && typeof window.document !== "undefined" && typeof window.document.createElement !== "undefined" && typeof window.HTMLElement !== "undefined" && typeof window.navigator !== "undefined" && typeof window.getComputedStyle === "function" && typeof window.requestAnimationFrame === "function" && typeof window.matchMedia === "function";
    const hasValidWindow = Object.getOwnPropertyDescriptor(globalThis, "window")?.get?.toString().includes("[native code]") ?? false;
    return isBrowser2 && hasValidWindow;
  }
  function getHeaders() {
    const headers = {
      "content-type": "application/json+protobuf",
      "x-goog-api-key": GOOG_API_KEY,
      "x-user-agent": "grpc-web-javascript/0.1"
    };
    if (!isBrowser()) {
      headers["user-agent"] = USER_AGENT;
    }
    return headers;
  }
  function buildURL(endpointName, useYouTubeAPI) {
    return `${useYouTubeAPI ? YT_BASE_URL : GOOG_BASE_URL}/${useYouTubeAPI ? "api/jnn/v1" : "$rpc/google.internal.waa.v1.Waa"}/${endpointName}`;
  }

  // node_modules/bgutils-js/dist/core/challengeFetcher.js
  async function create(bgConfig, interpreterHash) {
    const requestKey = bgConfig.requestKey;
    if (!bgConfig.fetch)
      throw new BGError("BAD_CONFIG", "No fetch function provided");
    const payload = [requestKey];
    if (interpreterHash)
      payload.push(interpreterHash);
    const response = await bgConfig.fetch(buildURL("Create", bgConfig.useYouTubeAPI), {
      method: "POST",
      headers: getHeaders(),
      body: JSON.stringify(payload)
    });
    if (!response.ok)
      throw new BGError("REQUEST_FAILED", "Failed to fetch challenge", { status: response.status });
    const rawData = await response.json();
    return parseChallengeData(rawData);
  }
  function parseChallengeData(rawData) {
    let challengeData = [];
    if (rawData.length > 1 && typeof rawData[1] === "string") {
      const descrambled = descramble(rawData[1]);
      challengeData = JSON.parse(descrambled || "[]");
    } else if (rawData.length && typeof rawData[0] === "object") {
      challengeData = rawData[0];
    }
    const [messageId, wrappedScript, wrappedUrl, interpreterHash, program, globalName, , clientExperimentsStateBlob] = challengeData;
    const privateDoNotAccessOrElseSafeScriptWrappedValue = Array.isArray(wrappedScript) ? wrappedScript.find((value) => value && typeof value === "string") : null;
    const privateDoNotAccessOrElseTrustedResourceUrlWrappedValue = Array.isArray(wrappedUrl) ? wrappedUrl.find((value) => value && typeof value === "string") : null;
    return {
      messageId,
      interpreterJavascript: {
        privateDoNotAccessOrElseSafeScriptWrappedValue,
        privateDoNotAccessOrElseTrustedResourceUrlWrappedValue
      },
      interpreterHash,
      program,
      globalName,
      clientExperimentsStateBlob
    };
  }
  function descramble(scrambledChallenge) {
    const buffer = base64ToU8(scrambledChallenge);
    if (buffer.length)
      return new TextDecoder().decode(buffer.map((b) => b + 97));
  }

  // node_modules/bgutils-js/dist/core/webPoClient.js
  var webPoClient_exports = {};
  __export(webPoClient_exports, {
    decodeColdStartToken: () => decodeColdStartToken,
    generate: () => generate,
    generateColdStartToken: () => generateColdStartToken,
    generatePlaceholder: () => generatePlaceholder
  });

  // node_modules/bgutils-js/dist/core/botGuardClient.js
  var BotGuardClient = class _BotGuardClient {
    constructor(options) {
      this.deferredVmFunctions = new DeferredPromise();
      this.defaultTimeout = 3e3;
      this.userInteractionElement = options.userInteractionElement;
      this.vm = options.globalObj[options.globalName];
      this.program = options.program;
    }
    /**
     * Factory method to create and load a BotGuardClient instance.
     * @param options - Configuration options for the BotGuardClient.
     * @returns A promise that resolves to a loaded BotGuardClient instance.
     */
    static async create(options) {
      return await new _BotGuardClient(options).load();
    }
    async load() {
      if (!this.vm)
        throw new BGError("VM_INIT", "VM not found");
      if (!this.vm.a)
        throw new BGError("VM_INIT", "VM init function not found");
      const vmFunctionsCallback = (asyncSnapshotFunction, shutdownFunction, passEventFunction, checkCameraFunction) => {
        this.deferredVmFunctions.resolve({
          asyncSnapshotFunction,
          shutdownFunction,
          passEventFunction,
          checkCameraFunction
        });
      };
      try {
        this.syncSnapshotFunction = await this.vm.a(this.program, vmFunctionsCallback, true, this.userInteractionElement, () => {
        }, [[], []])[0];
      } catch (error) {
        throw new BGError("VM_ERROR", "Could not load program", { error });
      }
      return this;
    }
    /**
     * Takes a snapshot asynchronously.
     * @returns The snapshot result.
     * @example
     * ```ts
     * const result = await botguard.snapshot({
     *   contentBinding: {
     *     c: "a=6&a2=10&b=SZWDwKVIuixOp7Y4euGTgwckbJA&c=1729143849&d=1&t=7200&c1a=1&c6a=1&c6b=1&hh=HrMb5mRWTyxGJphDr0nW2Oxonh0_wl2BDqWuLHyeKLo",
     *     e: "ENGAGEMENT_TYPE_VIDEO_LIKE",
     *     encryptedVideoId: "P-vC09ZJcnM"
     *    }
     * });
     *
     * console.log(result);
     * ```
     */
    async snapshot(args, timeout = 3e3) {
      return await Promise.race([
        new Promise(async (resolve, reject) => {
          const vmFunctions = await this.deferredVmFunctions.promise;
          if (!vmFunctions.asyncSnapshotFunction)
            return reject(new BGError("ASYNC_SNAPSHOT", "Asynchronous snapshot function not found"));
          await vmFunctions.asyncSnapshotFunction((response) => resolve(response), [
            args.contentBinding,
            args.signedTimestamp,
            args.webPoSignalOutput,
            args.skipPrivacyBuffer
          ]);
        }),
        new Promise((_, reject) => setTimeout(() => reject(new BGError("TIMEOUT", "VM operation timed out")), timeout))
      ]);
    }
    /**
     * Passes an event to the VM.
     * @throws Error Throws an error if the pass event function is not found.
     */
    async passEvent(args, timeout = this.defaultTimeout) {
      return await Promise.race([
        (async () => {
          const vmFunctions = await this.deferredVmFunctions.promise;
          if (!vmFunctions.passEventFunction)
            throw new BGError("PASS_EVENT", "Pass event function not found");
          vmFunctions.passEventFunction(args);
        })(),
        new Promise((_, reject) => setTimeout(() => reject(new BGError("TIMEOUT", "VM operation timed out")), timeout))
      ]);
    }
    /**
     * Checks the "camera".
     * @throws Error Throws an error if the check camera function is not found.
     */
    async checkCamera(args, timeout = this.defaultTimeout) {
      return await Promise.race([
        (async () => {
          const vmFunctions = await this.deferredVmFunctions.promise;
          if (!vmFunctions.checkCameraFunction)
            throw new BGError("CHECK_CAMERA", "Check camera function not found");
          vmFunctions.checkCameraFunction(args);
        })(),
        new Promise((_, reject) => setTimeout(() => reject(new BGError("TIMEOUT", "VM operation timed out")), timeout))
      ]);
    }
    /**
     * Shuts down the VM. Taking a snapshot after this will throw an error.
     * @throws Error Throws an error if the shutdown function is not found.
     */
    async shutdown(timeout = this.defaultTimeout) {
      return await Promise.race([
        (async () => {
          const vmFunctions = await this.deferredVmFunctions.promise;
          if (!vmFunctions.shutdownFunction)
            throw new BGError("SHUTDOWN", "Shutdown function not found");
          vmFunctions.shutdownFunction();
        })(),
        new Promise((_, reject) => setTimeout(() => reject(new BGError("TIMEOUT", "VM operation timed out")), timeout))
      ]);
    }
    /**
     * Takes a snapshot synchronously.
     * @returns The snapshot result.
     * @throws Error Throws an error if the synchronous snapshot function is not found.
     */
    async snapshotSynchronous(args) {
      if (!this.syncSnapshotFunction)
        throw new BGError("SYNC_SNAPSHOT", "Synchronous snapshot function not found");
      return this.syncSnapshotFunction([
        args.contentBinding,
        args.signedTimestamp,
        args.webPoSignalOutput,
        args.skipPrivacyBuffer
      ]);
    }
  };

  // node_modules/bgutils-js/dist/core/webPoMinter.js
  var WebPoMinter = class _WebPoMinter {
    constructor(mintCallback) {
      this.mintCallback = mintCallback;
    }
    static async create(integrityTokenResponse, webPoSignalOutput2) {
      const getMinter = webPoSignalOutput2[0];
      if (!getMinter)
        throw new BGError("VM_ERROR", "PMD:Undefined");
      if (!integrityTokenResponse.integrityToken)
        throw new BGError("INTEGRITY_ERROR", "No integrity token provided", { integrityTokenResponse });
      const mintCallback = await getMinter(base64ToU8(integrityTokenResponse.integrityToken));
      if (!(mintCallback instanceof Function))
        throw new BGError("VM_ERROR", "APF:Failed");
      return new _WebPoMinter(mintCallback);
    }
    async mintAsWebsafeString(identifier) {
      const result = await this.mint(identifier);
      return u8ToBase64(result, true);
    }
    async mint(identifier) {
      const result = await this.mintCallback(new TextEncoder().encode(identifier));
      if (!result)
        throw new BGError("VM_ERROR", "YNJ:Undefined");
      if (!(result instanceof Uint8Array))
        throw new BGError("VM_ERROR", "ODM:Invalid");
      return result;
    }
  };

  // node_modules/bgutils-js/dist/core/webPoClient.js
  async function generate(args) {
    const { program, bgConfig, globalName } = args;
    const { identifier } = bgConfig;
    const botguard = await BotGuardClient.create({ program, globalName, globalObj: bgConfig.globalObj });
    const webPoSignalOutput2 = [];
    const botguardResponse = await botguard.snapshot({ webPoSignalOutput: webPoSignalOutput2 });
    const payload = [bgConfig.requestKey, botguardResponse];
    const integrityTokenResponse = await bgConfig.fetch(buildURL("GenerateIT", bgConfig.useYouTubeAPI), {
      method: "POST",
      headers: getHeaders(),
      body: JSON.stringify(payload)
    });
    const integrityTokenJson = await integrityTokenResponse.json();
    const [integrityToken, estimatedTtlSecs, mintRefreshThreshold, websafeFallbackToken] = integrityTokenJson;
    const integrityTokenData = {
      integrityToken,
      estimatedTtlSecs,
      mintRefreshThreshold,
      websafeFallbackToken
    };
    const webPoMinter = await WebPoMinter.create(integrityTokenData, webPoSignalOutput2);
    const poToken = await webPoMinter.mintAsWebsafeString(identifier);
    return { poToken, integrityTokenData };
  }
  function generateColdStartToken(identifier, clientState) {
    const encodedIdentifier = new TextEncoder().encode(identifier);
    if (encodedIdentifier.length > 118)
      throw new BGError("BAD_INPUT", "Content binding is too long.", { identifierLength: encodedIdentifier.length });
    const timestamp = Math.floor(Date.now() / 1e3);
    const randomKeys = [Math.floor(Math.random() * 256), Math.floor(Math.random() * 256)];
    const header = randomKeys.concat([
      0,
      clientState ?? 1
    ], [
      timestamp >> 24 & 255,
      timestamp >> 16 & 255,
      timestamp >> 8 & 255,
      timestamp & 255
    ]);
    const packet = new Uint8Array(2 + header.length + encodedIdentifier.length);
    packet[0] = 34;
    packet[1] = header.length + encodedIdentifier.length;
    packet.set(header, 2);
    packet.set(encodedIdentifier, 2 + header.length);
    const payload = packet.subarray(2);
    const keyLength = randomKeys.length;
    for (let i = keyLength; i < payload.length; i++) {
      payload[i] ^= payload[i % keyLength];
    }
    return u8ToBase64(packet, true);
  }
  function generatePlaceholder(identifier, clientState) {
    return generateColdStartToken(identifier, clientState);
  }
  function decodeColdStartToken(token) {
    const packet = base64ToU8(token);
    const payloadLength = packet[1];
    const totalPacketLength = 2 + payloadLength;
    if (packet.length !== totalPacketLength)
      throw new BGError("BAD_INPUT", "Invalid packet length.", { packetLength: packet.length, expectedLength: totalPacketLength });
    const payload = packet.subarray(2);
    const keyLength = 2;
    for (let i = keyLength; i < payload.length; ++i) {
      payload[i] ^= payload[i % keyLength];
    }
    const keys = [payload[0], payload[1]];
    const unknownVal = payload[2];
    const clientState = payload[3];
    const timestamp = payload[4] << 24 | payload[5] << 16 | payload[6] << 8 | payload[7];
    const date = new Date(timestamp * 1e3);
    const identifier = new TextDecoder().decode(payload.subarray(8));
    return {
      identifier,
      timestamp,
      unknownVal,
      clientState,
      keys,
      date
    };
  }

  // entrypoint.js
  var G2 = globalThis;
  var defHidden = (name, value) => Object.defineProperty(G2, name, { value, configurable: true, writable: true, enumerable: false });
  var webPoSignalOutput;
  var minter;
  defHidden("runBotguard", async (interpreterJavascript, program, globalName, profile) => {
    if (profile) G2.__wxApplyProfile(profile);
    new Function(interpreterJavascript)();
    const botguard = await core_exports.BotGuardClient.create({
      program,
      globalName,
      globalObj: G2
    });
    webPoSignalOutput = [];
    const botguardResponse = await botguard.snapshot({ webPoSignalOutput });
    return botguardResponse;
  });
  defHidden("newMinter", async (integrityToken) => {
    minter = await core_exports.WebPoMinter.create({ integrityToken }, webPoSignalOutput);
    return true;
  });
  defHidden("mint", async (identifier) => {
    return await minter.mintAsWebsafeString(identifier);
  });
})();
