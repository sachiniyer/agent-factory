import assert from "node:assert/strict";
import { test } from "node:test";
import { EditorSelection, EditorState } from "@codemirror/state";
import { editorExtensions } from "./docs_editor.js";
import { activeLines, parseTable, type PreviewSpec, previewSpecs, specsToDecorations } from "./md_live_preview.js";

function state(doc: string, cursor = doc.length): EditorState {
  return EditorState.create({ doc, selection: EditorSelection.cursor(cursor), extensions: editorExtensions() });
}

function specsFor(doc: string, cursor?: number): { specs: PreviewSpec[]; s: EditorState } {
  const s = state(doc, cursor);
  return { specs: previewSpecs(s, activeLines(s)), s };
}

function hidden(s: EditorState, specs: PreviewSpec[]): string[] {
  return specs.filter((x) => x.type === "hide").map((x) => s.doc.sliceString(x.from, x.to));
}

function lineClasses(s: EditorState, specs: PreviewSpec[]): Map<number, string[]> {
  const out = new Map<number, string[]>();
  for (const x of specs) {
    if (x.type !== "line") continue;
    const n = s.doc.lineAt(x.pos).number;
    out.set(n, [...(out.get(n) ?? []), x.cls]);
  }
  return out;
}

const tail = "\n\nend";

test("a heading away from the cursor hides its marks and styles the line", () => {
  const { specs, s } = specsFor(`## Endpoints${tail}`);
  assert.deepEqual(hidden(s, specs), ["## "]);
  assert.deepEqual(lineClasses(s, specs).get(1), ["cm-md-h cm-md-h2"]);
});

test("the line with the cursor shows its source but keeps its style", () => {
  const { specs, s } = specsFor(`## Endpoints${tail}`, 3);
  assert.deepEqual(hidden(s, specs), []);
  assert.deepEqual(lineClasses(s, specs).get(1), ["cm-md-h cm-md-h2"]);
});

test("emphasis, strong, code and strikethrough hide their marks off the cursor line", () => {
  const { specs, s } = specsFor(`a *b* **c** \`d\` ~~e~~${tail}`);
  assert.deepEqual(hidden(s, specs), ["*", "*", "**", "**", "`", "`", "~~", "~~"]);
  const marks = specs.filter((x) => x.type === "mark").map((x) => x.type === "mark" && x.cls);
  assert.deepEqual(marks, ["cm-md-em", "cm-md-strong", "cm-md-code", "cm-md-strike"]);
});

test("a link shows only its text off the cursor line", () => {
  const doc = `see [the docs](https://example.com) now${tail}`;
  const { specs, s } = specsFor(doc);
  assert.deepEqual(hidden(s, specs), ["[", "](https://example.com)"]);
  const link = specs.find((x) => x.type === "mark" && x.cls === "cm-md-link");
  assert.ok(link && link.type === "mark");
  assert.equal(s.doc.sliceString(link.from, link.to), "the docs");
});

test("bullet marks become bullets off the cursor line; ordered marks stay", () => {
  const { specs, s } = specsFor(`- one\n- two\n\n1. first${tail}`, 0);
  const bullets = specs.filter((x) => x.type === "bullet").map((x) => s.doc.lineAt(x.from).number);
  assert.deepEqual(bullets, [2], "line 1 holds the cursor and keeps its dash");
  assert.ok(specs.some((x) => x.type === "mark" && x.cls === "cm-md-listmark" && s.doc.lineAt(x.from).number === 4));
});

test("a question callout styles every line and replaces its tag with a label", () => {
  const doc = `> [!question] Should tokens expire?\n> They do today.${tail}`;
  const { specs, s } = specsFor(doc);
  const lines = lineClasses(s, specs);
  assert.deepEqual(lines.get(1), ["cm-callout cm-callout-question cm-callout-first"]);
  assert.deepEqual(lines.get(2), ["cm-callout cm-callout-question cm-callout-last"]);
  const label = specs.find((x) => x.type === "callout-label");
  assert.ok(label && label.type === "callout-label");
  assert.equal(label.kind, "question");
  assert.equal(s.doc.sliceString(label.from, label.to), "[!question] ");
  assert.deepEqual(hidden(s, specs), ["> ", "> "]);
});

test("todo and decision callouts get their own classes", () => {
  for (const kind of ["todo", "decision"] as const) {
    const { specs, s } = specsFor(`> [!${kind}] Text${tail}`);
    assert.deepEqual(lineClasses(s, specs).get(1), [`cm-callout cm-callout-${kind} cm-callout-first cm-callout-last`]);
  }
});

test("a callout line with the cursor shows its tag", () => {
  const { specs } = specsFor(`> [!todo] Write it${tail}`, 4);
  assert.equal(specs.filter((x) => x.type === "callout-label").length, 0);
});

test("a plain quote is a quote, not a callout", () => {
  const { specs, s } = specsFor(`> just a quote${tail}`);
  assert.deepEqual(lineClasses(s, specs).get(1), ["cm-md-quote"]);
  assert.equal(specs.filter((x) => x.type === "callout-label").length, 0);
});

test("a table away from the cursor renders as one table widget", () => {
  const doc = `| Route | Auth |\n| :--- | ---: |\n| /a | yes |\n| /b | no |${tail}`;
  const { specs, s } = specsFor(doc);
  const tables = specs.filter((x) => x.type === "table");
  assert.equal(tables.length, 1);
  const t = tables[0]!;
  assert.ok(t.type === "table");
  assert.equal(s.doc.lineAt(t.from).number, 1);
  assert.equal(s.doc.lineAt(t.to).number, 4);
  assert.deepEqual(t.rows, [["Route", "Auth"], ["/a", "yes"], ["/b", "no"]]);
  assert.deepEqual(t.align, ["left", "right"]);
});

test("a table that holds the cursor shows its source", () => {
  const doc = `| A | B |\n| - | - |\n| 1 | 2 |${tail}`;
  const { specs, s } = specsFor(doc, 2);
  assert.equal(specs.filter((x) => x.type === "table").length, 0);
  assert.deepEqual(lineClasses(s, specs).get(3), ["cm-md-table-source"]);
});

test("a fenced block away from the cursor becomes one codeblock spec", () => {
  const doc = "```\n## not a heading\n*no*\n```" + tail;
  const { specs, s } = specsFor(doc);
  const blocks = specs.filter((x) => x.type === "codeblock");
  assert.equal(blocks.length, 1);
  const b = blocks[0]!;
  assert.ok(b.type === "codeblock");
  assert.equal(b.lang, "");
  assert.equal(b.source, "## not a heading\n*no*");
  assert.equal(s.doc.lineAt(b.from).number, 1);
  assert.equal(s.doc.lineAt(b.to).number, 4, "the widget replaces the whole fence, markers included");
  assert.equal(lineClasses(s, specs).size, 0, "the fence's lines are not styled as a code block");
});

test("a mermaid fence away from the cursor becomes one mermaid spec with its source", () => {
  const doc = "```mermaid\nflowchart LR\n  A-->B\n```" + tail;
  const { specs, s } = specsFor(doc);
  const mermaids = specs.filter((x) => x.type === "mermaid");
  assert.equal(mermaids.length, 1);
  const m = mermaids[0]!;
  assert.ok(m.type === "mermaid");
  assert.equal(m.source, "flowchart LR\n  A-->B");
  assert.equal(s.doc.lineAt(m.from).number, 1);
  assert.equal(s.doc.lineAt(m.to).number, 4);
  assert.equal(lineClasses(s, specs).size, 0, "the fence's lines are not styled as a code block");
});

test("a mermaid fence that holds the cursor shows its source", () => {
  const doc = "```mermaid\nflowchart LR\n  A-->B\n```" + tail;
  const { specs, s } = specsFor(doc, doc.indexOf("-->"));
  assert.equal(specs.filter((x) => x.type === "mermaid").length, 0);
  assert.deepEqual(lineClasses(s, specs).get(2), ["cm-md-codeblock"]);
});

test("the mermaid info string tolerates case, spacing and the tilde fence", () => {
  for (const open of ["```Mermaid", "``` mermaid", "~~~mermaid"]) {
    const { specs } = specsFor(`${open}\ngraph TD\n  A-->B\n${open[0]}${open[0]}${open[0]}${tail}`);
    assert.equal(specs.filter((x) => x.type === "mermaid").length, 1, open);
  }
});

test("a fenced block carries its language, first word of the info string", () => {
  const doc = "```go\nfunc main() {}\n```" + tail;
  const { specs } = specsFor(doc);
  const blocks = specs.filter((x) => x.type === "codeblock");
  assert.equal(blocks.length, 1);
  assert.ok(blocks[0]!.type === "codeblock");
  assert.equal(blocks[0]!.lang, "go");
  assert.equal(blocks[0]!.source, "func main() {}");
});

test("a fenced block that holds the cursor shows its source", () => {
  const doc = "```go\nfunc main() {}\n```" + tail;
  const { specs, s } = specsFor(doc, doc.indexOf("main"));
  assert.equal(specs.filter((x) => x.type === "codeblock").length, 0);
  assert.deepEqual(lineClasses(s, specs).get(1), ["cm-md-codeblock"]);
  assert.deepEqual(lineClasses(s, specs).get(3), ["cm-md-codeblock"], "the markers reappear on the cursor's lines");
});

test("an unknown or extra-word language still renders as a code block", () => {
  for (const [fence, want] of [["```brainfuck", "brainfuck"], ["``` go linenos", "go"], ["```GO", "go"]] as const) {
    const { specs } = specsFor(`${fence}\nx\n\`\`\`${tail}`);
    const b = specs.filter((x) => x.type === "codeblock")[0];
    assert.ok(b && b.type === "codeblock", fence);
    assert.equal(b.lang, want, fence);
  }
});

test("an indented code block stays source-styled", () => {
  const doc = "para\n\n    indented code\n    more" + tail;
  const { specs, s } = specsFor(doc);
  assert.equal(specs.filter((x) => x.type === "codeblock").length, 0);
  assert.deepEqual(lineClasses(s, specs).get(3), ["cm-md-codeblock"]);
});

test("an empty mermaid fence still becomes a mermaid spec", () => {
  const { specs } = specsFor("```mermaid\n```" + tail);
  const m = specs.filter((x) => x.type === "mermaid")[0];
  assert.ok(m && m.type === "mermaid" && m.source === "");
});

test("parseTable keeps escaped pipes inside a cell", () => {
  assert.deepEqual(parseTable(["a | b", "--|--", "x \\| y | z"]).rows, [["a", "b"], ["x | y", "z"]]);
});

test("the specs build a valid decoration set", () => {
  const doc = `# Title\n\n> [!question] Why?\n\n- item with [link](u) and **bold**\n\n| a | b |\n|---|---|\n| 1 | 2 |${tail}`;
  const { specs } = specsFor(doc);
  const set = specsToDecorations(specs);
  assert.ok(set.size > 0);
});
