import assert from "node:assert/strict";
import test from "node:test";

import {
  historyWheelPlan,
  invertTerminalMouseOverride,
  terminalMouseOverride,
  terminalMouseOverrideFlag,
  terminalMouseOverrideHeld,
  lineContentColumns,
  terminalCellAtPoint,
  TOUCH_LONG_PRESS_MS,
  TOUCH_SCROLL_SLOP_PX,
  touchHistoryScrollPlan,
  touchPressStillHeld,
  touchScrollClaimsGesture,
  wordRangeAtColumn,
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

test("a touch drag scrolls the way the content follows the finger (#2682)", () => {
  // Dragging DOWN pulls older output into view: negative lines, like a wheel scrolled
  // back. Getting this backwards is the one failure a real device shows instantly and
  // a green unit suite would still call a fix, so pin the direction, not just the
  // magnitude.
  assert.deepEqual(touchHistoryScrollPlan(100, 140, 24, 20, 0), { lines: -2, remainder: 0 });
  assert.deepEqual(touchHistoryScrollPlan(140, 100, 24, 20, 0), { lines: 2, remainder: 0 });

  // A drag is delivered as many small steps, so sub-row motion has to accumulate
  // across them or a slow scroll never moves at all.
  const first = touchHistoryScrollPlan(100, 108, 24, 16, 0);
  assert.deepEqual(first, { lines: 0, remainder: -0.5 });
  assert.deepEqual(touchHistoryScrollPlan(108, 116, 24, 16, first.remainder), { lines: -1, remainder: 0 });

  // A finger that has not moved must not scroll — and must not report -0 rows.
  assert.deepEqual(touchHistoryScrollPlan(100, 100, 24, 20, 0), { lines: 0, remainder: 0 });
});

test("a tap's wobble is not a scroll, in either direction (#2682)", () => {
  // Claiming a gesture cancels the touch, and a cancelled touch fires no
  // compatibility mouse events — so this threshold is what keeps an unsteady tap
  // delivering its click to a mouse-aware application instead of silently doing
  // nothing. Both directions, because a finger drifts either way.
  assert.equal(touchScrollClaimsGesture(200, 203), false);
  assert.equal(touchScrollClaimsGesture(200, 197), false);
  assert.equal(touchScrollClaimsGesture(200, 200 + TOUCH_SCROLL_SLOP_PX - 1), false);

  // At the threshold the gesture is a scroll — the same distance at which the
  // browser would start one itself, so the two can never both act on it.
  assert.equal(touchScrollClaimsGesture(200, 200 + TOUCH_SCROLL_SLOP_PX), true);
  assert.equal(touchScrollClaimsGesture(200, 200 - TOUCH_SCROLL_SLOP_PX), true);

  // Under one terminal row, or a real scroll would visibly lag the finger.
  assert.ok(TOUCH_SCROLL_SLOP_PX < 13 * 1.2);
});

test("a point resolves to the cell under it, and nothing outside the grid (#2849)", () => {
  const rect = { left: 100, top: 50, width: 800, height: 400 }; // 80x20 grid → 10x20 cells
  assert.deepEqual(terminalCellAtPoint(100, 50, rect, 80, 20), { col: 0, row: 0 });
  assert.deepEqual(terminalCellAtPoint(105, 55, rect, 80, 20), { col: 0, row: 0 });
  assert.deepEqual(terminalCellAtPoint(115, 71, rect, 80, 20), { col: 1, row: 1 });
  // The last cell is inside; one pixel past the box is not — an off-by-one here
  // would copy from a row the finger never touched.
  assert.deepEqual(terminalCellAtPoint(899, 449, rect, 80, 20), { col: 79, row: 19 });
  assert.equal(terminalCellAtPoint(900, 449, rect, 80, 20), null);
  assert.equal(terminalCellAtPoint(899, 450, rect, 80, 20), null);
  assert.equal(terminalCellAtPoint(99, 60, rect, 80, 20), null);

  // A pane that has not been laid out yet must not resolve to cell 0,0.
  assert.equal(terminalCellAtPoint(0, 0, { left: 0, top: 0, width: 0, height: 0 }, 80, 20), null);
});

test("long-press selects the whitespace-delimited token under the finger (#2849)", () => {
  const cells = [..."cd /srv/app-2 && go test"];

  // The token, not xterm's double-click word: a path keeps its slashes and a branch
  // its dashes, because a fragment of either is not what anyone meant to copy.
  assert.deepEqual(wordRangeAtColumn(cells, 5), { start: 3, length: 10 });
  assert.deepEqual(wordRangeAtColumn(cells, 3), { start: 3, length: 10 });
  assert.deepEqual(wordRangeAtColumn(cells, 12), { start: 3, length: 10 });
  assert.deepEqual(wordRangeAtColumn(cells, 0), { start: 0, length: 2 });

  // A blank cell has no word — the caller falls back to the whole line.
  assert.equal(wordRangeAtColumn(cells, 2), null);
  assert.equal(wordRangeAtColumn(cells, 999), null);
  assert.equal(wordRangeAtColumn([], 0), null);

  // A wide character spans two columns and reports "" in the second. It has to join
  // the token it continues, or every CJK word would be cut at its first glyph.
  const wide = ["ls", "", " ", "近", "", "藤", "", " "];
  assert.deepEqual(wordRangeAtColumn(wide, 3), { start: 3, length: 4 });
  assert.deepEqual(wordRangeAtColumn(wide, 4), { start: 3, length: 4 });
  assert.deepEqual(wordRangeAtColumn(wide, 0), { start: 0, length: 2 });
});

test("the line fallback stops at the content, not the blank tail (#2849)", () => {
  assert.equal(lineContentColumns([..."echo hi", " ", " ", " "]), 7);
  assert.equal(lineContentColumns([..."x"]), 1);
  assert.equal(lineContentColumns([" ", " "]), 0);
  assert.equal(lineContentColumns([]), 0);
  // A trailing wide-char continuation is content, not tail.
  assert.equal(lineContentColumns(["近", "", " "]), 2);
});

test("a press and a scroll are decided by ONE threshold (#2849)", () => {
  // Same number in both directions, so no gesture can be a press and a scroll at
  // once, and none can be neither.
  assert.equal(touchPressStillHeld(100, 200, 100, 200), true);
  assert.equal(touchPressStillHeld(100, 200, 100 + TOUCH_SCROLL_SLOP_PX - 1, 200), true);
  assert.equal(touchPressStillHeld(100, 200, 100, 200 - (TOUCH_SCROLL_SLOP_PX - 1)), true);
  assert.equal(touchPressStillHeld(100, 200, 100 + TOUCH_SCROLL_SLOP_PX, 200), false);
  assert.equal(touchPressStillHeld(100, 200, 100, 200 + TOUCH_SCROLL_SLOP_PX), false);
  // The press gives up exactly where the scroll takes over.
  assert.equal(touchPressStillHeld(0, 0, 0, TOUCH_SCROLL_SLOP_PX), !touchScrollClaimsGesture(0, TOUCH_SCROLL_SLOP_PX));

  // Long enough to be deliberate, short enough that a thumb does not think it failed.
  assert.ok(TOUCH_LONG_PRESS_MS >= 300 && TOUCH_LONG_PRESS_MS <= 800);
});
