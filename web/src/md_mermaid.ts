// Rendering for ```mermaid fences in the Docs live preview (#6642).
//
// previewSpecs (md_live_preview.ts) only emits the spec; everything the fence
// turns into lives here: the block widget, the loader for the library's own
// lazy chunk (dist/lazy/mermaid.js — never the eager bundle, never a CDN,
// because af serves the web from embedded dist), the sanitizer the output
// passes through before it touches the DOM, and the theme wiring that
// re-renders mounted diagrams on a light/dark flip.
//
// Proposals are partly agent-authored, so the diagram source is semi-trusted
// and it renders inside the daemon's control origin. Three layers, in render
// order: mermaid runs inside a srcdoc iframe whose own CSP denies every load
// the diagram names (images/fonts/connections — any origin), the chunk is
// initialized with securityLevel 'strict' (mermaid DOMPurify-cleans label
// HTML, ignores click statements, and `secure` keeps %%{init}%% directives
// from undoing any of that or writing into generated CSS), and sanitizeSvg is
// ours: it keeps only the SVG vocabulary mermaid emits, so no <a>,
// <foreignObject>, <script>, event attribute, external reference or SMIL
// animation survives a render. The page's CSP is the last layer under all.

import type { EditorView } from "@codemirror/view";
import { WidgetType } from "@codemirror/view";
import { errorText } from "./api.js";

/** What dist/lazy/mermaid.js (src/mermaid_chunk.ts) puts on its frame's window. */
interface MermaidChunk {
  renderMermaidDiagram(source: string, theme: Record<string, string>): Promise<string>;
}

let chunk: Promise<MermaidChunk> | null = null;

// Renderers can make the browser fetch URLs the diagram names — label <img>
// metadata, flowchart img nodes, font/url() CSS — before the svg string ever
// reaches sanitizeSvg. So mermaid runs inside a srcdoc iframe whose own CSP
// (stacked on the page's) denies every fetch: nothing but same-origin scripts
// and inline styles is allowed in, images/fonts/connections are 'none' by
// default-src. The only thing that leaves the frame is the svg string.
const RENDER_FRAME_CSP =
  "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; " +
  "img-src data:; font-src 'none'; connect-src 'none'; media-src 'none'; " +
  "object-src 'none'; frame-src 'none'; worker-src 'none'; child-src 'none'; " +
  "manifest-src 'none'; base-uri 'none'; form-action 'none'";

// The frame lives as long as the page does, unless a render out-waits us —
// see renderWithTimeout, which tears it down so a hung render can't keep the
// chunk's serialized queue busy until reload.
let renderFrame: HTMLIFrameElement | null = null;

async function openRenderFrame(): Promise<MermaidChunk> {
  const url = new URL("./mermaid.js", import.meta.url).href;
  const frame = document.createElement("iframe");
  frame.setAttribute("aria-hidden", "true");
  frame.tabIndex = -1;
  frame.style.cssText = "position:fixed;left:-200vw;top:-200vh;width:1px;height:1px;border:0";
  // The DNS-prefetch meta MUST be first: browsers prefetch anchor hosts that
  // reach the frame inside label HTML, and CSP does not govern prefetch.
  frame.srcdoc =
    `<!doctype html><meta http-equiv="x-dns-prefetch-control" content="off">` +
    `<meta charset="utf-8">` +
    `<meta http-equiv="Content-Security-Policy" content="${RENDER_FRAME_CSP}">` +
    `<script type="module" src="${url}"><\/script>`;
  document.body.appendChild(frame);
  renderFrame = frame;
  const w = frame.contentWindow;
  if (!w) {
    dropRenderFrame();
    throw new Error("the mermaid render frame has no window");
  }
  // The chunk self-registers on the frame's window when its module executes;
  // a load failure only ever shows as a timeout, so a retry rebuilds it all.
  for (let i = 0; i < 1200; i++) {
    const api = (w as unknown as { __afMermaidChunk?: MermaidChunk }).__afMermaidChunk;
    if (api) return api;
    await new Promise((r) => setTimeout(r, 25));
  }
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
 *  stuck behind whatever hung the last one. */
function dropRenderFrame(): void {
  renderFrame?.remove();
  renderFrame = null;
  chunk = null;
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
  "style", "image",
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
  /image-set|\\|\/\*|@(?!keyframes\b)|expression|javascript:|vbscript:|-moz-binding|behavior\s*:/i;

// In an attribute value, image-set, backslash escapes, comment obfuscation
// and the scriptable schemes are never legitimate. url() is checked per
// occurrence instead — url(#...) is how markers, gradients and clips point at
// each other inside the file, and any other argument means a fetch.
const HOSTILE_ATTR_VALUE =
  /image-set|\\|\/\*|expression\s*\(|javascript:|vbscript:|-moz-binding|behavior\s*:/i;

const CSS_URL_ARG = /url[ \t\n\r\f]*\([ \t\n\r\f]*['"]?[ \t\n\r\f]*(.)/gi;

/** True when a value holds a url() whose target is anything but #fragment —
 *  a fetch the diagram chose. (A regex lookahead can't express this: the
 *  whitespace/quote skip would backtrack and let url( match alone.) */
function hasExternalCssUrl(value: string): boolean {
  for (const m of value.matchAll(CSS_URL_ARG)) if (m[1] !== "#") return true;
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
    if (local.startsWith("on") || local === "srcdoc") {
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

/** Renders with a parent-side timer: mermaid serializes renders through its
 *  own queue inside the frame, so a render that never settles — a denied load
 *  can hang it — would keep every later diagram waiting until reload. On
 *  timeout the whole frame is torn down (dropRenderFrame), so the next paint
 *  opens a fresh frame with an unstuck queue. */
function renderWithTimeout(m: MermaidChunk, source: string): Promise<string> {
  return Promise.race([
    m.renderMermaidDiagram(source, mermaidThemeVars()),
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new RenderTimeoutError()), RENDER_TIMEOUT_MS)),
  ]);
}

function renderCached(source: string, mode: "light" | "dark"): Promise<string> {
  const key = mode + "\n" + source;
  const hit = svgCache.get(key);
  if (hit) {
    svgCache.delete(key);
    svgCache.set(key, hit);
    return hit;
  }
  const p = loadMermaidChunk()
    .then((m) => renderWithTimeout(m, source))
    .catch((e: unknown) => {
      if (e instanceof RenderTimeoutError) dropRenderFrame();
      svgCache.delete(key);
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
    const svg = sanitizeSvg(await renderCached(h.source, mode));
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
