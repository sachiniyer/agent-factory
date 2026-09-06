// All product chrome is migrated; generated tokens are the only color source.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const migrated = /./;
const tokens = readFileSync(new URL("./tokens.css", import.meta.url), "utf8");
const tokenNames = new Set([...tokens.matchAll(/(--af-[\w-]+):/g)].map((m) => m[1]));
// Fonts and stacking levels are fixed structural rules, outside the 23-token budget.
const structural = new Set(["--af-font-ui", "--af-font-mono", "--af-z-appbar", "--af-z-drawer", "--af-z-popover", "--af-z-modal"]);

function violations(css: string): string[] {
  const errors: string[] = [];
  const source = css.replace(/\/\*[\s\S]*?\*\//g, "");
  for (const [, selector, body] of source.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    if (!migrated.test(selector)) continue;
    for (const declaration of body.split(";")) {
      const colon = declaration.indexOf(":");
      if (colon < 0) continue;
      const property = declaration.slice(0, colon).trim();
      const value = declaration.slice(colon + 1).trim();
      // No disguised literals in shadows, gradients, fallbacks or private variables.
      if (/#(?:[\da-f]{3,8})\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color|color-mix)\s*\(/i.test(value)) {
        errors.push(`${selector.trim()}: ${property}: literal color`);
      }
      if (/^(?:color|background(?:-color)?|border(?:-(?:top|right|bottom|left))?(?:-color)?|outline(?:-color)?|fill|stroke)$/.test(property)
        && /[a-z]/i.test(value.replace(/var\([^)]*\)/g, "").replace(/\b(?:solid|dashed|none|transparent|currentColor|inherit|unset|initial|revert|calc|important)\b/g, ""))) {
        errors.push(`${selector.trim()}: ${property}: named color`);
      }
      if (/-?\d*\.?\d+(?:px|rem|em)\b/.test(value)) errors.push(`${selector.trim()}: ${property}: literal metric`);
      for (const [, name] of value.matchAll(/var\((--[\w-]+)/g)) {
        if (!tokenNames.has(name) && !structural.has(name)) errors.push(`${selector.trim()}: ${name}: legacy token`);
      }
    }
  }
  return errors;
}

test("all chrome resolves colors and metrics through generated design tokens", () => {
  assert.deepEqual(violations(readFileSync(new URL("./styles.css", import.meta.url), "utf8")), []);
});

test("chrome token lint rejects nested, named and fallback colors and legacy metrics", () => {
  for (const value of ["#fff", "red", "beige", "rgb(1 2 3)", "var(--af-ink, #fff)", "linear-gradient(#fff, #000)", "var(--af-text)", "12px"]) {
    assert.notEqual(violations(`@media (max-width: 768px) { .af-rail { background: ${value}; } }`).length, 0, value);
  }
  assert.deepEqual(violations('.af-rail { color: var(--af-ink); padding: var(--af-space-2); }'), []);
});
