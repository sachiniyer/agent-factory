const assert = require("node:assert/strict");
const test = require("node:test");
const {
  goBehaviorProjection,
  goSourcesEquivalent,
  isOrdinaryGoLineComment,
  patchTouchesOnlyCommentLines,
} = require("./go-inert.js");

const file = (body) => `package ui\n\n${body}\n`;
// Enough lines that a hunk's three lines of context cannot reach the backticks.
const filler = Array.from({ length: 6 }, (_, i) => `row ${i}`).join("\n");

// Each canary is a change that LOOKS like a comment edit and changes what the
// toolchain builds. Every one must keep the play-test requirement (#4477).
const MUST_STAY_GATED = {
  "a //go:embed pattern": [
    file('import _ "embed"\n\n//go:embed help.txt\nvar help string'),
    file('import _ "embed"\n\n//go:embed usage.txt\nvar help string'),
  ],
  "a //go:build constraint": ["//go:build linux\n\n" + file("func F() {}"), "//go:build !linux\n\n" + file("func F() {}")],
  "a legacy // +build constraint": ["// +build linux\n\n" + file("func F() {}"), "// +build darwin\n\n" + file("func F() {}")],
  "adding a //go:generate directive": [file("func F() {}"), file("//go:generate stringer -type=T\nfunc F() {}")],
  "a //line directive": [file("//line a.go:10\nfunc F() {}"), file("//line a.go:20\nfunc F() {}")],
  "a /*line*/ directive": [file("/*line a.go:10*/ func F() {}"), file("/*line a.go:20*/ func F() {}")],
  "any block comment": [file("/* one */\nfunc F() {}"), file("/* two */\nfunc F() {}")],
  "a struct tag": [file('type T struct {\n\tA int `json:"a"`\n}'), file('type T struct {\n\tA int `json:"b"`\n}')],
  "an interpreted string holding //": [file('const U = "http://a"'), file('const U = "http://b"')],
  // Misreading '"' as anything but a rune would open a string there, and the
  // `// a` inside the real string below would then look like a comment.
  "a string after a rune literal holding a quote": [
    file("const Q = '\"'\nconst S = \"// a\""),
    file("const Q = '\"'\nconst S = \"// b\""),
  ],
  "a // line inside a raw string, far from its backticks": [
    file(`const Help = \`\n${filler}\n// shown to the user\n${filler}\n\``),
    file(`const Help = \`\n${filler}\n// SHOWN to the user\n${filler}\n\``),
  ],
  "a blank line inside a raw string": [
    file(`const Help = \`\n${filler}\n${filler}\n\``),
    file(`const Help = \`\n${filler}\n\n${filler}\n\``),
  ],
  "a cgo preamble comment": [
    file('// #include "a.h"\nimport "C"'),
    file('// #include "b.h"\nimport "C"'),
  ],
  "a cgo import spelled with an escape": [
    file('// #include "a.h"\nimport "\\x43"'),
    file('// #include "b.h"\nimport "\\x43"'),
  ],
  "a cgo import after a very long import name": [
    file(`// #include "a.h"\nimport ${"x".repeat(300)}func "C"`),
    file(`// #include "b.h"\nimport ${"x".repeat(300)}func "C"`),
  ],
  "commenting out a statement": [file("func F() {\n\tg()\n}"), file("func F() {\n\t// g()\n}")],
  "commented-out code with no space": [file("//default:\nfunc F() {}"), file("//case 1:\nfunc F() {}")],
  "a lint pragma": [file("func F() {} //nolint:errcheck"), file("func F() {} //nolint:unused")],
  "joining two lines across a removed comment": [
    file("func F() int {\n\treturn // why\n\t1\n}"),
    file("func F() int {\n\treturn 1\n}"),
  ],
  "splitting a token with a space": [file("var x = a+ +b"), file("var x = a++b")],
  "gluing tokens where a space was": [file("var x = a - -b"), file("var x = a --b")],
  "a changed spaced // go: prose line": [file("// go:embed a\nfunc F() {}"), file("// go:embed b\nfunc F() {}")],
  "an import comment": [
    'package ui // import "example.com/a"\n',
    'package ui // import "example.com/b"\n',
  ],
};

for (const [name, [before, after]] of Object.entries(MUST_STAY_GATED)) {
  test(`#4477 canary: ${name} is not inert`, () => {
    assert.notEqual(before, after, "the fixture must actually change something");
    assert.equal(goSourcesEquivalent(before, after), false);
  });
}

// Shapes that change nothing the toolchain reads.
const MAY_SKIP = {
  "a reworded doc comment": [file("// Old doc.\nfunc F() {}"), file("// New doc,\n// now two lines.\nfunc F() {}")],
  "a deleted comment and blank line": [file("// Doc.\n\nfunc F() {}"), file("func F() {}")],
  "an edited trailing comment": [file("var x = 1 // old"), file("var x = 1 // new")],
  "an empty // line": [file("//\n// Doc.\nfunc F() {}"), file("// Doc.\nfunc F() {}")],
  "tab-indented example in a doc comment": [file("//\tf(x)\nfunc F() {}"), file("//\tf(y)\nfunc F() {}")],
  "a comment holding quotes and backticks": [
    file("// don't \"panic\" on `af`\nfunc F() {}"),
    file("// don't \"panic\" on `af` again\nfunc F() {}"),
  ],
  "a comment holding a block-comment opener": [file("// see /* here\nfunc F() {}"), file("// see /* there\nfunc F() {}")],
  "an indentation-only change": [file("func F() {\n\tg()\n}"), file("func F() {\n        g()\n}")],
  "CRLF line endings": [file("// a\nfunc F() {}"), file("// b\nfunc F() {}").replace(/\n/g, "\r\n")],
  "an unchanged raw string with a comment edit outside it": [
    file(`// old\nconst Help = \`\n// verbatim\n\``),
    file(`// new\nconst Help = \`\n// verbatim\n\``),
  ],
  "a directive left alone while prose around it changes": [
    "//go:build linux\n\n// Package ui draws.\npackage ui\n",
    "//go:build linux\n\n// Package ui renders.\npackage ui\n",
  ],
  "a string \"C\" outside the import section": [file('func F() string { return "C" } // x'), file('func F() string { return "C" } // y')],
};

for (const [name, [before, after]] of Object.entries(MAY_SKIP)) {
  test(`#4477: ${name} is inert`, () => {
    assert.notEqual(before, after);
    assert.equal(goSourcesEquivalent(before, after), true);
  });
}

test("#4477: files the projection cannot reason about are never inert", () => {
  for (const source of [
    file('const s = "unterminated'),
    file("const s = `unterminated"),
    file("/* unterminated"),
    file("const r = '\n'"),
    file('const s = "a\\\nb"'),
    file("func F() {}\u0000"),
    "\uFEFF" + file("func F() {}"),
  ]) {
    assert.equal(goBehaviorProjection(source), null, JSON.stringify(source));
    assert.equal(goSourcesEquivalent(source, source), false, "null must not compare equal to itself");
  }
  assert.equal(goBehaviorProjection(undefined), null);
});

test("#4477: a directive-shaped comment is never ordinary prose", () => {
  for (const text of ["//go:build x", "//line a.go:1", "//export F", "//nolint", "//x", "// +build x", "// go:embed x", '// import "a"', "//\r"]) {
    assert.equal(isOrdinaryGoLineComment(text), false, text);
  }
  for (const text of ["//", "// prose", "//\tcode sample", "//  +not a constraint? still +build", "// importing is fine"]) {
    assert.equal(isOrdinaryGoLineComment(text), true, text);
  }
});

// The prefilter only decides whether the blob reads are worth making. It must
// accept the witness shape and reject anything that is not a whole-line comment.
test("#4477: the patch prefilter passes only whole-line comment edits", () => {
  const witness = [
    "@@ -1,4 +1,5 @@",
    " package app",
    " ",
    "-// The TUI drives every daemon control + read call over the HTTP API client",
    "+// The session, task, project, tab, preview, and snapshot seams in this file use",
    "+// the HTTP API client (#1592 Phase 2 PR3). Each builds a fresh apiclient.Client",
    " ",
    "",
  ].join("\n");
  assert.equal(patchTouchesOnlyCommentLines(witness), true);
  assert.equal(patchTouchesOnlyCommentLines("@@ -1 +1,2 @@\n x\n+\n+\t// note"), true);

  for (const patch of [
    undefined,
    "",
    "@@ -1 +1 @@\n x",
    "@@ -1 +1 @@\n-x := 1 // old\n+x := 1 // new",
    "@@ -1 +1 @@\n-// a\n+//go:embed b",
    "@@ -1 +1 @@\n-// a\n+// +build b",
    "@@ -1 +1 @@\n-// a\n+/* b */",
    "@@ -1 +1 @@\n-// a\n+// b\n\\ No newline at end of file",
    "diff --git a/x b/x\n@@ -1 +1 @@\n-// a\n+// b",
    "@@ -1 +1 @@\n--flag  help text\n+// b",
  ]) {
    assert.equal(patchTouchesOnlyCommentLines(patch), false, JSON.stringify(patch));
  }
});
