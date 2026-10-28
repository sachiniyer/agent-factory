import assert from "node:assert/strict";
import { test } from "node:test";
import { readFileSync } from "node:fs";
import { terminalSurface } from "./components.js";
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

// WCAG relative luminance: the fixed ANSI mapping must remain legible on the
// generated terminal surface, including black and bright-black agent output.
const luminance = (hex: string) => channels(hex).map((c) => {
  const v = c / 255;
  return v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4;
}).reduce((sum, c, i) => sum + c * [.2126, .7152, .0722][i], 0);
for (const mode of ["light", "dark"] as const) {
  test(`${mode} ANSI entries remain readable on the token terminal background`, () => {
    const bg = luminance(tokens.colors.surface[mode]);
    for (const [name, hex] of Object.entries(TERMINAL_ANSI[mode])) {
      const fg = luminance(hex!);
      const ratio = (Math.max(bg, fg) + .05) / (Math.min(bg, fg) + .05);
      assert.ok(ratio >= 4.5, `${name}: ${ratio}:1`);
    }
  });
}

test("terminal background and foreground resolve from the generated surface and ink", (t) => {
  const original = Object.getOwnPropertyDescriptor(globalThis, "getComputedStyle");
  t.after(() => {
    if (original) Object.defineProperty(globalThis, "getComputedStyle", original);
    else Reflect.deleteProperty(globalThis, "getComputedStyle");
  });
  for (const mode of ["light", "dark"] as const) {
    Object.defineProperty(globalThis, "getComputedStyle", { configurable: true, value: () => ({
      getPropertyValue: (name: string) => tokens.colors[name.replace("--af-", "")][mode],
    }) });
    const theme = terminalSurface({} as HTMLElement, TERMINAL_ANSI[mode]);
    assert.equal(theme.background, tokens.colors.surface[mode]);
    assert.equal(theme.foreground, tokens.colors.ink[mode]);
    assert.equal(theme.selectionBackground, tokens.colors["surface-raised"][mode]);
    assert.equal(theme.red, TERMINAL_ANSI[mode].red);
  }
});
