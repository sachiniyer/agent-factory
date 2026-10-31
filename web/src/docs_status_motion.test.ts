import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const css = readFileSync(new URL("./styles.css", import.meta.url), "utf8");

// The saving glyph spins, and prefers-reduced-motion must stop it. The reduced
// motion rule has to carry the animated rule's own selector: a shorter one
// loses on specificity and the glyph keeps spinning (found in the #6292
// qualification run).
test("the Docs save glyph's reduced-motion rule is as specific as the rule that spins it", () => {
  const spin = css.match(/(\.af-docs-status:is\([^)]*\) \.af-icon) \{[^}]*animation: af-working-spin/);
  assert.ok(spin, "the animated save-status rule exists");
  const reduced = css.match(/@media \(prefers-reduced-motion: reduce\) \{\s*([^{]+)\{\s*animation: none;/g) ?? [];
  assert.ok(reduced.some((block) => block.includes(spin[1])), "a reduced-motion block disables the spin with the same selector");
});

test("the save glyph and the working dot share one af-working-spin keyframes rule", () => {
  const css = readFileSync(new URL("./styles.css", import.meta.url), "utf8");
  assert.equal(css.match(/@keyframes af-working-spin/g)?.length, 1, "a second definition would silently override the first");
  assert.match(css, /\.af-dot-working \.af-icon \{[^}]*animation: af-working-spin/);
  assert.match(css, /\.af-docs-status:is\([^)]*\) \.af-icon \{[^}]*animation: af-working-spin/);
});
