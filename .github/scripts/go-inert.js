"use strict";

// Whether a change to a Go source file provably cannot change the program (#4477).
//
// The TUI gate demands an attestation that someone drove the TUI and looked, and
// a diff that only rewords comments leaves nothing to look at. The dangerous
// mistake is the opposite one: calling a change inert when it is not. A false
// "needs a play-test" costs one attestation; a false "inert" ships an unverified
// TUI change and turns the gate into a formality. So everything here resolves
// doubt toward "not inert", and the proof works on WHOLE files rather than on a
// diff. A hunk cannot say whether its lines sit inside a raw string literal —
// the opening backtick can be any distance above it — and a `//` line inside one
// is string content, not a comment.
//
// The proof: project each file to what the Go toolchain can observe, and compare.
// The projection is the source text with every run of whitespace and ordinary
// line comments collapsed to one separator — a newline if the run held one, else
// a space — and everything else kept byte-for-byte. The Go scanner treats such a
// run exactly that way: a line comment acts as a newline, and whitespace matters
// only as a token boundary and, through a newline, for semicolon insertion. So
// equal projections mean equal token streams, without this file having to
// tokenize Go correctly — it only has to find comments and literals where the
// Go scanner finds them, which is five start sequences (`//`, `/*`, `"`, `'`,
// a backtick) and their ends.
//
// Kept verbatim, so any change to them keeps the gate:
//   - every string, rune and raw string literal — struct tags included;
//   - every `/* */` comment. `/*line` is a directive, and no gated file uses the
//     block form today, so treating all of them as significant costs nothing;
//   - every line comment that is not plain prose: `//` followed by anything
//     other than a space or tab (`//go:build`, `//go:embed`, `//line`,
//     `//export`, `//nolint`, commented-out code), plus the space-led forms
//     that tools still read — `// +build`, a spaced `// go:` and an import
//     comment (`// import "…"`).
// And a file that imports "C" is never inert: every comment above that import
// is C source for cgo.
//
// Measured before this landed, over the 845 master commits that touched a gated
// production file (2,189 modified files): no file this calls inert differs from
// its parent in go/scanner's token stream, and one inert file is called
// significant (commented-out code with no space after `//`).

// Top-level declarations that end the import section. Imports must precede all
// of them, so a string literal after one of these is not an import path.
const GO_DECLARATION_KEYWORDS = new Set(["const", "func", "type", "var"]);
// Sticky, so a match starts exactly at lastIndex and covers the whole word. A
// windowed match can split a long identifier and read its tail as `func`.
const GO_WORD = /[\p{L}\p{Nd}_]+/uy;

// A line comment the toolchain does not read. `text` starts at the `//` and
// runs to the end of the line.
function isOrdinaryGoLineComment(text) {
  const body = text.slice(2);
  return (
    (body === "" || /^[ \t]/.test(body)) &&
    !/^[ \t]*(?:\+build|go:|import[ \t]*["`])/.test(body)
  );
}

// Whether an import path literal names cgo's pseudo-package. Import paths are
// unquoted before use, so `"\x43"` is "C" too; one character or one escape is
// the only way to spell a one-byte string.
function goLiteralIsC(literal) {
  const body = literal.slice(1, -1);
  if (literal[0] === "`") return body.replace(/\r/g, "") === "C";
  return body === "C" || /^\\(?:x43|103|u0043|U00000043)$/.test(body);
}

// The toolchain-observable projection of a Go file, or null when the file is
// not one this can reason about: an unterminated literal or block comment, a
// newline inside a one-line literal, a NUL or byte-order mark anywhere, or a
// cgo import. null never compares equal to anything, so it keeps the gate.
function goBehaviorProjection(source) {
  if (typeof source !== "string" || /[\u0000\uFEFF]/.test(source)) return null;
  let out = "";
  // The separator owed before the next kept text: "", " ", or "\n".
  let separator = "";
  let importSection = true;
  const keep = (text) => {
    // Leading and trailing separators are dropped: the scanner inserts a
    // semicolon at end of file whether or not a newline precedes it.
    if (out && separator) out += separator;
    out += text;
    separator = "";
  };
  const n = source.length;
  let i = 0;
  while (i < n) {
    const ch = source[i];
    if (ch === " " || ch === "\t" || ch === "\r" || ch === "\n") {
      if (ch === "\n") separator = "\n";
      else if (!separator) separator = " ";
      i += 1;
    } else if (ch === "/" && source[i + 1] === "/") {
      const newline = source.indexOf("\n", i);
      const end = newline < 0 ? n : newline;
      const text = source.slice(i, end);
      if (!isOrdinaryGoLineComment(text)) keep(text);
      else if (!separator) separator = " ";
      // The newline that ends the comment is scanned next and owes the "\n".
      i = end;
    } else if (ch === "/" && source[i + 1] === "*") {
      const end = source.indexOf("*/", i + 2);
      if (end < 0) return null;
      keep(source.slice(i, end + 2));
      i = end + 2;
    } else if (ch === '"' || ch === "'") {
      let j = i + 1;
      while (j < n && source[j] !== ch) {
        if (source[j] === "\n") return null;
        if (source[j] === "\\") {
          // Skipping the escaped character is enough to find the closing
          // quote: no escape's later characters are quotes or newlines.
          if (j + 1 >= n || source[j + 1] === "\n") return null;
          j += 1;
        }
        j += 1;
      }
      if (j >= n) return null;
      const literal = source.slice(i, j + 1);
      if (ch === '"' && importSection && goLiteralIsC(literal)) return null;
      keep(literal);
      i = j + 1;
    } else if (ch === "`") {
      const end = source.indexOf("`", i + 1);
      if (end < 0) return null;
      const literal = source.slice(i, end + 1);
      if (importSection && goLiteralIsC(literal)) return null;
      keep(literal);
      i = end + 1;
    } else {
      GO_WORD.lastIndex = i;
      const word = GO_WORD.exec(source)?.[0];
      if (word) {
        if (GO_DECLARATION_KEYWORDS.has(word)) importSection = false;
        keep(word);
        i += word.length;
      } else {
        // Operators and anything the scanner would reject are kept as-is.
        // Their spelling never matters here, only that it is preserved.
        keep(ch);
        i += 1;
      }
    }
  }
  return out;
}

// Whether `before` and `after` are the same program to the Go toolchain.
function goSourcesEquivalent(before, after) {
  const projected = goBehaviorProjection(before);
  return projected !== null && projected === goBehaviorProjection(after);
}

// A cheap necessary condition, read from a pulls.listFiles `patch`: every added
// or removed line is blank or a whole-line ordinary comment. It exists only to
// avoid reading blobs for the common case — a real code change — and it cannot
// make anything inert on its own: goSourcesEquivalent still decides. It is
// deliberately stricter than the proof, so an edited trailing comment or a
// gofmt realignment on a code line keeps the gate.
function patchTouchesOnlyCommentLines(patch) {
  if (typeof patch !== "string" || !patch.startsWith("@@")) return false;
  let changed = 0;
  for (const line of patch.split("\n")) {
    if (line.startsWith("+") || line.startsWith("-")) {
      const text = line.slice(1).replace(/^[ \t]+/, "").replace(/[ \t\r]+$/, "");
      if (text !== "" && !(text.startsWith("//") && isOrdinaryGoLineComment(text))) return false;
      changed += 1;
    } else if (!line.startsWith("@@") && !line.startsWith(" ") && line !== "") {
      // "\ No newline at end of file", or a shape this does not know.
      return false;
    }
  }
  return changed > 0;
}

module.exports = {
  goBehaviorProjection,
  goSourcesEquivalent,
  isOrdinaryGoLineComment,
  patchTouchesOnlyCommentLines,
};
