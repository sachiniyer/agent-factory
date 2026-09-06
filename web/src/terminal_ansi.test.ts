import assert from "node:assert/strict";
import { test } from "node:test";
import { readFileSync } from "node:fs";
import { TERMINAL_ANSI } from "./terminal_ansi.js";

const tokens = JSON.parse(readFileSync(new URL("../../design/tokens.json", import.meta.url), "utf8"));
const colors = ["Red", "Green", "Yellow", "Blue", "Magenta", "Cyan"] as const;
const channels = (hex: string) => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
for (const mode of ["light", "dark"] as const) {
  test(`${mode} ANSI bright band has eight distinct entries, each different from product ink`, () => {
    const bright = Object.entries(TERMINAL_ANSI[mode]).filter(([name]) => name.startsWith("bright")).map(([, value]) => value!.toLowerCase());
    assert.equal(bright.length, 8);
    assert.equal(new Set(bright).size, 8);
    for (const value of bright) assert.notEqual(value, tokens.colors.ink[mode].toLowerCase());
  });
  test(`${mode} chromatic brights preserve their base hue toward the readable end of the scale`, () => {
    for (const color of colors) {
      const palette = TERMINAL_ANSI[mode] as Record<string, string>;
      const base = channels(palette[color.toLowerCase()]);
      const bright = channels(palette[`bright${color}`]);
      base.forEach((channel, i) => {
        // A fixed 20% blend with black/white preserves hue, with integer rounding.
        const expected = mode === "light" ? channel * .8 : channel + (255 - channel) * .2;
        assert.ok(Math.abs(bright[i] - expected) <= 1, color);
      });
    }
  });
}
