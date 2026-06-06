// Global-surface enumerator shared by two callers:
//
//   - internal/shimaudit/surface.go embeds and evaluates this expression in
//     QuickJS to read the shim's browser-visible surface.
//   - build/js/capture-globals.mjs injects it at document-start to read Chrome's
//     surface.
//
// It is a single expression so both callers can use it without wrapping or
// modifying the source.
//
// Descriptors establish presence and descriptor kind before any value is read,
// so throwing getters are still recorded. Value reads then add type,
// constructability, ancestry, interface, parent, and alias details when
// available. The constructability probe checks [[Construct]] without invoking
// the candidate function.
(function () {
  function isConstructable(v) {
    // v is used only as newTarget, so its body is never invoked.
    if (typeof v !== 'function') return false;
    try { Reflect.construct(function () {}, [], v); return true; } catch (_) { return false; }
  }
  function isEventClass(v) {
    try {
      if (typeof Event === 'undefined') return false;
      if (v === Event) return true;
      return v != null && v.prototype != null && v.prototype instanceof Event;
    } catch (_) { return false; }
  }
  function ctorName(v) {
    try { return typeof v.name === 'string' ? v.name : ''; } catch (_) { return ''; }
  }
  function parentName(v) {
    // Return the parent interface name, excluding Function.prototype.
    try {
      var p = Object.getPrototypeOf(v);
      if (typeof p === 'function' && p !== Function.prototype) return ctorName(p);
    } catch (_) {}
    return '';
  }
  function enumerate(obj) {
    var out = {};
    if (obj == null) return out;
    // First name an object/function value was seen under, for alias identity.
    var firstName = new Map();
    var seen = Object.create(null); // nearest-wins across the prototype chain
    var o = obj, depth = 0;
    while (o != null && depth < 64) {
      var own = (o === obj);
      var descs;
      try { descs = Object.getOwnPropertyDescriptors(o); } catch (_) { descs = {}; }
      var names = Object.getOwnPropertyNames(descs);
      for (var i = 0; i < names.length; i++) {
        var name = names[i];
        // Skip '__proto__': `out[name] = shape` would invoke the prototype setter
        // rather than record a member, and it carries no surface signal anyway.
        if (name === '' || name === '__proto__' || seen[name]) continue;
        seen[name] = true;
        var d = descs[name];
        var accessor = !!(d && ('get' in d || 'set' in d));
        var shape = { descriptor: accessor ? 'accessor' : 'data' };
        if (own) shape.own = true;
        try {
        // Invoke inherited getters with the enumerated object as the receiver.
        // Native getters often reject the prototype object with "Illegal
        // invocation"; a getter may still throw for its own reasons.
          var v = (d && 'value' in d) ? d.value
            : (d && d.get) ? d.get.call(obj)
            : undefined; // set-only accessor: no readable value
          shape.access = 'ok';
          shape.typeof = typeof v;
          if (typeof v === 'function') {
            if (isConstructable(v)) shape.constructable = true;
            if (isEventClass(v)) shape.event = true;
            var iname = ctorName(v); if (iname) shape.interface = iname;
            var pname = parentName(v); if (pname) shape.parent = pname;
          }
          if (v !== null && (typeof v === 'object' || typeof v === 'function')) {
            if (firstName.has(v)) { var f = firstName.get(v); if (f !== name) shape.alias = f; }
            else firstName.set(v, name);
          }
        } catch (_) {
          shape.access = 'threw';
        }
        out[name] = shape;
      }
      try { o = Object.getPrototypeOf(o); } catch (_) { o = null; }
      depth++;
    }
    return out;
  }
  var win = (typeof window !== 'undefined') ? window
    : (typeof globalThis !== 'undefined') ? globalThis : this;
  var nav = (typeof navigator !== 'undefined') ? navigator : null;
  var doc = (typeof document !== 'undefined') ? document : null;
  return { window: enumerate(win), navigator: enumerate(nav), document: enumerate(doc) };
})()
