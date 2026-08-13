import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const srcRoot = dirname(fileURLToPath(import.meta.url));

test("web components contain no raw colors outside the theme mechanism", () => {
  const files = readdirSync(srcRoot)
    .filter((name) => name.endsWith(".ts") && !name.endsWith(".test.ts") && name !== "theme.ts")
    .sort();
  const rawColor = /["'`]#(?:[\da-fA-F]{3}|[\da-fA-F]{4}|[\da-fA-F]{6}|[\da-fA-F]{8})["'`]|rgba?\(/g;
  const violations: string[] = [];
  for (const file of files) {
    const source = readFileSync(join(srcRoot, file), "utf8");
    for (const match of source.matchAll(rawColor)) {
      const line = source.slice(0, match.index).split("\n").length;
      violations.push(`${file}:${line}: ${match[0]}`);
    }
  }
  assert.deepEqual(violations, [], "component colors must resolve through --af-* semantic tokens");
});

test("visible headings keep their authored sentence case", () => {
  const css = readFileSync(join(srcRoot, "styles.css"), "utf8");
  assert.doesNotMatch(css, /text-transform:\s*uppercase/, "CSS must not turn interface copy into CAPS");
});

test("component CSS uses semantic tokens for every color", () => {
  const css = readFileSync(join(srcRoot, "styles.css"), "utf8");
  const rawComponentColor = /^\s*(?!--af-)[\w-]+\s*:\s*(?:#[\da-fA-F]{3,8}|rgba?\()/gm;
  assert.doesNotMatch(css, rawComponentColor, "component colors must resolve through --af-* semantic tokens");
});
