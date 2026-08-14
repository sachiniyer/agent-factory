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

import {
  connectionAttemptMayCommit,
  contrastRatio,
  createLatestRequestGate,
  deriveTheme,
  hasConnectedToken,
  NORD_THEME,
  paletteFetchFailurePlan,
  themeColorMetaContents,
} from "./theme.js";

function composite(foreground: string, background: string, alpha: number): string {
  const channels = (color: string): number[] => [1, 3, 5].map((start) => Number.parseInt(color.slice(start, start + 2), 16));
  const fg = channels(foreground);
  const bg = channels(background);
  return `#${fg
    .map((value, index) => Math.round(value * alpha + bg[index]! * (1 - alpha)).toString(16).padStart(2, "0"))
    .join("")}`;
}

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
  assert.equal(light.tokens["--af-accent"], "#2D6271", "the light derivation keeps Nord's frost-cyan hue");
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

test("pane states consume their corresponding semantic border tokens", () => {
  const css = readFileSync(new URL("./styles.css", import.meta.url), "utf8");

  assert.match(css, /\.af-pane-focused\s*\{[^}]*outline:\s*2px solid var\(--af-border-selected\)/s);
  assert.match(css, /\.af-dragging-tab \.af-pane\s*\{[^}]*outline:\s*1px dashed var\(--af-border-interactive\)/s);
  assert.match(css, /\.af-drop-overlay\s*\{[^}]*border:\s*1px solid var\(--af-border-preview\)/s);
});

test("tinted semantic states consume their contrast-safe text tokens", () => {
  const css = readFileSync(new URL("./styles.css", import.meta.url), "utf8");

  for (const selector of [
    ".af-project-item-current .af-project-item-name",
    ".af-theme-opt-active",
    ".af-rail-empty-new",
    ".af-rail-new:hover",
    ".af-tasks-add:hover",
  ]) {
    assert.match(css, new RegExp(`${selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}[^}]*color:\\s*var\\(--af-accent-text\\)`, "s"));
  }
  for (const selector of [".af-tab-close:hover", ".af-pane-close:hover", ".af-danger:hover"]) {
    assert.match(css, new RegExp(`${selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}[^}]*color:\\s*var\\(--af-danger-text\\)`, "s"));
  }
  assert.match(css, /\.af-config-notice\s*\{[^}]*color:\s*var\(--af-accent-text\)/s);
  assert.match(
    css,
    /\.af-project-item-current \.af-project-item-path\s*\{[^}]*color:\s*var\(--af-selected-text-muted\)/s,
  );
  assert.match(css, /\.af-row-selected\s*\{[^}]*color:\s*var\(--af-selected-text\)/s);
  assert.match(css, /\.af-row-selected \.af-row-branch\s*\{[^}]*color:\s*var\(--af-selected-text-muted\)/s);
});

for (const mode of ["light", "dark"] as const) {
  test(`${mode} derived tokens meet the contrast floor`, () => {
    const { tokens } = deriveTheme(NORD_THEME, mode);
    const surfaces = [
      tokens["--af-bg-canvas"],
      tokens["--af-bg-surface"],
      tokens["--af-bg-inset"],
      tokens["--af-bg-raised"],
    ];
    for (const name of [
      "--af-text",
      "--af-text-2",
      "--af-text-3",
      "--af-accent",
      "--af-danger",
      "--af-text-muted",
      "--af-status-needs-you",
      "--af-status-working",
      "--af-status-waiting",
      "--af-status-broken",
      "--af-status-inactive",
    ] as const) {
      assert.match(tokens[name], /^#[0-9A-F]{6}$/, `${name} must be a concrete derived color`);
      for (const surface of surfaces) {
        assert.ok(
          contrastRatio(tokens[name], surface) >= 4.5,
          `${name} ${tokens[name]} must be AA on ${surface}`,
        );
      }
    }
    assert.ok(contrastRatio(tokens["--af-on-accent"], tokens["--af-accent"]) >= 4.5);
    assert.ok(contrastRatio(tokens["--af-on-accent"], tokens["--af-accent-hover"]) >= 4.5);
    for (const name of ["--af-dot-ready", "--af-dot-lost", "--af-dot-limit", "--af-border-selected"] as const) {
      for (const surface of surfaces) {
        assert.ok(
          contrastRatio(tokens[name], surface) >= 3,
          `${name} ${tokens[name]} must remain visible on ${surface}`,
        );
      }
    }
  });

  test(`${mode} semantic text stays AA on its translucent state fills`, () => {
    const { tokens } = deriveTheme(NORD_THEME, mode);
    const surfaces = [
      tokens["--af-bg-canvas"],
      tokens["--af-bg-surface"],
      tokens["--af-bg-inset"],
      tokens["--af-bg-raised"],
    ];
    const subtleAlpha = mode === "dark" ? 0.12 : 0.09;
    const tintAlpha = mode === "dark" ? 0.2 : 0.16;

    for (const surface of surfaces) {
      for (const alpha of [subtleAlpha, tintAlpha]) {
        const fill = composite(tokens["--af-accent"], surface, alpha);
        assert.ok(
          contrastRatio(tokens["--af-accent-text"], fill) >= 4.5,
          `${tokens["--af-accent-text"]} must be AA on accent fill ${fill}`,
        );
      }
      const dangerFill = composite(tokens["--af-danger"], surface, subtleAlpha);
      assert.ok(
        contrastRatio(tokens["--af-danger-text"], dangerFill) >= 4.5,
        `${tokens["--af-danger-text"]} must be AA on danger fill ${dangerFill}`,
      );
    }
  });
}

test("palette refresh generations reject an older completion for the same token", () => {
  const gate = createLatestRequestGate();
  const older = gate.begin();
  const newer = gate.begin();

  assert.equal(older.isCurrent(), false);
  assert.equal(newer.isCurrent(), true);
});

test("connection fences admit tokenless clients and reject invalidated attempts", () => {
  const gate = createLatestRequestGate();
  const attempt = gate.begin();

  assert.equal(hasConnectedToken(""), true, "the empty token is an authorized tokenless connection");
  assert.equal(hasConnectedToken(null), false);
  assert.equal(connectionAttemptMayCommit(attempt, "", ""), true);
  gate.invalidate();
  assert.equal(connectionAttemptMayCommit(attempt, "", ""), false);
});

test("transient palette failures retain the last good palette and retry", () => {
  assert.deepEqual(paletteFetchFailurePlan(503, true), { reset: false, retry: true, reauthenticate: false });
  assert.deepEqual(paletteFetchFailurePlan(0, false), { reset: true, retry: true, reauthenticate: false });
  assert.deepEqual(paletteFetchFailurePlan(404, true), { reset: true, retry: false, reauthenticate: false });
});

test("rejected palette credentials stop retrying and require authentication", () => {
  for (const status of [401, 403]) {
    assert.deepEqual(paletteFetchFailurePlan(status, true), {
      reset: false,
      retry: false,
      reauthenticate: true,
    });
  }
});

for (const mode of ["light", "dark"] as const) {
  test(`${mode} ANSI normal and bright roles remain readable and distinct`, () => {
    const { xterm } = deriveTheme(NORD_THEME, mode);
    const canvas = xterm.background!;

    for (const [normal, bright] of [
      ["black", "brightBlack"],
      ["red", "brightRed"],
      ["green", "brightGreen"],
      ["yellow", "brightYellow"],
      ["blue", "brightBlue"],
      ["magenta", "brightMagenta"],
      ["cyan", "brightCyan"],
      ["white", "brightWhite"],
    ] as const) {
      for (const name of [normal, bright]) {
        assert.ok(
          contrastRatio(xterm[name]!, canvas) >= 4.5,
          `${name} ${xterm[name]} must be readable on terminal canvas ${canvas}`,
        );
      }
      assert.notEqual(xterm[normal], xterm[bright], `${normal} and ${bright} must preserve intensity`);
    }
  });
}

test("selected-row foregrounds remain AA on both accent fill strengths", () => {
  const { tokens } = deriveTheme(
    {
      ...NORD_THEME,
      background: "#777777",
      background_subtle: "#777777",
      background_panel: "#777777",
      foreground: "#000000",
      foreground_muted: "#000000",
      foreground_dim: "#000000",
      accent: "#000000",
    },
    "dark",
  );
  const fills = [
    composite(tokens["--af-accent"], tokens["--af-bg-surface"], 0.12),
    composite(tokens["--af-accent"], tokens["--af-bg-surface"], 0.2),
  ];

  for (const name of [
    "--af-selected-text",
    "--af-selected-text-muted",
    "--af-selected-status-needs-you",
    "--af-selected-status-working",
    "--af-selected-status-waiting",
    "--af-selected-status-broken",
    "--af-selected-status-inactive",
  ] as const) {
    for (const fill of fills) {
      assert.ok(contrastRatio(tokens[name], fill) >= 4.5, `${name} ${tokens[name]} must be AA on ${fill}`);
    }
  }
});

test("a readable endpoint preserves a coherent custom surface system", () => {
  const { tokens } = deriveTheme(
    {
      ...NORD_THEME,
      background: "#777777",
      background_subtle: "#777777",
      background_panel: "#777777",
      foreground: "#777777",
    },
    "dark",
  );

  assert.equal(tokens["--af-bg-canvas"], "#777777");
  assert.equal(tokens["--af-bg-surface"], "#777777");
  assert.equal(tokens["--af-bg-raised"], "#777777");
  assert.notEqual(tokens["--af-text"], "#777777");
  for (const surface of [
    tokens["--af-bg-canvas"],
    tokens["--af-bg-surface"],
    tokens["--af-bg-inset"],
    tokens["--af-bg-raised"],
  ]) {
    assert.ok(contrastRatio(tokens["--af-text"], surface) >= 4.5);
  }
});

test("a surface system with no shared fill foreground falls back coherently", () => {
  const { tokens } = deriveTheme(
    {
      ...NORD_THEME,
      background: "#000000",
      background_subtle: "#000000",
      background_panel: "#FFFFFF",
      foreground: "#757575",
      foreground_muted: "#757575",
      foreground_dim: "#757575",
      accent: "#757575",
    },
    "dark",
  );
  const surfaces = [
    tokens["--af-bg-canvas"],
    tokens["--af-bg-surface"],
    tokens["--af-bg-inset"],
    tokens["--af-bg-raised"],
  ];
  assert.deepEqual(surfaces.slice(0, 2), [NORD_THEME.background, NORD_THEME.background_subtle]);
  for (const surface of surfaces) {
    for (const alpha of [0.12, 0.2]) {
      const fill = composite(tokens["--af-accent"], surface, alpha);
      assert.ok(contrastRatio(tokens["--af-accent-text"], fill) >= 4.5, `${tokens["--af-accent-text"]} on ${fill}`);
    }
  }
});

test("custom ANSI normal and bright roles remain readable and distinct", () => {
  const { xterm } = deriveTheme(
    {
      ...NORD_THEME,
      background: "#777777",
      background_subtle: "#777777",
      background_panel: "#777777",
      foreground: "#000000",
      foreground_muted: "#000000",
      foreground_dim: "#000000",
      accent: "#000000",
    },
    "dark",
  );
  for (const [normal, bright] of [
    ["black", "brightBlack"],
    ["red", "brightRed"],
    ["green", "brightGreen"],
    ["yellow", "brightYellow"],
    ["blue", "brightBlue"],
    ["magenta", "brightMagenta"],
    ["cyan", "brightCyan"],
    ["white", "brightWhite"],
  ] as const) {
    assert.notEqual(xterm[normal], xterm[bright], `${normal} and ${bright} collapsed`);
    for (const name of [normal, bright]) {
      assert.ok(
        contrastRatio(xterm[name]!, xterm.background!) >= 4.5,
        `${name} ${xterm[name]} must be AA on ${xterm.background}`,
      );
    }
  }
});

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

test("surface fallback also replaces rejected shadow and backdrop endpoints", () => {
  const { tokens } = deriveTheme(
    {
      ...NORD_THEME,
      background: "#FFFFFF",
      background_subtle: "#000000",
      background_panel: "#FFFFFF",
      foreground: "#777777",
    },
    "dark",
  );

  assert.equal(tokens["--af-bg-canvas"], NORD_THEME.background);
  assert.equal(tokens["--af-shadow-1"], "0 1px 2px rgba(46, 52, 64, 0.4)");
  assert.equal(tokens["--af-shadow-2"], "0 4px 10px rgba(46, 52, 64, 0.45)");
  assert.equal(tokens["--af-shadow-overlay"], "0 16px 48px rgba(46, 52, 64, 0.6)");
  assert.equal(tokens["--af-backdrop"], "rgba(46, 52, 64, 0.66)");
});

test("inset participates in the coherent surface and text contrast checks", () => {
  const { tokens } = deriveTheme(
    {
      ...NORD_THEME,
      background: "#000000",
      background_subtle: "#FFFFFF",
      background_panel: "#000000",
      foreground: "#757575",
    },
    "dark",
  );

  assert.equal(tokens["--af-bg-inset"], "#313744");
  assert.ok(contrastRatio(tokens["--af-text"], tokens["--af-bg-inset"]) >= 4.5);
});

test("xterm applies a readable configured terminal selection foreground", () => {
  const { xterm } = deriveTheme(
    {
      ...NORD_THEME,
      selection_background: "#000000",
      selection_foreground: "#FFFFFF",
    },
    "dark",
  );

  assert.equal(xterm.selectionForeground, "#FFFFFF");
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

test("light contrast correction preserves configured semantic hues", () => {
  const red = "#FF0000";
  const { tokens, xterm } = deriveTheme(
    {
      ...NORD_THEME,
      accent: red,
      success: red,
      warning: red,
      error: red,
      info: red,
      purple: red,
      pane_border_default: red,
      pane_border_selected: red,
      pane_border_interactive: red,
      pane_border_preview: red,
    },
    "light",
  );

  for (const color of [
    tokens["--af-accent"],
    tokens["--af-danger"],
    tokens["--af-dot-ready"],
    tokens["--af-dot-lost"],
    tokens["--af-border"],
    tokens["--af-border-selected"],
    xterm.red!,
    xterm.green!,
    xterm.yellow!,
    xterm.blue!,
    xterm.magenta!,
    xterm.cyan!,
  ]) {
    assert.match(color, /^#[0-9A-F]{2}0000$/, `${color} must retain the configured red hue`);
  }
});
