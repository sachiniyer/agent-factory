// Rendering for fenced code blocks in the Docs live preview (#6731).
//
// previewSpecs (md_live_preview.ts) only emits the spec; everything the fence
// turns into lives here: the block widget, the loader for the grammars' lazy
// chunk (dist/lazy/codeblocks.js — never the eager shell or the Docs bundle,
// never a CDN), and the DOM paint. On the cursor's lines the fence keeps its
// line styling instead, so its source stays editable.
//
// Code is only ever text: spans are built with createElement/textContent, and
// nothing in a block can become markup or make the browser fetch anything.

import type { EditorView } from "@codemirror/view";
import { WidgetType } from "@codemirror/view";

type CodeblocksModule = typeof import("./codeblocks_chunk.js");

let chunk: Promise<CodeblocksModule | null> | null = null;

/** Loads the grammars' chunk once; a failed load resolves to null, which
 *  leaves every block as plain code rather than showing an error. */
function loadCodeblocksChunk(): Promise<CodeblocksModule | null> {
  if (!chunk) {
    // A computed specifier keeps esbuild from inlining the chunk here — the
    // docs bundle must fetch it by URL only when a doc needs it, like
    // docs_tab.ts loads the Docs bundle itself.
    const url = new URL("./codeblocks.js", import.meta.url).href;
    chunk = (import(url) as Promise<CodeblocksModule>).catch(() => null);
  }
  return chunk;
}

/** Builds the content of a rendered block as DOM nodes: plain text between
 *  highlighted spans, all as text — a code block can never carry markup into
 *  the document. `spans` null means plain code (unknown language, oversized
 *  source, chunk failed), and the caller leaves the block unhighlighted. */
export function buildCodeNodes(source: string, spans: readonly { from: number; to: number; cls: string }[]): Node[] {
  const nodes: Node[] = [];
  let pos = 0;
  for (const span of spans) {
    if (span.from > pos) nodes.push(document.createTextNode(source.slice(pos, span.from)));
    const el = document.createElement("span");
    el.className = span.cls;
    el.textContent = source.slice(span.from, span.to);
    nodes.push(el);
    pos = span.to;
  }
  if (pos < source.length) nodes.push(document.createTextNode(source.slice(pos)));
  return nodes;
}

// Rendered code by language+source, so the cursor-in/source/cursor-out dance
// — which rebuilds the widget each time — does not re-tokenise. The map is
// small because a pane shows few blocks; null means "don't paint again"
// (unknown language, oversized source or a failed chunk).
const rendered = new Map<string, Promise<Node[] | null>>();
const RENDERED_MAX = 32;

function highlightedNodes(lang: string, source: string): Promise<Node[] | null> {
  const key = lang + "\n" + source;
  let hit = rendered.get(key);
  if (hit !== undefined) return hit;
  hit = loadCodeblocksChunk().then((m) => {
    const spans = m?.highlightRanges(source, lang);
    return spans ? buildCodeNodes(source, spans) : null;
  });
  rendered.set(key, hit);
  if (rendered.size > RENDERED_MAX) {
    rendered.delete(rendered.keys().next().value as string);
  }
  return hit;
}

/** The block widget that owns one fenced block's rendered output. Its code
 *  paints plain first, then the lazy chunk's spans swap in — so a slow first
 *  load reads as the block, not a hole, and a chunk that never arrives still
 *  leaves correct text. A mousedown puts the cursor inside the fence so its
 *  source can be edited, exactly like the table and mermaid widgets. */
export class CodeBlockWidget extends WidgetType {
  constructor(
    readonly source: string,
    readonly lang: string,
  ) {
    super();
  }
  override eq(other: CodeBlockWidget): boolean {
    return other.source === this.source && other.lang === this.lang;
  }
  override toDOM(view: EditorView): HTMLElement {
    const wrap = document.createElement("div");
    wrap.className = "cm-md-fence";
    if (this.lang) {
      const lang = document.createElement("div");
      lang.className = "cm-md-fence-lang";
      lang.textContent = this.lang;
      wrap.append(lang);
    }
    const pre = document.createElement("pre");
    pre.className = "cm-md-fence-code";
    const code = document.createElement("code");
    code.textContent = this.source;
    pre.append(code);
    wrap.append(pre);
    // No language → never load the chunk: a plain fence stays plain without
    // spending the fetch. A destroyed widget's node is only ever written by
    // this paint, so landing on it after unmount costs nothing.
    if (this.lang) {
      void highlightedNodes(this.lang, this.source).then((nodes) => {
        if (nodes) code.replaceChildren(...nodes.map((n) => n.cloneNode(true)));
      });
    }
    wrap.addEventListener("mousedown", (e) => {
      e.preventDefault();
      const pos = view.posAtDOM(wrap);
      view.dispatch({ selection: { anchor: pos } });
      view.focus();
    });
    return wrap;
  }
  override ignoreEvent(): boolean {
    return false;
  }
}
