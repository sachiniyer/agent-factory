// Rendering for ```mermaid fences in the Docs live preview (#6642, #6669).
//
// previewSpecs (md_live_preview.ts) only emits the spec; everything the fence
// turns into lives here: the block widget, the loader for the library's own
// lazy document (dist/lazy/mermaid-frame.html — never the eager bundle,
// never a CDN, because af serves the web from embedded dist), the sanitizer
// the output passes through before it touches the DOM, and the theme wiring
// that re-renders mounted diagrams on a light/dark flip.
//
// Proposals are partly agent-authored, so the diagram source is semi-trusted
// and it renders inside the daemon's control origin. Three layers, in render
// order: mermaid runs inside an iframe navigated to that document, SANDBOXED
// without allow-same-origin (#6669) — an opaque origin, so its CSP denying
// every load the diagram names (images/fonts/connections — any origin) is
// backed by a privilege boundary too: the frame cannot read the parent's DOM
// or storage, and its requests carry no credentials. The library is inline
// in the document, so the frame fetches nothing at all on its own. It is
// initialized with
// securityLevel 'strict' (mermaid DOMPurify-cleans label HTML, ignores click
// statements, and `secure` keeps %%{init}%% directives from undoing any of
// that or writing into generated CSS), and sanitizeSvg is ours: it keeps only
// the SVG vocabulary mermaid emits, so no <a>, <foreignObject>, <script>,
// event attribute, external reference or SMIL animation survives a render.
// The page's CSP is the last layer under all.

import type { EditorView } from "@codemirror/view";
import { WidgetType } from "@codemirror/view";
import { errorText } from "./api.js";
import { decodeFrameReply, encodePortGrant, encodeRenderRequest, frameReplyFor, MSG_HELLO, MSG_READY, MSG_RENDERED } from "./mermaid_wire.js";

/** The render channel the frame's chunk answers — kept identical to the old
 *  in-window API so the render path below is unchanged; the implementation is
 *  postMessage now (see the frame channel below), not a cross-window call. */
interface MermaidChunk {
  renderMermaidDiagram(source: string, theme: Record<string, string>): Promise<string>;
}

let chunk: Promise<MermaidChunk> | null = null;

// Renderers can make the browser fetch URLs the diagram names — label <img>
// metadata, flowchart img nodes, font/url() CSS — before the svg string ever
// reaches sanitizeSvg. So mermaid runs inside an iframe whose own CSP denies
// every fetch (default-src 'none' bar its one inline module and inline
// styles): nothing the diagram names can be fetched, on any origin, and the
// only thing that leaves the frame is the svg string.
//
// #6669: the frame is SANDBOXED without allow-same-origin, so its document
// has an opaque origin — code in it cannot read the parent's DOM, storage or
// cookies, and a request it would make carries no credentials. It is a REAL
// navigated document, dist/lazy/mermaid-frame.html, not srcdoc: srcdoc
// inherits the page's CSP, and in WebKit an inherited 'self' matches nothing
// in an opaque origin — every load is denied. The document carries the whole
// library inline under a script-src hash the daemon names in its CSP
// (daemon/webserve.go), so the frame fetches nothing on its own:
// no credential-less subrequest for an authenticating proxy to refuse, and
// no CSP URL-source an IPv6 or proxied UI origin would have to spell. The
// header also carries `sandbox` and `frame-ancestors 'self'`, so the document
// is opaque however it is loaded — a foreign origin cannot frame it at all,
// and a top-level load gets the opaque origin too, never af's storage.

// The frame lives as long as the page does, unless a render out-waits us —
// see renderOne, which tears it down so a hung render can't keep the
// chunk's serialized queue busy until reload.
let renderFrame: HTMLIFrameElement | null = null;

// The frame document is ~1.5 MB on the wire (the whole library inline — the
// no-subresource property #6669 requires), so the open timeout is the fetch
// itself on a slow link: ~30 s at 400 kbps. 60 s leaves headroom for the
// parse+eval that follows; a hung open still fails and retries as before.
const OPEN_FRAME_TIMEOUT_MS = 60_000;
// A frame that just failed to open is not retried inside this window —
// otherwise every repaint would poll the whole open timeout again.
const OPEN_RETRY_AFTER_MS = 30_000;
let lastOpenFailedAt = 0;

// ── Frame channel ────────────────────────────────────────────────────────────
// The sandboxed frame's channel (mermaid_wire.ts): the window postMessage
// carries exactly two JSON-string messages — the frame's ready announce in,
// the parent's port grant out — and a transferred MessagePort carries
// everything after. The port is bound to the DOCUMENT, not the frame's
// WindowProxy: if the frame navigates itself, its replacement document holds
// no port, cannot have seen the hello nonce (it lived in the first
// document's ready announce), and anything posted to a dead port is
// discarded — so a navigated frame can never be sent a diagram source, no
// matter how long its replacement takes to fire `load`.
let frameReady = false;
let renderSeq = 0;
let renderPort: MessagePort | null = null;
let frameHello = "";
const pendingReplies = new Map<string, { resolve: (svg: string) => void; reject: (e: Error) => void }>();

function onFrameMessage(e: MessageEvent): void {
  // The ready announce counts only from the current frame's own window
  // (frameReplyFor owns the source check); a message from anything else —
  // including a dropped frame's late post — is not part of the protocol.
  const msg = frameReplyFor(e, renderFrame?.contentWindow);
  if (!msg || msg.t !== MSG_READY) return;
  // One channel per frame: a second ready (a replaced document announcing
  // itself before the parent observed the first) binds nothing.
  if (renderPort || !renderFrame) return;
  const channel = new MessageChannel();
  channel.port1.onmessage = onPortMessage;
  renderPort = channel.port1;
  frameHello = msg.hello;
  renderFrame.contentWindow?.postMessage(encodePortGrant(), "*", [channel.port2]);
}

function onPortMessage(e: MessageEvent): void {
  const msg = decodeFrameReply(e.data);
  if (!msg) return;
  if (msg.t === MSG_HELLO) {
    // The binding proof: the echo must be the nonce the ready announce
    // carried. A port that landed anywhere else can never produce it — the
    // nonce lived inside the first document's announce.
    if (msg.hello === frameHello) frameReady = true;
    else dropRenderFrame();
    return;
  }
  if (msg.t !== MSG_RENDERED) return;
  const pending = pendingReplies.get(msg.id);
  if (!pending) return;
  pendingReplies.delete(msg.id);
  if (msg.ok) pending.resolve(msg.svg);
  else pending.reject(new Error(msg.error));
}

/** The in-page stub the render path calls: post a request down the port,
 *  settle on the matching reply. Renders are already serialized by
 *  renderSerialized, so at most one is outstanding at a time — the map still
 *  keys by id so a stray or late reply can never be misattributed. `alive`
 *  is the frame's own still-first-document check: a frame that navigated
 *  itself is not ours to post to anymore. */
function frameApi(frame: HTMLIFrameElement, port: MessagePort, alive: () => boolean): MermaidChunk {
  return {
    renderMermaidDiagram(source, theme) {
      if (renderFrame !== frame || renderPort !== port || !alive()) {
        return Promise.reject(new Error("the mermaid render frame was dropped"));
      }
      const id = `r${++renderSeq}`;
      return new Promise<string>((resolve, reject) => {
        pendingReplies.set(id, { resolve, reject });
        // A port post needs no target origin — the entangled end is the only
        // possible receiver, and it dies with the document that holds it.
        port.postMessage(encodeRenderRequest(id, source, theme));
      });
    },
  };
}

/** Polls `probe` until it yields a value or `timeoutMs` runs out, on a wall
 *  deadline rather than an iteration count: throttled timers (a background
 *  tab) stretch sleeps, and counting polls would stretch the wait without
 *  bound. The probe runs once more after the final sleep — a registration
 *  landing inside it still counts, so a good load is never scored a failure
 *  and barred by the retry cooldown. */
export async function pollUntil<T>(probe: () => T | undefined, timeoutMs: number): Promise<T | undefined> {
  const deadline = Date.now() + timeoutMs;
  let got = probe();
  while (got === undefined && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 25));
    got = probe();
  }
  return got;
}

async function openRenderFrame(): Promise<MermaidChunk> {
  if (Date.now() - lastOpenFailedAt < OPEN_RETRY_AFTER_MS) {
    throw new Error("the mermaid render frame failed to open recently");
  }
  const url = new URL("./mermaid-frame.html", import.meta.url).href;
  const frame = document.createElement("iframe");
  frame.setAttribute("aria-hidden", "true");
  frame.tabIndex = -1;
  // Sandboxed to scripts and nothing else, BEFORE src, so the document is
  // born with an opaque origin (#6669). No allow-same-origin: that token
  // would hand the frame the parent's origin back and undo the isolation.
  frame.setAttribute("sandbox", "allow-scripts");
  frame.style.cssText = "position:fixed;left:-200vw;top:-200vh;width:1px;height:1px;border:0";
  // A second load event means the frame navigated itself. Its document —
  // the one the port was bound to — is gone, so whatever lives there now
  // must never see another diagram source: drop the whole frame and let the
  // next render open a fresh one. The port itself is already safe (it died
  // with the document that held it); this drop is the cleanup and the
  // belt-and-braces guard frameApi's sends consult too, so a post that would
  // race the drop fails instead of landing anywhere new.
  let navs = 0;
  frame.addEventListener("load", () => {
    navs++;
    if (navs > 1 && renderFrame === frame) dropRenderFrame();
  });
  frame.src = url;
  // The listener goes up before the frame does: the chunk's ready post can
  // only arrive after this synchronous block, but an early listener makes
  // that a fact rather than a scheduling assumption.
  window.addEventListener("message", onFrameMessage);
  frameReady = false;
  document.body.appendChild(frame);
  renderFrame = frame;
  // The chunk announces itself over postMessage once its module executes;
  // a load failure only ever shows as a timeout, so a retry rebuilds it all.
  // pollUntil waits for a DEFINED value, so the probe maps "not yet" to
  // undefined — polling the boolean itself would return false instantly.
  const ok = await pollUntil(() => (frameReady ? true : undefined), OPEN_FRAME_TIMEOUT_MS);
  if (ok && renderPort) return frameApi(frame, renderPort, () => navs === 1);
  lastOpenFailedAt = Date.now();
  dropRenderFrame();
  throw new Error("the mermaid chunk did not load in its render frame");
}

/** Owns the render frame once; a failed open is forgotten so a retry rebuilds. */
function loadMermaidChunk(): Promise<MermaidChunk> {
  if (!chunk) {
    chunk = openRenderFrame().catch((e: unknown) => {
      chunk = null;
      throw e;
    });
  }
  return chunk;
}

/** Removes the render frame and forgets the chunk, so the next render opens a
 *  fresh frame — and a fresh mermaid, whose serialized render queue is not
 *  stuck behind whatever hung the last one. Pending renders are failed out of
 *  the channel rather than left to wait out their own timeout. */
function dropRenderFrame(): void {
  renderFrame?.remove();
  renderFrame = null;
  chunk = null;
  frameReady = false;
  renderPort?.close();
  renderPort = null;
  frameHello = "";
  for (const pending of pendingReplies.values()) {
    pending.reject(new Error("the mermaid render frame was dropped"));
  }
  pendingReplies.clear();
}

/** The mode af paints in — the same rule theme.ts's currentMode() applies,
 *  inlined so this bundle does not pull the xterm palette in with it. */
function themeMode(): "light" | "dark" {
  const stamped = document.documentElement.dataset.afTheme;
  if (stamped === "light" || stamped === "dark") return stamped;
  const attr = document.documentElement.getAttribute("data-theme");
  if (attr === "light" || attr === "dark") return attr;
  try {
    return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  } catch {
    return "dark";
  }
}

/** af's resolved tokens as mermaid themeVariables, read live so a flip paints
 *  the next render in the new palette. Only the colors every diagram shares —
 *  the exotic per-type variables inherit from primary/secondary/tertiary. */
function mermaidThemeVars(): Record<string, string> {
  const cs = getComputedStyle(document.documentElement);
  // A token's resolved value must be a color mermaid understands — a theme
  // value it cannot parse throws mid-render, so odd values take the fallback.
  const v = (name: string, fallback: string): string => {
    const got = cs.getPropertyValue(name).trim();
    return got && CSS.supports("color", got) ? got : fallback;
  };
  const s = (name: string, fallback: string): string => cs.getPropertyValue(name).trim() || fallback;
  // Fallbacks stay theme-resolved too (the document's computed text/background),
  // never literals — component colors must come out of the theme mechanism.
  const ink = v("--af-ink", cs.color);
  const muted = v("--af-ink-muted", ink);
  const surface = v("--af-surface", cs.backgroundColor);
  const raised = v("--af-surface-raised", surface);
  const border = v("--af-border", ink);
  return {
    background: surface,
    fontFamily: s("--af-font-ui", cs.fontFamily),
    fontSize: "14px",
    primaryColor: raised,
    primaryTextColor: ink,
    primaryBorderColor: border,
    secondaryColor: surface,
    tertiaryColor: surface,
    lineColor: muted,
    textColor: ink,
    mainBkg: raised,
    secondBkg: surface,
    nodeTextColor: ink,
    titleColor: ink,
    clusterBkg: surface,
    clusterBorder: border,
    edgeLabelBackground: surface,
    defaultLinkColor: muted,
    noteBkgColor: raised,
    noteTextColor: ink,
    noteBorderColor: border,
    actorBkg: raised,
    actorTextColor: ink,
    actorBorder: border,
    actorLineColor: muted,
    signalColor: ink,
    signalTextColor: ink,
    labelBoxBkgColor: surface,
    labelBoxBorderColor: border,
    labelTextColor: ink,
    loopTextColor: ink,
    activationBkgColor: raised,
    activationBorderColor: border,
    sequenceNumberColor: surface,
    sectionBkgColor: surface,
    altSectionBkgColor: raised,
    sectionBkgColor2: raised,
    taskBkgColor: raised,
    taskTextColor: ink,
    taskTextDarkColor: ink,
    taskTextOutsideColor: muted,
    taskBorderColor: border,
    gridColor: border,
    todayLineColor: v("--af-accent", muted),
    pie1: v("--af-accent", muted),
    pie2: v("--af-callout-todo", muted),
    pie3: v("--af-ready", muted),
    pie4: v("--af-callout-question", muted),
    pie5: v("--af-attention", muted),
    pie6: v("--af-limit-reached", muted),
    pie7: v("--af-dead", muted),
    pie8: v("--af-archived", muted),
    pieTitleTextColor: ink,
    pieSectionTextColor: surface,
    pieLegendTextColor: ink,
  };
}

// ── Sanitizer ────────────────────────────────────────────────────────────────
// The element names a mermaid SVG is made of. Everything else is dropped with
// its subtree — <a> (click directives), <foreignObject> (label HTML), <script>,
// <iframe>, SMIL (<animate>, <set> — "no animation", and SMIL can trigger on
// events), <image>-adjacent fetch elements and anything unanticipated.
const KEEP = new Set([
  "svg", "g", "defs", "marker", "symbol", "use",
  "rect", "circle", "ellipse", "line", "polyline", "polygon", "path",
  "text", "tspan", "textpath",
  "title", "desc",
  "clippath", "mask", "pattern",
  "lineargradient", "radialgradient", "stop",
  // <switch> keeps its children sanitized recursively: renderers (journey
  // labels among them) pair a <foreignObject> with an SVG <text> fallback,
  // and dropping the switch takes the fallback with it — the foreignObject
  // branch still goes.
  "style", "image", "switch",
]);

const SVG_NS = "http://www.w3.org/2000/svg";

// A <style> block survives only when it holds nothing mermaid could not have
// written itself: no image-set, no backslash (u\72l escapes), no comment
// opener (u/**/rl), no at-rule but the @keyframes mermaid ships in every
// diagram (the page styles pin animation:none over them anyway) — and every
// url() must be a #fragment ref, which is how mermaid's generated CSS points
// at its own gradients and markers. One hit drops the whole block: a
// diagram's own CSS never needs any of the rest, so one means the source was
// hostile, not stylish.
const HOSTILE_STYLE =
  /image-set|\\|\/\*|@(?!keyframes\b)|expression|javascript:|vbscript:|-moz-binding|behavior\s*:|\b(?:src|image|element|cross-fade)[ \t\n\r\f]*\(/i;

// In an attribute value, image-set, backslash escapes, comment obfuscation
// and the scriptable schemes are never legitimate — and neither are the
// other CSS functions that name a load (image, element, src, cross-fade).
// url() is checked per occurrence instead — url(#...) is how markers,
// gradients and clips point at each other inside the file, and any other
// argument means a fetch.
const HOSTILE_ATTR_VALUE =
  /image-set|\\|\/\*|expression\s*\(|javascript:|vbscript:|-moz-binding|behavior\s*:|\b(?:src|image|element|cross-fade)[ \t\n\r\f]*\(/i;

const CSS_WS = new Set([" ", "\t", "\n", "\r", "\f"]);

/** True when a value holds a url() whose target is anything but #fragment —
 *  a fetch the diagram chose. Plain string scan, not a regex: no occurrence
 *  may be skipped. A `(.)` capture misses U+2028/U+2029 (`.` excludes line
 *  separators, but CSS treats them as ordinary URL characters), which let
 *  url(\u2028/…) slip through a character-class scan entirely. */
function hasExternalCssUrl(value: string): boolean {
  const v = value.toLowerCase();
  let i = v.indexOf("url");
  while (i !== -1) {
    let j = i + 3;
    while (CSS_WS.has(v[j])) j++;
    if (v[j] !== "(") {
      i = v.indexOf("url", j);
      continue;
    }
    j++;
    while (CSS_WS.has(v[j])) j++;
    if (v[j] === '"' || v[j] === "'") j++;
    if (v[j] !== "#") return true;
    i = v.indexOf("url", j);
  }
  return false;
}

export function scrub(el: Element): void {
  for (const child of [...el.children]) {
    const name = child.localName.toLowerCase();
    // Only SVG-namespace elements on the keep list: a same-named element in
    // another namespace (MathML, XHTML) never survives.
    if (child.namespaceURI !== SVG_NS || !KEEP.has(name)) {
      child.remove();
      continue;
    }
    if (name === "style") {
      const css = child.textContent ?? "";
      if (HOSTILE_STYLE.test(css) || hasExternalCssUrl(css)) {
        child.remove();
        continue;
      }
    }
    scrub(child);
  }
  for (const attr of [...el.attributes]) {
    // Attributes are matched by local name, so namespaced lookalikes
    // (evil:href, evil:onclick) get the same treatment as the plain names.
    const local = attr.localName.toLowerCase();
    const value = attr.value;
    if (local.startsWith("on") || local === "srcdoc" ||
        // A <switch> picks its first child whose conditions pass — its
        // conditional-processing attributes are dropped so the surviving
        // child always renders; a diagram must not gate which of its own
        // representations shows.
        local === "requiredfeatures" || local === "requiredextensions" ||
        local === "systemlanguage") {
      el.removeAttribute(attr.name);
    } else if (local === "href") {
      // Internal fragment refs (#id) stay, and <image> keeps inline raster
      // data — a link out of the file does not. Only <image> may carry data:,
      // only the raster types a renderer legitimately emits. The trim is C0
      // control and space only — what the URL parser strips — not JS trim(),
      // which also strips unicode spaces a URL would keep, hiding the true
      // first character from the # check.
      const v = value.replace(/^[\x00-\x20]+|[\x00-\x20]+$/g, "").toLowerCase();
      const ok = v.startsWith("#") ||
        (el.localName.toLowerCase() === "image" && /^data:image\/(png|gif|jpeg|webp);base64,/.test(v));
      if (!ok) el.removeAttribute(attr.name);
    } else if (HOSTILE_ATTR_VALUE.test(value) || hasExternalCssUrl(value)) {
      el.removeAttribute(attr.name);
    }
  }
}

/** Parses mermaid's SVG string and returns it sanitized, imported into the
 *  live document. Throws when what came back is not SVG at all. */
export function sanitizeSvg(svgText: string): SVGSVGElement {
  const parsed = new DOMParser().parseFromString(svgText, "image/svg+xml");
  const root = parsed.documentElement;
  if (root.localName !== "svg" || root.getElementsByTagName("parsererror").length > 0) {
    throw new Error("the renderer returned something that was not SVG");
  }
  scrub(root);
  // DOMParser types the root as Element; the localName check above is the
  // proof it is the SVG root.
  return document.importNode(root, true) as unknown as SVGSVGElement;
}

// ── Rendering ────────────────────────────────────────────────────────────────

interface DiagramHandle {
  source: string;
  /** Bumped on every render call, so a stale async landing cannot overwrite a
   *  newer result (or a newer source) for the same element. */
  gen: number;
}

// Mounted diagram elements → what they render. Lets a theme flip repaint every
// live diagram without tracking widgets; pruned when an element is destroyed
// or found disconnected.
const live = new Map<HTMLElement, DiagramHandle>();

// Rendered SVG by mode+source, so the cursor-in/source/cursor-out dance — which
// rebuilds the widget each time — does not re-run the layout engine. Failures
// are not kept. The map is small because a pane shows few diagrams.
const svgCache = new Map<string, Promise<string>>();
const SVG_CACHE_MAX = 32;

class RenderTimeoutError extends Error {
  constructor() {
    super("render timed out");
  }
}

const RENDER_TIMEOUT_MS = 20_000;

// One render in flight, serialized in the PARENT: the chunk keeps its own
// queue too, but a per-render timeout can only be fair here — a timer that
// started at enqueue would share a slow diagram's deadline with every
// diagram queued behind it.
let renderQueue: Promise<unknown> = Promise.resolve();

function renderSerialized(source: string, theme: Record<string, string>): Promise<string> {
  const job = renderQueue.then(() => renderOne(source, theme));
  renderQueue = job.catch(() => {});
  return job;
}

/** Dispatches one render on the current frame with the timeout running.
 *  On timeout the frame is torn down — but only if it is still the frame
 *  this render was dispatched on, so a dead frame's late timer can never
 *  kill a healthier replacement. The timer is cleared on settle so success
 *  carries no pending teardown. */
async function renderOne(source: string, theme: Record<string, string>): Promise<string> {
  const m = await loadMermaidChunk();
  const frame = renderFrame;
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      m.renderMermaidDiagram(source, theme),
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new RenderTimeoutError()), RENDER_TIMEOUT_MS);
      }),
    ]);
  } catch (e) {
    if (e instanceof RenderTimeoutError && renderFrame === frame) {
      dropRenderFrame();
    }
    throw e;
  } finally {
    clearTimeout(timer);
  }
}

// A source that timed out is likely to hang again, but "likely" is not
// "certain" — a render can lose its race to a busy main thread rather than
// to a poisoned diagram. So a timeout fails fast only inside a cooldown and
// then earns a real retry, and the ledger is capped so a flood of distinct
// hanging sources cannot grow it without bound.
const TIMED_OUT_RETRY_MS = 60_000;
const TIMED_OUT_MAX = 32;

/** Per-key timeout memory for renderCached: when a key last timed out,
 *  bounded so the ledger stays small. */
export class TimeoutLedger {
  private at = new Map<string, number>();

  constructor(
    private readonly cooldownMs: number,
    private readonly max: number,
  ) {}

  /** True while `key` sits inside its cooldown. Once the cooldown has
   *  elapsed the entry is dropped, so the next render tries for real. */
  cooling(key: string, now: number): boolean {
    const t = this.at.get(key);
    if (t === undefined) return false;
    if (now - t < this.cooldownMs) return true;
    this.at.delete(key);
    return false;
  }

  /** Records a fresh timeout for `key`, evicting the oldest entry if the
   *  ledger has reached its cap. */
  note(key: string, now: number): void {
    this.at.delete(key);
    this.at.set(key, now);
    if (this.at.size > this.max) {
      this.at.delete(this.at.keys().next().value as string);
    }
  }
}

const timedOut = new TimeoutLedger(TIMED_OUT_RETRY_MS, TIMED_OUT_MAX);

function renderCached(source: string, mode: "light" | "dark", theme: Record<string, string>): Promise<string> {
  const key = mode + "\n" + source;
  const hit = svgCache.get(key);
  if (hit) {
    svgCache.delete(key);
    svgCache.set(key, hit);
    return hit;
  }
  if (timedOut.cooling(key, Date.now())) return Promise.reject(new RenderTimeoutError());
  const p = renderSerialized(source, theme).catch((e: unknown) => {
    svgCache.delete(key);
    if (e instanceof RenderTimeoutError) timedOut.note(key, Date.now());
    throw e;
  });
  svgCache.set(key, p);
  if (svgCache.size > SVG_CACHE_MAX) {
    svgCache.delete(svgCache.keys().next().value as string);
  }
  return p;
}

function showError(el: HTMLElement, source: string, e: unknown): void {
  el.classList.add("cm-md-mermaid-bad");
  const msg = document.createElement("p");
  msg.className = "cm-md-mermaid-errtext";
  msg.textContent = `Could not render this diagram: ${errorText(e)}`;
  const pre = document.createElement("pre");
  pre.className = "cm-md-mermaid-src";
  pre.textContent = source;
  el.replaceChildren(msg, pre);
}

async function paint(el: HTMLElement): Promise<void> {
  const h = live.get(el);
  if (!h) return;
  const gen = ++h.gen;
  try {
    const mode = themeMode();
    // Mode and palette are snapshotted together here: reading the variables
    // at dispatch would let a mid-queue theme flip store one mode's palette
    // under the other's cache key.
    const svg = sanitizeSvg(await renderCached(h.source, mode, mermaidThemeVars()));
    if (gen !== h.gen || !el.isConnected) return;
    el.classList.remove("cm-md-mermaid-bad");
    el.dataset.mmdTheme = mode;
    el.replaceChildren(svg);
  } catch (e) {
    if (gen !== h.gen || !el.isConnected) return;
    showError(el, h.source, e);
  }
}

let watching = false;

/** Repaints every live diagram when the theme flips — explicit flips stamp
 *  data-af-theme/data-theme on <html>, and a System choice follows the OS
 *  media query. Registered once, the first time a diagram mounts. */
function watchTheme(): void {
  if (watching) return;
  watching = true;
  const rerender = (): void => {
    for (const el of live.keys()) {
      if (!el.isConnected) {
        live.delete(el);
      } else {
        void paint(el);
      }
    }
  };
  new MutationObserver(rerender).observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["data-af-theme", "data-theme"],
  });
  try {
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", rerender);
  } catch {
    // No matchMedia: only explicit flips repaint, which the observer covers.
  }
}

/** The block widget that owns one ```mermaid fence's rendered output. Its
 *  initial contents are the source itself, so a slow first render reads as the
 *  block, not a hole; a mousedown puts the cursor inside the fence so its
 *  source can be edited, exactly like the table widget. */
export class MermaidWidget extends WidgetType {
  constructor(readonly source: string) {
    super();
  }
  override eq(other: MermaidWidget): boolean {
    return other.source === this.source;
  }
  override toDOM(view: EditorView): HTMLElement {
    const wrap = document.createElement("div");
    wrap.className = "cm-md-mermaid";
    const pre = document.createElement("pre");
    pre.className = "cm-md-mermaid-src";
    pre.textContent = this.source;
    wrap.append(pre);
    live.set(wrap, { source: this.source, gen: 0 });
    watchTheme();
    void paint(wrap);
    wrap.addEventListener("mousedown", (e) => {
      e.preventDefault();
      const pos = view.posAtDOM(wrap);
      view.dispatch({ selection: { anchor: pos } });
      view.focus();
    });
    return wrap;
  }
  override destroy(dom: HTMLElement): void {
    live.delete(dom);
  }
  override ignoreEvent(): boolean {
    return false;
  }
}
