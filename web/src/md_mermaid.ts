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
// and it renders inside the daemon's control origin. The chunk is initialized
// with securityLevel 'strict' — mermaid encodes label HTML, ignores click
// statements, and its `secure` list keeps %%{init}%% directives from undoing
// any of that — and sanitizeSvg is the second, ours: it keeps only the SVG
// vocabulary mermaid emits, so no <a>, <foreignObject>, <script>, event
// attribute, external reference or SMIL animation survives a render. The
// page's CSP (default-src 'self') is the third layer under both.

import type { EditorView } from "@codemirror/view";
import { WidgetType } from "@codemirror/view";
import { errorText } from "./api.js";

/** What dist/lazy/mermaid.js (src/mermaid_chunk.ts) exports. */
interface MermaidChunk {
  renderMermaidDiagram(source: string, theme: Record<string, string>): Promise<string>;
}

let chunk: Promise<MermaidChunk> | null = null;

/** Fetches the mermaid chunk once; a failed load is forgotten so a retry refetches. */
function loadMermaidChunk(): Promise<MermaidChunk> {
  if (!chunk) {
    // A computed specifier keeps esbuild from inlining the chunk here (same
    // trick docs_tab.ts uses for this very bundle).
    const url = new URL("./mermaid.js", import.meta.url).href;
    chunk = (import(url) as Promise<MermaidChunk>).catch((e: unknown) => {
      chunk = null;
      throw e;
    });
  }
  return chunk;
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
  const v = (name: string, fallback: string): string => cs.getPropertyValue(name).trim() || fallback;
  // Fallbacks stay theme-resolved too (the document's computed text/background),
  // never literals — component colors must come out of the theme mechanism.
  const ink = v("--af-ink", cs.color);
  const muted = v("--af-ink-muted", ink);
  const surface = v("--af-surface", cs.backgroundColor);
  const raised = v("--af-surface-raised", surface);
  const border = v("--af-border", ink);
  return {
    background: surface,
    fontFamily: v("--af-font-ui", cs.fontFamily),
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

// CSS that reaches outside the SVG — a fetch (external url, @import) or an
// injected rule (expression, binding, script schemes). url(#id) stays: it is
// how markers, clipPaths and gradients reference each other inside the file.
const EXTERNAL_CSS = /url\s*\(\s*['"]?\s*(?!#)|@import|@charset|@namespace|expression\s*\(|javascript:|vbscript:|-moz-binding|behavior\s*:/i;

function scrub(el: Element): void {
  for (const child of [...el.children]) {
    if (!KEEP.has(child.localName.toLowerCase())) {
      child.remove();
      continue;
    }
    if (child.localName.toLowerCase() === "style" && EXTERNAL_CSS.test(child.textContent ?? "")) {
      // The whole block goes: a diagram's own CSS never needs an external
      // reference, so one means the source was hostile, not stylish.
      child.remove();
      continue;
    }
    scrub(child);
  }
  for (const attr of [...el.attributes]) {
    const name = attr.name.toLowerCase();
    const value = attr.value;
    if (name.startsWith("on") || name === "srcdoc") {
      el.removeAttribute(attr.name);
    } else if (name === "href" || name === "xlink:href") {
      // Internal fragment refs (#id) stay, and <image> keeps inline raster
      // data — a link out of the file does not. Only <image> may carry data:;
      // on <use> it can name a whole second document.
      const v = value.trim().toLowerCase();
      const ok = v.startsWith("#") || (el.localName.toLowerCase() === "image" && v.startsWith("data:image/"));
      if (!ok) el.removeAttribute(attr.name);
    } else if (EXTERNAL_CSS.test(value)) {
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

function renderCached(source: string, mode: "light" | "dark"): Promise<string> {
  const key = mode + "\n" + source;
  const hit = svgCache.get(key);
  if (hit) {
    svgCache.delete(key);
    svgCache.set(key, hit);
    return hit;
  }
  const p = loadMermaidChunk()
    .then((m) => m.renderMermaidDiagram(source, mermaidThemeVars()))
    .catch((e: unknown) => {
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
