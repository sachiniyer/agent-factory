// Unit tests for the code-fence grammar chunk (#6731): the alias table, the
// span shape highlightRanges returns, and the bounds — unknown language and
// oversized source both mean "plain code", never an error.

import assert from "node:assert/strict";
import { test } from "node:test";
import { highlightRanges, languageFor, MAX_HIGHLIGHT_CHARS } from "./codeblocks_chunk.js";

test("languageFor resolves the curated aliases, case-insensitively", () => {
  for (const [alias, canonical] of [
    ["go", "go"], ["golang", "go"], ["GO", "go"],
    ["js", "js"], ["javascript", "js"], ["node", "js"], ["mjs", "js"],
    ["jsx", "jsx"], ["ts", "ts"], ["typescript", "ts"], ["tsx", "tsx"],
    ["py", "python"], ["python", "python"], ["python3", "python"],
    ["sh", "sh"], ["bash", "sh"], ["zsh", "sh"], ["shell", "sh"],
    ["json", "json"], ["yaml", "yaml"], ["yml", "yaml"],
    ["toml", "toml"], ["html", "html"], ["css", "css"],
    ["sql", "sql"], ["rs", "rust"], ["rust", "rust"],
    ["diff", "diff"], ["patch", "diff"], ["md", "markdown"], ["markdown", "markdown"],
  ] as const) {
    const a = languageFor(alias);
    const b = languageFor(canonical);
    assert.ok(a && b, `${alias}/${canonical} must resolve`);
    assert.equal(a, b, `${alias} must resolve to the same grammar as ${canonical}`);
  }
});

test("languageFor returns null for unknown and empty languages", () => {
  for (const lang of ["", "brainfuck", "cobol", "go2", "json5"]) {
    assert.equal(languageFor(lang), null, lang);
  }
});

// A fence's info string is document text, and every Object.prototype member
// name lowercases onto the map keys — "constructor", "__proto__", "toString".
// The lookup must return null for them, never a truthy non-grammar that
// highlightRanges would then try to parse with.
test("languageFor returns null for Object.prototype member names", () => {
  for (const lang of ["constructor", "__proto__", "toString", "hasOwnProperty", "valueOf", "CONSTRUCTOR"]) {
    assert.equal(languageFor(lang), null, lang);
    assert.equal(highlightRanges("code", lang), null, `${lang} stays plain code`);
  }
});

test("a go block gets keyword, string and comment spans over real text", () => {
  const src = `// hello\npackage main\n\nimport "fmt"\n\nfunc main() {\n\tfmt.Println("hi", 42)\n}\n`;
  const spans = highlightRanges(src, "go");
  assert.ok(spans, "go must highlight");
  const cls = new Map(spans!.map((s) => [src.slice(s.from, s.to), s.cls]));
  assert.equal(cls.get("// hello"), "tok-comment");
  assert.equal(cls.get("package"), "tok-keyword");
  assert.equal(cls.get('"fmt"'), "tok-string");
  assert.equal(cls.get("42"), "tok-number");
});

test("spans are ordered, non-overlapping and inside the source", () => {
  const src = `def f(x):\n    return x + "s"  # tail\n`;
  const spans = highlightRanges(src, "python")!;
  let pos = 0;
  for (const s of spans) {
    assert.ok(s.from >= pos, "spans must not overlap");
    assert.ok(s.from < s.to, "a span must cover text");
    pos = s.to;
  }
  assert.ok(pos <= src.length, "no span may run past the source");
});

test("code that names markup or URLs comes back as plain spans", () => {
  const src = `<img src=x onerror="alert(1)">\n<script>fetch("https://x.invalid")</script>`;
  const spans = highlightRanges(src, "html")!;
  // The output is ranges + class names over the text — the only thing the
  // widget can ever build from it is spans with textContent.
  for (const s of spans) {
    assert.ok(src.slice(s.from, s.to).length > 0);
    assert.match(s.cls, /^tok-[\w-]+(?: tok-[\w-]+)*$/);
  }
});

test("unknown and oversized blocks return null, not an error", () => {
  assert.equal(highlightRanges("code", "brainfuck"), null);
  assert.equal(highlightRanges("x".repeat(MAX_HIGHLIGHT_CHARS + 1), "go"), null);
  assert.ok(highlightRanges("x".repeat(64), "go") !== null, "a small block highlights");
});

test("a diff block marks inserted and deleted lines", () => {
  const src = `--- a/f\n+++ b/f\n@@ -1 +1 @@\n-old\n+new\n`;
  const spans = highlightRanges(src, "diff")!;
  const cls = new Map(spans.map((s) => [src.slice(s.from, s.to), s.cls]));
  assert.equal(cls.get("-old"), "tok-deleted");
  assert.equal(cls.get("+new"), "tok-inserted");
});
