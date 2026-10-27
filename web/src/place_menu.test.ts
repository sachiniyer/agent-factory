import { test } from "node:test";
import assert from "node:assert/strict";
import { placeMenu } from "./components.js";

type Box = { top: number; bottom: number; height: number };
const box = (top: number, height: number): Box => ({ top, bottom: top + height, height });

function node(rect: Box, overflowY = "visible", parentElement: unknown = null) {
  const classes = new Set<string>();
  return {
    rect, overflowY, parentElement,
    getBoundingClientRect: () => rect,
    classList: { add: (c: string) => classes.add(c), remove: (c: string) => classes.delete(c), contains: (c: string) => classes.has(c) },
  };
}

function place(spaceBelow: number, gap: number) {
  Object.assign(globalThis, {
    window: { innerHeight: 1000 },
    getComputedStyle: (el: { overflowY: string }) => ({ overflowY: el.overflowY }),
  });
  const rail = node(box(0, 600), "auto");
  const trigger = node(box(600 - spaceBelow - 20, 20), "visible", rail);
  const anchor = trigger.rect;
  const panel = node(box(anchor.bottom + gap, 100));
  placeMenu(trigger as unknown as HTMLElement, panel as unknown as HTMLElement);
  return panel.classList.contains("af-menu-up");
}

test("placeMenu keeps the menu below when it and its gap fit", () => {
  assert.equal(place(108, 8), false);
});

test("placeMenu opens upward when the gap pushes the menu past the clip edge", () => {
  assert.equal(place(104, 8), true);
  assert.equal(place(104, 0), false);
});
