// The Docs strip keeps a surviving tab's button attached across re-renders
// (#7409). Browsers drop a click whose pressed node leaves the document before
// the release, so a doc-list refresh that rebuilt the strip mid-press lost the
// click. These cases pin the property under fakedom: the witness is the strip's
// removeChild, which every detach (including a move) goes through, and focus,
// which fakedom drops from a removed node the way a browser does.

import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";

import { type FakeElement, FakeEvent, installFakeDom } from "./fakedom.js";
import { DocTabs, type DocTabEvents } from "./docs_tabs.js";
import type { ProposalDocRef } from "./proposal_api.js";

let dom: ReturnType<typeof installFakeDom>;
beforeEach(() => {
  dom = installFakeDom();
});
afterEach(() => dom.restore());

const ref = (name: string): ProposalDocRef => ({ name, file: `${name}.md` });

function setup(): { strip: FakeElement; tabs: DocTabs; opened: string[]; detached: FakeElement[] } {
  const strip = dom.doc.createElement("div");
  dom.doc.body.append(strip);
  const opened: string[] = [];
  const none = (): void => {};
  const on: DocTabEvents = {
    open: (doc) => opened.push(doc.name),
    openGone: (doc) => opened.push(`gone:${doc.name}`),
    key: none, menu: none, press: none, move: none, release: none,
  };
  const detached: FakeElement[] = [];
  const removeChild = strip.removeChild.bind(strip);
  strip.removeChild = (n) => {
    detached.push(n as FakeElement);
    removeChild(n);
  };
  const tabs = new DocTabs(strip as unknown as HTMLElement, on);
  return { strip, tabs, opened, detached };
}

const names = (strip: FakeElement): string[] => strip.children.map((c) => c.textContent);
const selected = (strip: FakeElement): string[] =>
  strip.children.filter((c) => c.getAttribute("aria-selected") === "true").map((c) => c.textContent);

test("a refresh with the same docs detaches nothing and keeps the focused tab focused", () => {
  const { strip, tabs, detached } = setup();
  tabs.render([ref("a"), ref("b")], new Map(), "a");
  const [a, b] = strip.children;
  b.focus();
  tabs.render([ref("a"), ref("b")], new Map(), "a");
  assert.deepEqual(detached, [], "an unchanged list must not take a tab out of the strip");
  assert.equal(strip.children[0], a);
  assert.equal(strip.children[1], b);
  assert.equal(dom.doc.activeElement, b, "focus stays on the tab, as it would not on a rebuilt one");
});

test("a doc added mid-press keeps the pressed tab attached, and its click opens it", () => {
  const { strip, tabs, opened, detached } = setup();
  tabs.render([ref("a-first"), ref("b-second")], new Map(), "a-first");
  const pressed = strip.children[1];
  tabs.render([ref("a-first"), ref("b-second"), ref("c-third")], new Map(), "a-first");
  assert.deepEqual(names(strip), ["a-first", "b-second", "c-third"]);
  assert.deepEqual(detached, [], "adding a doc inserts its tab and moves no other");
  assert.equal(strip.children[1], pressed);
  pressed.click();
  assert.deepEqual(opened, ["b-second"]);
});

test("selection, tab order and title follow each render on the same buttons", () => {
  const { strip, tabs } = setup();
  tabs.render([ref("a"), ref("b")], new Map(), "a");
  const [a, b] = strip.children;
  assert.deepEqual(selected(strip), ["a"]);
  assert.equal(a.tabIndex, 0);
  assert.equal(b.tabIndex, -1);
  tabs.render([ref("a"), { name: "b", file: "b-renamed.md" }], new Map(), "b");
  assert.equal(strip.children[1], b);
  assert.deepEqual(selected(strip), ["b"]);
  assert.equal(a.tabIndex, -1);
  assert.equal(b.tabIndex, 0);
  assert.equal(b.title, "b-renamed.md");
});

test("a click opens the doc as last rendered, not as first built", () => {
  const strip = dom.doc.createElement("div");
  const seen: ProposalDocRef[] = [];
  const none = (): void => {};
  const tabs = new DocTabs(strip as unknown as HTMLElement, {
    open: (doc) => seen.push(doc), openGone: none, key: none, menu: none, press: none, move: none, release: none,
  });
  tabs.render([ref("a")], new Map(), null);
  const later = { name: "a", file: "a-later.md" };
  tabs.render([later], new Map(), null);
  strip.children[0].click();
  assert.equal(seen[0], later);
});

test("a deleted doc's tab follows the listed ones and opens through openGone", () => {
  const { strip, tabs, opened } = setup();
  tabs.render([ref("a")], new Map([["gone", ref("gone")]]), "gone");
  assert.deepEqual(names(strip), ["a", "gone (deleted)"]);
  assert.deepEqual(selected(strip), ["gone (deleted)"]);
  assert.match(strip.children[1].title, /gone\.md was deleted/);
  strip.children[1].click();
  assert.deepEqual(opened, ["gone:gone"]);
  // Listed again (restored): the deleted tab goes, the listed one stays put.
  const a = strip.children[0];
  tabs.render([ref("a"), ref("gone")], new Map([["gone", ref("gone")]]), "gone");
  assert.deepEqual(names(strip), ["a", "gone"]);
  assert.equal(strip.children[0], a);
});

test("a removed doc's tab leaves the strip and comes back as a new button", () => {
  const { strip, tabs, detached } = setup();
  tabs.render([ref("a"), ref("b"), ref("c")], new Map(), "a");
  const [a, b, c] = strip.children;
  tabs.render([ref("a"), ref("c")], new Map(), "a");
  assert.deepEqual(names(strip), ["a", "c"]);
  assert.deepEqual(detached, [b], "only the removed doc's tab is detached");
  assert.equal(strip.children[0], a);
  assert.equal(strip.children[1], c);
  tabs.render([ref("a"), ref("b"), ref("c")], new Map(), "a");
  assert.notEqual(strip.children[1], b, "a doc that left is rebuilt, not resurrected");
});

test("the tab's other events reach the pane with the doc and the button", () => {
  const strip = dom.doc.createElement("div");
  const calls: string[] = [];
  const tabs = new DocTabs(strip as unknown as HTMLElement, {
    open: () => calls.push("open"),
    openGone: () => calls.push("openGone"),
    key: (_e, doc) => calls.push(`key:${doc.name}`),
    menu: (_e, doc, tab) => calls.push(`menu:${doc.name}:${tab === (strip.children[0] as unknown)}`),
    press: (_e, doc, tab) => calls.push(`press:${doc.name}:${tab === (strip.children[0] as unknown)}`),
    move: () => calls.push("move"),
    release: () => calls.push("release"),
  });
  tabs.render([ref("a")], new Map(), "a");
  const a = strip.children[0];
  for (const type of ["keydown", "contextmenu", "pointerdown", "pointermove", "pointerup", "pointercancel", "pointerleave"]) {
    a.dispatchEvent(new FakeEvent(type));
  }
  assert.deepEqual(calls, ["key:a", "menu:a:true", "press:a:true", "move", "release", "release", "release"]);
});
