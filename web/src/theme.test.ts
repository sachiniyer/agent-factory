// Unit coverage for the theme-color collapse rule (#1826/#1761 audit item).
//
// index.html ships one theme-color meta per scheme and the browser paints its chrome
// with whichever matches the OS. That is right for Auto and wrong for an explicit
// choice, so themeColorMetaContents collapses both metas to one colour when the user
// overrides. The rule is pure and pinned here; that the metas are actually rewritten
// in a live document is asserted by the Playwright selftest.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { contrastRatio, deriveTheme, NORD_THEME, themeColorMetaContents } from "./theme.js";

// The --af-bg-surface pair from styles.css: the appbar fill the browser chrome abuts.
const LIGHT = deriveTheme(NORD_THEME, "light").tokens["--af-bg-surface"];
const DARK = deriveTheme(NORD_THEME, "dark").tokens["--af-bg-surface"];

test("Auto keeps the metas per-scheme, so the media queries still decide", () => {
  assert.deepEqual(themeColorMetaContents("auto"), { light: LIGHT, dark: DARK });
});

test("an explicit Dark paints the chrome dark even on a light OS", () => {
  // The bug this exists to prevent: the media-query metas follow the OS, so without
  // the collapse a user who picks Dark on a light OS gets a white chrome capping a
  // dark app. Both metas carry the dark colour, so whichever the browser matches, it
  // paints dark.
  assert.deepEqual(themeColorMetaContents("dark"), { light: DARK, dark: DARK });
});

test("an explicit Light paints the chrome light even on a dark OS", () => {
  assert.deepEqual(themeColorMetaContents("light"), { light: LIGHT, dark: LIGHT });
});

test("the collapsed value is one of the per-scheme values, never a third colour", () => {
  // Cheap guard against a future edit introducing a bespoke "explicit" colour that
  // drifts from the tokens the app actually paints with.
  const auto = themeColorMetaContents("auto");
  for (const choice of ["light", "dark"] as const) {
    const { light, dark } = themeColorMetaContents(choice);
    assert.equal(light, dark, `${choice} must collapse both metas to one colour`);
    assert.ok([auto.light, auto.dark].includes(light), `${choice} produced a colour outside the token pair`);
  }
});

test("Nord drives both modes from one semantic source", () => {
  const dark = deriveTheme(NORD_THEME, "dark");
  const light = deriveTheme(NORD_THEME, "light");

  assert.equal(dark.tokens["--af-bg-canvas"], NORD_THEME.background);
  assert.equal(dark.tokens["--af-bg-surface"], NORD_THEME.background_subtle);
  assert.equal(dark.tokens["--af-bg-raised"], NORD_THEME.background_panel);
  assert.equal(light.tokens["--af-bg-canvas"], NORD_THEME.foreground);
  assert.equal(light.tokens["--af-text"], NORD_THEME.background);
  assert.equal(light.tokens["--af-accent"], "#306979", "the light derivation keeps Nord's frost-cyan hue");
  assert.notEqual(light.tokens["--af-accent"], dark.tokens["--af-accent"]);

  assert.equal(dark.xterm.cyan, dark.tokens["--af-accent"]);
  assert.equal(dark.xterm.green, dark.tokens["--af-term-green"]);
  assert.equal(dark.xterm.yellow, dark.tokens["--af-term-amber"]);
  assert.equal(dark.xterm.blue, dark.tokens["--af-term-blue"]);
});

test("the no-JavaScript CSS floor matches the derived Nord tokens exactly", () => {
  const css = readFileSync(new URL("./styles.css", import.meta.url), "utf8");
  const block = (pattern: RegExp): Map<string, string> => {
    const body = css.match(pattern)?.[1];
    assert.ok(body, `missing CSS token block ${pattern}`);
    return new Map(
      [...body.matchAll(/(--af-[a-z0-9-]+):\s*([^;]+);/g)].map((match) => [match[1]!, match[2]!.trim()]),
    );
  };
  const blocks = {
    light: block(/\/\* --- Design tokens: LIGHT[\s\S]*?:root \{([\s\S]*?)\n\}/),
    dark: block(/:root\[data-theme="dark"\] \{([\s\S]*?)\n\}/),
  };

  for (const mode of ["light", "dark"] as const) {
    for (const [name, value] of Object.entries(deriveTheme(NORD_THEME, mode).tokens)) {
      assert.equal(blocks[mode].get(name)?.toLowerCase(), value.toLowerCase(), `${mode} ${name} drifted`);
    }
  }
});

for (const mode of ["light", "dark"] as const) {
  test(`${mode} derived tokens meet the contrast floor`, () => {
    const { tokens } = deriveTheme(NORD_THEME, mode);
    const surfaces = [tokens["--af-bg-canvas"], tokens["--af-bg-surface"], tokens["--af-bg-raised"]];
    for (const name of ["--af-text", "--af-text-2", "--af-text-3", "--af-accent", "--af-danger"] as const) {
      for (const surface of surfaces) {
        assert.ok(
          contrastRatio(tokens[name], surface) >= 4.5,
          `${name} ${tokens[name]} must be AA on ${surface}`,
        );
      }
    }
    assert.ok(contrastRatio(tokens["--af-on-accent"], tokens["--af-accent"]) >= 4.5);
    for (const name of ["--af-dot-ready", "--af-dot-lost", "--af-dot-limit", "--af-border-selected"] as const) {
      for (const surface of surfaces) {
        assert.ok(
          contrastRatio(tokens[name], surface) >= 3,
          `${name} ${tokens[name]} must remain visible on ${surface}`,
        );
      }
    }
  });
}

test("an unreadable or malformed custom slot falls back independently", () => {
  const custom = {
    ...NORD_THEME,
    foreground: NORD_THEME.background,
    accent: NORD_THEME.background,
    error: "red",
  };
  const { tokens } = deriveTheme(custom, "dark");

  assert.notEqual(tokens["--af-text"], custom.foreground);
  assert.notEqual(tokens["--af-accent"], custom.accent);
  assert.notEqual(tokens["--af-danger"], custom.error);
  assert.ok(contrastRatio(tokens["--af-text"], tokens["--af-bg-raised"]) >= 4.5);
  assert.ok(contrastRatio(tokens["--af-accent"], tokens["--af-bg-raised"]) >= 4.5);
  assert.ok(contrastRatio(tokens["--af-danger"], tokens["--af-bg-raised"]) >= 4.5);
});

test("an incoherent custom elevation system falls back as a readable unit", () => {
  const { tokens } = deriveTheme(
    {
      ...NORD_THEME,
      background: "#000000",
      background_subtle: "#FFFFFF",
      background_panel: "#000000",
      foreground: "#FFFFFF",
    },
    "dark",
  );

  assert.equal(tokens["--af-bg-canvas"], NORD_THEME.background);
  assert.equal(tokens["--af-bg-surface"], NORD_THEME.background_subtle);
  assert.equal(tokens["--af-bg-raised"], NORD_THEME.background_panel);
  for (const surface of [tokens["--af-bg-canvas"], tokens["--af-bg-surface"], tokens["--af-bg-raised"]]) {
    assert.ok(contrastRatio(tokens["--af-text"], surface) >= 4.5);
  }
});

test("the final light semantic fallback is verified against every surface", () => {
  const { tokens } = deriveTheme(
    {
      ...NORD_THEME,
      foreground: "#5431D4",
      foreground_strong: "#3D2893",
      background: "#F5F5F5",
      accent: "#BD7884",
    },
    "light",
  );
  const surfaces = [tokens["--af-bg-canvas"], tokens["--af-bg-surface"], tokens["--af-bg-raised"]];

  for (const surface of surfaces) {
    assert.ok(
      contrastRatio(tokens["--af-accent"], surface) >= 4.5,
      `${tokens["--af-accent"]} must remain AA on ${surface}`,
    );
    assert.ok(
      contrastRatio(tokens["--af-border-selected"], surface) >= 3,
      `${tokens["--af-border-selected"]} must remain visible on ${surface}`,
    );
  }
});
