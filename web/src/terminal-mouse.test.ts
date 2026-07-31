import assert from "node:assert/strict";
import test from "node:test";

import { historyWheelPlan, terminalMouseOverride, terminalMouseOverrideHeld } from "./terminal-mouse.js";

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
