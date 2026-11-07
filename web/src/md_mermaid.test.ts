// Unit tests for the SVG sanitizer in md_mermaid.ts (#6642) — the policy
// itself, element by element and attribute by attribute, exercised on a fake
// DOM so the hostile cases run where there is no browser — plus the two
// render-path helpers that are pure enough to drive directly: the chunk
// poll and the timed-out ledger.
import assert from "node:assert/strict";
import { test } from "node:test";
import { pollUntil, scrub, TimeoutLedger } from "./md_mermaid.js";

const SVG_NS = "http://www.w3.org/2000/svg";

interface Attr { name: string; localName: string; value: string }

// The narrow surface scrub() touches: children, localName, namespaceURI,
// attributes (name/localName/value), removeAttribute, remove, textContent.
class FakeEl {
  children: FakeEl[] = [];
  removed = false;
  parent: FakeEl | null = null;
  constructor(
    public localName: string,
    public attrs: Attr[] = [],
    kids: FakeEl[] = [],
    public text = "",
    public namespaceURI: string | null = SVG_NS,
  ) {
    for (const k of kids) k.parent = this;
    this.children = kids;
  }
  get attributes(): Attr[] { return this.attrs; }
  get textContent(): string {
    return this.text + this.children.map((c) => c.textContent).join("");
  }
  removeAttribute(name: string): void {
    this.attrs = this.attrs.filter((a) => a.name !== name);
  }
  remove(): void {
    this.removed = true;
    if (this.parent) this.parent.children = this.parent.children.filter((c) => c !== this);
  }
}

const attr = (name: string, value: string, localName = name.split(":").pop() as string): Attr =>
  ({ name, localName, value });
const el = (name: string, attrs: Attr[] = [], kids: FakeEl[] = [], text = "", ns: string | null = SVG_NS): FakeEl =>
  new FakeEl(name, attrs, kids, text, ns);
const scrubbed = (root: FakeEl): FakeEl => {
  scrub(root as unknown as Element);
  return root;
};
const attrNames = (e: FakeEl): string[] => e.attrs.map((a) => a.name);
const kidNames = (e: FakeEl): string[] => e.children.map((c) => c.localName);

test("scrub keeps the SVG vocabulary a diagram is made of", () => {
  const root = scrubbed(el("svg", [], [
    el("g", [], [el("rect"), el("path"), el("text", [], [el("tspan")])]),
    el("defs", [], [el("marker"), el("lineargradient", [], [el("stop")])]),
  ]));
  assert.deepEqual(kidNames(root), ["g", "defs"]);
  assert.deepEqual(kidNames(root.children[0]), ["rect", "path", "text"]);
});

test("scrub drops every non-SVG-vocabulary element with its subtree", () => {
  for (const name of ["a", "script", "foreignObject", "iframe", "animate", "animateTransform", "set", "video", "audio", "canvas", "form"]) {
    const root = scrubbed(el("svg", [], [el(name, [], [el("rect")]), el("g")]));
    assert.deepEqual(kidNames(root), ["g"], `<${name}> must be dropped`);
  }
});

test("scrub drops a keep-listed name in a foreign namespace (A1)", () => {
  const root = scrubbed(el("svg", [], [
    el("g", [], [], "", "http://www.w3.org/1999/xhtml"),
    el("text", [], [], "", "http://www.w3.org/1998/Math/MathML"),
    el("g"),
  ]));
  assert.deepEqual(kidNames(root), ["g"]);
});

test("scrub drops on* and srcdoc attributes, including namespaced lookalikes (A1)", () => {
  const root = scrubbed(el("g", [
    attr("onclick", "x()"),
    attr("evil:onload", "x()", "onload"),
    attr("srcdoc", "<script>x</script>"),
    attr("fill", "red"),
  ]));
  assert.deepEqual(attrNames(root), ["fill"]);
});

test("scrub href keeps #fragments, drops everything else, by local name", () => {
  const use = scrubbed(el("use", [
    attr("href", "#arrow"),
    attr("xlink:href", "https://mmd.invalid/x.svg", "href"),
    attr("evil:href", "javascript:x()", "href"),
  ]));
  assert.deepEqual(attrNames(use), ["href"]);
});

test("scrub href trims only URL whitespace, so unicode-space tricks fail (N5)", () => {
  // A URL parser strips C0 controls and space, not unicode spaces: "%A0#x"
  // is not a fragment ref, and "%A0javascript:" is not a scheme — but both
  // are a fetch the diagram chose, so both go.
  const use = scrubbed(el("use", [
    attr("href", "\u00a0#x"),
    attr("xlink:href", "\u00a0javascript:x()", "href"),
  ]));
  assert.deepEqual(attrNames(use), []);
});

test("scrub href keeps raster data:image only on <image> (N5)", () => {
  const img = scrubbed(el("image", [attr("href", "data:image/png;base64,AAAA")]));
  const svgImg = scrubbed(el("image", [attr("href", "data:image/svg+xml;base64,AAAA")]));
  const use = scrubbed(el("use", [attr("href", "data:image/png;base64,AAAA")]));
  assert.deepEqual(attrNames(img), ["href"]);
  assert.deepEqual(attrNames(svgImg), [], "svg payloads are not rasters");
  assert.deepEqual(attrNames(use), []);
});

test("scrub drops attributes carrying URLs, escapes or scriptable schemes (B2)", () => {
  const cases: [string, string, string][] = [
    ["url external", "fill", "url(https://mmd.invalid/x.png)"],
    ["url same-origin", "fill", "url(/af-mmd-probe/x.png)"],
    ["url relative", "fill", "url(assets/x.png)"],
    ["url unicode-space padded", "fill", "url(\u00a0#x)"],
    ["image-set", "style", "background: image-set('/af-mmd-probe/x.png' 1x)"],
    ["-webkit-image-set", "style", "background: -webkit-image-set(url('/x.png') 1x)"],
    ["image()", "style", "background: image('/af-mmd-probe/x.png')"],
    ["element()", "style", "background: -moz-element(#other)"],
    ["src()", "style", "background: src('/af-mmd-probe/x.png')"],
    ["cross-fade()", "style", "background: cross-fade(url(#x), url(/af-mmd-probe/x.png), 50%)"],
    ["backslash escape", "style", "background: u\\72l('/af-mmd-probe/x.png')"],
    ["comment obfuscation", "style", "background: u/**/rl('/x.png')"],
    ["expression", "style", "width: expression(x())"],
    ["javascript:", "style", "background: url(javascript:x())"],
    ["behavior", "style", "behavior: url(x.htc)"],
    ["-moz-binding", "style", "-moz-binding: url(x.xml#b)"],
  ];
  for (const [label, name, value] of cases) {
    const e = scrubbed(el("rect", [attr(name, value)]));
    assert.deepEqual(attrNames(e), [], `${label} must be dropped`);
  }
});

test("scrub drops url() arguments a character-class scan would skip (B1)", () => {
  // JS `.` excludes U+2028/U+2029 but CSS treats them as ordinary URL
  // characters — a regex scan steps over the occurrence and the value
  // survives whole. The string-scan must find them, quoted and not.
  const cases: [string, string][] = [
    ["unquoted U+2028", "background:url(\u2028/af-mmd-probe/x.png)"],
    ["unquoted U+2029", "background:url(\u2029/af-mmd-probe/x.png)"],
    ["quoted U+2028", "background:url('\u2028/af-mmd-probe/x.png')"],
    ["quoted U+2029", 'background:url("\u2029/af-mmd-probe/x.png")'],
    ["padded U+2028", "background:url( \u2028/af-mmd-probe/x.png)"],
    ["U+2028 before a fragment — still not #", "background:url(\u2028#x)"],
  ];
  for (const [label, value] of cases) {
    const e = scrubbed(el("rect", [attr("style", value)]));
    assert.deepEqual(attrNames(e), [], `${label} must be dropped`);
    const root = scrubbed(el("svg", [], [el("style", [], [], `.a{${value}}`), el("g")]));
    assert.deepEqual(kidNames(root), ["g"], `${label} must drop the whole block`);
  }
});

test("scrub keeps <switch> with its sanitized children and drops its conditional gates", () => {
  // Journey labels ride <switch><foreignObject/><text/></switch>: keep the
  // switch so the SVG fallback survives, drop the foreignObject branch, and
  // strip the conditional-processing attributes wherever they appear so no
  // child can be gated out of rendering.
  const root = scrubbed(el("svg", [], [
    el("switch", [attr("requiredExtensions", "http://x.invalid/ext")], [
      el("foreignObject", [attr("requiredFeatures", "http://www.w3.org/TR/SVG11/feature#Extensibility")], [
        el("div", [], [], "task", "http://www.w3.org/1999/xhtml"),
      ]),
      el("text", [attr("systemLanguage", "en")], [], "task name"),
    ]),
  ]));
  assert.deepEqual(kidNames(root), ["switch"]);
  const sw = root.children[0];
  assert.deepEqual(kidNames(sw), ["text"], "foreignObject goes, the SVG fallback stays");
  assert.deepEqual(attrNames(sw), [], "requiredExtensions is stripped");
  assert.deepEqual(attrNames(sw.children[0]), [], "systemLanguage is stripped");
});

test("scrub keeps url(#fragment) in presentation attributes", () => {
  const e = scrubbed(el("path", [
    attr("fill", "url(#grad)"),
    attr("marker-end", 'url("#arrow")'),
    attr("filter", "url( #f )"),
  ]));
  assert.deepEqual(attrNames(e), ["fill", "marker-end", "filter"]);
});

test("scrub keeps a benign <style> block and Mermaid's own @keyframes", () => {
  const style = el("style", [], [], "#x .node{fill:#eee}@keyframes dash{to{stroke-dashoffset:0}}");
  const root = scrubbed(el("svg", [], [style, el("g")]));
  assert.deepEqual(kidNames(root), ["style", "g"]);
});

test("scrub keeps a <style> block whose url()s are all #fragment refs", () => {
  // Mermaid's generated CSS points at its own gradients and markers this way.
  const style = el("style", [], [],
    "#x .node{fill:url(#x-gradient)}.edge{marker-end:url('#x-arrow')}@keyframes dash{to{stroke-dashoffset:0}}");
  const root = scrubbed(el("svg", [], [style, el("g")]));
  assert.deepEqual(kidNames(root), ["style", "g"]);
});

test("scrub drops a <style> block containing anything Mermaid could not write (B2)", () => {
  const cases: [string, string][] = [
    ["url external", ".a{background:url('https://mmd.invalid/x.png')}"],
    ["url same-origin", ".a{background:url(/af-mmd-probe/x.png)}"],
    ["url bare identifier (resolves relative → a fetch)", ".a{fill:url(some-gradient)}"],
    ["image-set", ".a{background:image-set('/x.png' 1x)}"],
    ["backslash", ".a{background:u\\72l('/x.png')}"],
    ["comment", ".a{background:u/**/rl('/x.png')}"],
    ["@import", "@import '/x.css';"],
    ["@charset", "@charset 'utf-8';"],
    ["@font-face", "@font-face{src:url(/x.woff)}"],
    ["@media", "@media screen{.a{fill:red}}"],
    ["@supports", "@supports (display:grid){.a{fill:red}}"],
    ["expression", ".a{width:expression(x())}"],
    ["javascript:", ".a{background:url(javascript:x())}"],
  ];
  for (const [label, css] of cases) {
    const root = scrubbed(el("svg", [], [el("style", [], [], css), el("g")]));
    assert.deepEqual(kidNames(root), ["g"], `${label} must drop the whole block`);
  }
});

test("pollUntil returns the first value the probe yields", async () => {
  let calls = 0;
  const got = await pollUntil(() => (++calls === 3 ? "ready" : undefined), 5_000);
  assert.equal(got, "ready");
  assert.equal(calls, 3);
});

test("pollUntil probes once more after the final sleep", async () => {
  // The probe turns ready the moment the deadline passes — a wait that
  // re-checked the deadline before probing after its last sleep would never
  // see the registration and would score a good load a failure.
  const timeoutMs = 30;
  const start = Date.now();
  const got = await pollUntil(
    () => (Date.now() - start >= timeoutMs ? "ready" : undefined),
    timeoutMs,
  );
  assert.equal(got, "ready");
});

test("pollUntil gives up past the deadline when nothing registers", async () => {
  const got = await pollUntil(() => undefined, 40);
  assert.equal(got, undefined);
});

test("pollUntil treats any defined value as done — even falsy ones", async () => {
  // The contract a probe must satisfy (#6669): "not yet" is undefined and
  // nothing else — a false returned for "not ready" ends the poll instantly,
  // which is why openRenderFrame maps its flag with `? true : undefined`.
  const got = await pollUntil(() => false, 5_000);
  assert.equal(got, false);
});

test("TimeoutLedger fails fast inside the cooldown, then lets the source retry", () => {
  const ledger = new TimeoutLedger(60_000, 4);
  ledger.note("k", 1_000);
  assert.equal(ledger.cooling("k", 1_000), true);
  assert.equal(ledger.cooling("k", 1_000 + 59_999), true, "still inside the window");
  assert.equal(ledger.cooling("k", 1_000 + 60_000), false, "window elapsed — retry allowed");
  assert.equal(ledger.cooling("k", 1_000 + 60_001), false, "the elapsed entry is gone for good");
  assert.equal(ledger.cooling("other", 1_000), false, "an unnoted key never cools");
});

test("TimeoutLedger notes a repeat timeout afresh and caps its size", () => {
  const ledger = new TimeoutLedger(60_000, 3);
  ledger.note("a", 0);
  ledger.note("b", 1);
  ledger.note("c", 2);
  ledger.note("a", 10); // a re-note moves `a` behind `c`
  ledger.note("d", 20); // over the cap — the oldest entry (`b`) is evicted
  assert.equal(ledger.cooling("b", 30), false, "oldest entry was evicted");
  for (const k of ["a", "c", "d"]) assert.equal(ledger.cooling(k, 30), true, `${k} still cools`);
});
