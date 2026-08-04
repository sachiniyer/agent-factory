import assert from "node:assert/strict";
import test from "node:test";

import {
  historyWheelPlan,
  invertTerminalMouseOverride,
  terminalMouseOverride,
  terminalMouseOverrideFlag,
  terminalMouseOverrideHeld,
} from "./terminal-mouse.js";

test("application-mouse escape matches xterm's platform selection modifier", () => {
  assert.equal(terminalMouseOverride("Linux x86_64"), "Shift");
  assert.equal(terminalMouseOverride("Win32"), "Shift");
  assert.equal(terminalMouseOverride("MacIntel"), "Option");
  // Match xterm's own Platform.isMac predicate exactly. It classifies iPad
  // separately, so selection remains Shift there even though it is Apple-made.
  assert.equal(terminalMouseOverride("iPad"), "Shift");

  assert.equal(terminalMouseOverrideHeld({ shiftKey: true, altKey: false }, "Shift"), true);
  assert.equal(terminalMouseOverrideHeld({ shiftKey: false, altKey: true }, "Option"), true);
  assert.equal(terminalMouseOverrideHeld({ shiftKey: true, altKey: false }, "Option"), false);
});

test("inverting the override flips exactly the bit xterm reads to force selection", () => {
  // The flag names must stay xterm's own (SelectionService.shouldForceSelection):
  // Option/altKey on macOS, Shift/shiftKey elsewhere. Inverting the wrong one would
  // silently leave app mouse mode owning every drag.
  assert.equal(terminalMouseOverrideFlag("Option"), "altKey");
  assert.equal(terminalMouseOverrideFlag("Shift"), "shiftKey");

  // A plain drag must LOOK modified to xterm, so it selects instead of reporting.
  const plain = { shiftKey: false, altKey: false };
  invertTerminalMouseOverride(plain, "Shift");
  assert.equal(terminalMouseOverrideHeld(plain, "Shift"), true, "plain drag now forces selection");
  assert.equal(plain.altKey, false, "the other modifier is untouched — Alt still means column select");

  // …and a held modifier must look plain, so the drag reaches the application.
  const held = { shiftKey: true, altKey: false };
  invertTerminalMouseOverride(held, "Shift");
  assert.equal(terminalMouseOverrideHeld(held, "Shift"), false, "the modifier hands the drag to the app");

  const macPlain = { shiftKey: false, altKey: false };
  invertTerminalMouseOverride(macPlain, "Option");
  assert.equal(macPlain.altKey, true, "macOS forces selection via Option");
  assert.equal(macPlain.shiftKey, false, "Shift is not macOS's flag and must not move");

  const macHeld = { shiftKey: false, altKey: true };
  invertTerminalMouseOverride(macHeld, "Option");
  assert.equal(macHeld.altKey, false);
});

test("history wheel converts pixels, lines, and pages without losing trackpad fractions", () => {
  assert.deepEqual(historyWheelPlan({ deltaY: -5, deltaMode: 0 }, 24, 10, 0), {
    lines: 0,
    remainder: -0.5,
  });
  assert.deepEqual(historyWheelPlan({ deltaY: -5, deltaMode: 0 }, 24, 10, -0.5), {
    lines: -1,
    remainder: 0,
  });
  assert.deepEqual(historyWheelPlan({ deltaY: 3, deltaMode: 1 }, 24, 10, 0), { lines: 3, remainder: 0 });
  assert.deepEqual(historyWheelPlan({ deltaY: -1, deltaMode: 2 }, 24, 10, 0), { lines: -24, remainder: 0 });
});
