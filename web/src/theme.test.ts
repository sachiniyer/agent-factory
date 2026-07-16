// Unit coverage for the theme-color collapse rule (#1826/#1761 audit item).
//
// index.html ships one theme-color meta per scheme and the browser paints its chrome
// with whichever matches the OS. That is right for Auto and wrong for an explicit
// choice, so themeColorMetaContents collapses both metas to one colour when the user
// overrides. The rule is pure and pinned here; that the metas are actually rewritten
// in a live document is asserted by the Playwright selftest.

import { test } from "node:test";
import assert from "node:assert/strict";

import { themeColorMetaContents } from "./theme.js";

// The --af-bg-surface pair from styles.css: the appbar fill the browser chrome abuts.
const LIGHT = "#ffffff";
const DARK = "#141a22";

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
