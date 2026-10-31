import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const css = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "styles.css"), "utf8");

/** The declarations of every rule whose selector list contains `selector`. */
function rules(selector: string): string[] {
  const out: string[] = [];
  for (const m of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selectors = m[1]!.replace(/\/\*[\s\S]*?\*\//g, "").split(",").map((s) => s.trim());
    if (selectors.includes(selector)) out.push(m[2]!);
  }
  return out;
}

test("the focused split pane draws its ring above the terminal and the Docs editor", () => {
  const ring = rules(".af-kb-terminal .af-split-multi .af-pane-focused::after").join("");
  assert.match(ring, /position:\s*absolute/);
  assert.match(ring, /inset:\s*0/);
  assert.match(ring, /pointer-events:\s*none/);
  assert.match(ring, /border:[^;]*var\(--af-accent\)/);
  assert.match(rules(".af-pane-host").join(""), /isolation:\s*isolate/, "pane content must not paint over the ring");
  assert.equal(rules(".af-kb-terminal .af-split-multi .af-pane-focused").length, 0, "an outline is hidden by pane content");
});
