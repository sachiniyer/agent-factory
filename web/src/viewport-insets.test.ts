import { test } from "node:test";
import assert from "node:assert/strict";

import { viewportInsets } from "./viewport-insets.js";

test("no keyboard and no pinch leaves both insets at 0 (#6431)", () => {
  assert.deepEqual(viewportInsets(844, { height: 844, offsetTop: 0 }), { top: 0, bottom: 0 });
});

test("a soft keyboard shows up as a bottom inset (#6431)", () => {
  assert.deepEqual(viewportInsets(844, { height: 508, offsetTop: 0 }), { top: 0, bottom: 336 });
});

test("a scrolled visual viewport moves the top edge and shrinks the bottom (#6431)", () => {
  assert.deepEqual(viewportInsets(844, { height: 508, offsetTop: 100 }), { top: 100, bottom: 236 });
});

test("a visual viewport taller than the layout one (rounding, overscroll) never goes negative (#6431)", () => {
  assert.deepEqual(viewportInsets(844, { height: 846, offsetTop: -2 }), { top: 0, bottom: 0 });
});
