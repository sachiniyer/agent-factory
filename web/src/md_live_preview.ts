// Live preview for the Docs editor: markdown shows rendered, except on the lines
// that hold the cursor, which show their source. Decorations only hide or style
// source text; the buffer is never rewritten, so what the agent reads is exactly
// what the user typed.
//
// previewSpecs is the pure part (EditorState in, plain specs out) so tests can
// check it without a DOM. livePreview turns the specs into decorations.

import { type EditorState, type Extension, type Range, StateEffect, StateField } from "@codemirror/state";
import { Decoration, type DecorationSet, EditorView, WidgetType } from "@codemirror/view";
import { ensureSyntaxTree, syntaxTree } from "@codemirror/language";
import type { SyntaxNode } from "@lezer/common";
import { CALLOUT_LABELS, type CalloutKind, calloutKindOfLine } from "./callouts.js";

export type Align = "" | "left" | "center" | "right";

export type PreviewSpec =
  | { type: "line"; pos: number; cls: string }
  | { type: "mark"; from: number; to: number; cls: string }
  | { type: "hide"; from: number; to: number }
  | { type: "bullet"; from: number; to: number }
  | { type: "callout-label"; from: number; to: number; kind: CalloutKind }
  | { type: "table"; from: number; to: number; rows: string[][]; align: Align[] };

const PARSE_BUDGET_MS = 50;

/** The line numbers (1-based) that any selection range touches. */
export function activeLines(state: EditorState): Set<number> {
  const lines = new Set<number>();
  for (const r of state.selection.ranges) {
    const first = state.doc.lineAt(r.from).number;
    const last = state.doc.lineAt(r.to).number;
    for (let n = first; n <= last; n++) lines.add(n);
  }
  return lines;
}

function splitRow(line: string): string[] {
  let s = line.trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
  const cells: string[] = [];
  let cell = "";
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (ch === "\\" && s[i + 1] === "|") {
      cell += "|";
      i++;
    } else if (ch === "|") {
      cells.push(cell.trim());
      cell = "";
    } else {
      cell += ch;
    }
  }
  cells.push(cell.trim());
  return cells;
}

/** Reads a GFM table's source lines into rows of cell text and the column
 *  alignment that the delimiter row sets. */
export function parseTable(lines: readonly string[]): { rows: string[][]; align: Align[] } {
  const header = splitRow(lines[0] ?? "");
  const align = splitRow(lines[1] ?? "").map((d): Align => {
    const left = d.startsWith(":");
    const right = d.endsWith(":");
    return left && right ? "center" : right ? "right" : left ? "left" : "";
  });
  const rows = [header, ...lines.slice(2).map(splitRow)];
  return { rows, align };
}

function childrenNamed(node: SyntaxNode, name: string): SyntaxNode[] {
  const out: SyntaxNode[] = [];
  for (let c = node.firstChild; c; c = c.nextSibling) {
    if (c.name === name) out.push(c);
  }
  return out;
}

export function previewSpecs(state: EditorState, active: ReadonlySet<number>): PreviewSpec[] {
  const doc = state.doc;
  const tree = ensureSyntaxTree(state, doc.length, PARSE_BUDGET_MS) ?? syntaxTree(state);
  const specs: PreviewSpec[] = [];
  // `[!question]` also parses as a shortcut link; the callout owns that range.
  const calloutTags: { from: number; to: number }[] = [];
  const lineActive = (pos: number): boolean => active.has(doc.lineAt(pos).number);
  const rangeActive = (from: number, to: number): boolean => {
    const last = doc.lineAt(to).number;
    for (let n = doc.lineAt(from).number; n <= last; n++) {
      if (active.has(n)) return true;
    }
    return false;
  };
  const hide = (from: number, to: number): void => {
    if (to > from) specs.push({ type: "hide", from, to });
  };
  const withSpace = (to: number): number => (doc.sliceString(to, to + 1) === " " ? to + 1 : to);
  const lineClasses = (from: number, to: number, cls: (n: number, first: number, last: number) => string): void => {
    const first = doc.lineAt(from).number;
    const last = doc.lineAt(to).number;
    for (let n = first; n <= last; n++) {
      specs.push({ type: "line", pos: doc.line(n).from, cls: cls(n, first, last) });
    }
  };

  tree.iterate({
    enter(ref) {
      const node = ref.node;
      const name = node.name;
      const heading = /^(?:ATX|Setext)Heading(\d)$/.exec(name);
      if (heading) {
        specs.push({ type: "line", pos: doc.lineAt(node.from).from, cls: `cm-md-h cm-md-h${heading[1]}` });
        return;
      }
      switch (name) {
        case "HeaderMark": {
          if (lineActive(node.from)) return;
          const isClosing = node.from > doc.lineAt(node.from).from && node.parent?.name.startsWith("ATX");
          if (isClosing) {
            const before = doc.sliceString(node.from - 1, node.from) === " " ? node.from - 1 : node.from;
            hide(before, node.to);
          } else {
            hide(node.from, withSpace(node.to));
          }
          return;
        }
        case "Emphasis":
          specs.push({ type: "mark", from: node.from, to: node.to, cls: "cm-md-em" });
          return;
        case "StrongEmphasis":
          specs.push({ type: "mark", from: node.from, to: node.to, cls: "cm-md-strong" });
          return;
        case "Strikethrough":
          specs.push({ type: "mark", from: node.from, to: node.to, cls: "cm-md-strike" });
          return;
        case "InlineCode":
          specs.push({ type: "mark", from: node.from, to: node.to, cls: "cm-md-code" });
          return;
        case "EmphasisMark":
        case "StrikethroughMark":
          if (!lineActive(node.from)) hide(node.from, node.to);
          return;
        case "CodeMark":
          if (node.parent?.name === "InlineCode" && !lineActive(node.from)) hide(node.from, node.to);
          return;
        case "Link": {
          if (calloutTags.some((t) => node.from < t.to && node.to > t.from)) return false;
          const marks = childrenNamed(node, "LinkMark");
          const open = marks[0];
          const close = marks[1];
          if (!open || !close || open.to > close.from) return;
          specs.push({ type: "mark", from: open.to, to: close.from, cls: "cm-md-link" });
          if (!rangeActive(node.from, node.to)) {
            hide(open.from, open.to);
            hide(close.from, node.to);
          }
          return;
        }
        case "ListMark": {
          const list = node.parent?.parent?.name;
          if (list === "BulletList" && !lineActive(node.from)) {
            specs.push({ type: "bullet", from: node.from, to: node.to });
          } else {
            specs.push({ type: "mark", from: node.from, to: node.to, cls: "cm-md-listmark" });
          }
          return;
        }
        case "TaskMarker":
          specs.push({ type: "mark", from: node.from, to: node.to, cls: "cm-md-task" });
          return;
        case "Blockquote": {
          if (node.parent?.name === "Blockquote") return;
          const firstLine = doc.lineAt(node.from);
          const kind = calloutKindOfLine(firstLine.text);
          if (kind) {
            lineClasses(node.from, node.to, (n, first, last) =>
              `cm-callout cm-callout-${kind}${n === first ? " cm-callout-first" : ""}${n === last ? " cm-callout-last" : ""}`);
            const open = firstLine.text.indexOf("[!");
            const close = firstLine.text.indexOf("]", open);
            const from = firstLine.from + open;
            const to = firstLine.from + close + 1;
            calloutTags.push({ from, to });
            if (!lineActive(firstLine.from)) {
              specs.push({ type: "callout-label", from, to: withSpace(to), kind });
            }
          } else {
            lineClasses(node.from, node.to, () => "cm-md-quote");
          }
          return;
        }
        case "QuoteMark":
          if (!lineActive(node.from)) hide(node.from, withSpace(node.to));
          return;
        case "FencedCode":
        case "CodeBlock":
          lineClasses(node.from, node.to, () => "cm-md-codeblock");
          return false;
        case "HorizontalRule":
          specs.push({ type: "line", pos: doc.lineAt(node.from).from, cls: "cm-md-hr" });
          if (!lineActive(node.from)) hide(node.from, node.to);
          return;
        case "Table": {
          const first = doc.lineAt(node.from);
          const last = doc.lineAt(node.to);
          if (rangeActive(first.from, last.to)) {
            lineClasses(first.from, last.to, () => "cm-md-table-source");
            return false;
          }
          const lines: string[] = [];
          for (let n = first.number; n <= last.number; n++) lines.push(doc.line(n).text);
          specs.push({ type: "table", from: first.from, to: last.to, ...parseTable(lines) });
          return false;
        }
        default:
          return;
      }
    },
  });
  return specs;
}

class BulletWidget extends WidgetType {
  override eq(): boolean {
    return true;
  }
  toDOM(): HTMLElement {
    const span = document.createElement("span");
    span.className = "cm-md-bullet";
    span.textContent = "•";
    return span;
  }
}

class CalloutLabelWidget extends WidgetType {
  constructor(readonly kind: CalloutKind) {
    super();
  }
  override eq(other: CalloutLabelWidget): boolean {
    return other.kind === this.kind;
  }
  toDOM(): HTMLElement {
    const span = document.createElement("span");
    span.className = `cm-callout-label cm-callout-label-${this.kind}`;
    span.textContent = CALLOUT_LABELS[this.kind];
    return span;
  }
}

class TableWidget extends WidgetType {
  private readonly key: string;
  constructor(readonly rows: string[][], readonly align: Align[]) {
    super();
    this.key = JSON.stringify([rows, align]);
  }
  override eq(other: TableWidget): boolean {
    return other.key === this.key;
  }
  toDOM(view: EditorView): HTMLElement {
    const wrap = document.createElement("div");
    wrap.className = "cm-md-table-wrap";
    const table = document.createElement("table");
    table.className = "cm-md-table";
    const [header = [], ...body] = this.rows;
    const row = (cells: string[], tag: "th" | "td"): HTMLTableRowElement => {
      const tr = document.createElement("tr");
      for (let i = 0; i < Math.max(cells.length, header.length); i++) {
        const cell = document.createElement(tag);
        cell.textContent = cells[i] ?? "";
        const a = this.align[i];
        if (a) cell.style.textAlign = a;
        tr.append(cell);
      }
      return tr;
    };
    const thead = document.createElement("thead");
    thead.append(row(header, "th"));
    const tbody = document.createElement("tbody");
    for (const cells of body) tbody.append(row(cells, "td"));
    table.append(thead, tbody);
    wrap.append(table);
    // A click moves the cursor into the table, which then shows its source.
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

const bullet = Decoration.replace({ widget: new BulletWidget() });
const hidden = Decoration.replace({});

export function specsToDecorations(specs: readonly PreviewSpec[]): DecorationSet {
  const ranges: Range<Decoration>[] = [];
  for (const s of specs) {
    switch (s.type) {
      case "line":
        ranges.push(Decoration.line({ class: s.cls }).range(s.pos));
        break;
      case "mark":
        if (s.to > s.from) ranges.push(Decoration.mark({ class: s.cls }).range(s.from, s.to));
        break;
      case "hide":
        ranges.push(hidden.range(s.from, s.to));
        break;
      case "bullet":
        ranges.push(bullet.range(s.from, s.to));
        break;
      case "callout-label":
        ranges.push(Decoration.replace({ widget: new CalloutLabelWidget(s.kind) }).range(s.from, s.to));
        break;
      case "table":
        ranges.push(Decoration.replace({ widget: new TableWidget(s.rows, s.align), block: true }).range(s.from, s.to));
        break;
    }
  }
  return Decoration.set(ranges, true);
}

const setFocused = StateEffect.define<boolean>();

interface PreviewState {
  focused: boolean;
  decorations: DecorationSet;
}

// Source shows only on the cursor's lines while the editor has focus, so a doc
// that has just opened, or that the user has left, reads fully rendered.
function build(state: EditorState, focused: boolean): PreviewState {
  return { focused, decorations: specsToDecorations(previewSpecs(state, focused ? activeLines(state) : new Set())) };
}

const previewField = StateField.define<PreviewState>({
  create: (state) => build(state, false),
  update(value, tr) {
    let focused = value.focused;
    for (const e of tr.effects) {
      if (e.is(setFocused)) focused = e.value;
    }
    if (focused !== value.focused || tr.docChanged || tr.selection || syntaxTree(tr.state) !== syntaxTree(tr.startState)) {
      return build(tr.state, focused);
    }
    return value;
  },
  provide: (f) => EditorView.decorations.from(f, (v) => v.decorations),
});

export function livePreview(): Extension {
  return [previewField, EditorView.focusChangeEffect.of((_state, focusing) => setFocused.of(focusing))];
}
