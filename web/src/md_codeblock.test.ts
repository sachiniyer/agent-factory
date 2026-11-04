// Unit tests for the code-fence widget (#6731), on the fake DOM: the widget's
// plain first paint (the lazy chunk cannot resolve under node, so the block
// must read correctly before and without it), the language badge, the
// mousedown that puts the cursor inside the fence, and buildCodeNodes, which
// must reproduce the source exactly as text and spans.

import assert from "node:assert/strict";
import { test } from "node:test";
import type { EditorView } from "@codemirror/view";
import { FakeElement, FakeEvent, installFakeDom } from "./fakedom.js";
import { buildCodeNodes, CodeBlockWidget } from "./md_codeblock.js";

function fakeView(seen: { pos?: number }): EditorView {
  return {
    posAtDOM: () => 7,
    dispatch: (o: { selection: { anchor: number } }) => {
      seen.pos = o.selection.anchor;
    },
    focus: () => {},
  } as unknown as EditorView;
}

test("a fence with a language paints its source plainly first, badge on top", () => {
  const { restore } = installFakeDom();
  try {
    const wrap = new CodeBlockWidget("func main() {}", "go").toDOM(fakeView({}));
    assert.equal(wrap.className, "cm-md-fence");
    const badge = wrap.querySelector(".cm-md-fence-lang");
    assert.equal(badge?.textContent, "go");
    const code = wrap.querySelector(".cm-md-fence-code code");
    assert.equal(code?.textContent, "func main() {}", "the block reads as code before the chunk lands");
    assert.equal(code?.querySelector("span"), null, "no spans before the chunk lands");
  } finally {
    restore();
  }
});

test("a fence without a language has no badge and stays plain", () => {
  const { restore } = installFakeDom();
  try {
    const wrap = new CodeBlockWidget("plain text", "").toDOM(fakeView({}));
    assert.equal(wrap.querySelector(".cm-md-fence-lang"), null);
    assert.equal(wrap.querySelector(".cm-md-fence-code code")?.textContent, "plain text");
  } finally {
    restore();
  }
});

test("a click inside the block puts the cursor in the fence", () => {
  const { restore } = installFakeDom();
  try {
    const seen: { pos?: number } = {};
    const wrap = new CodeBlockWidget("x", "go").toDOM(fakeView(seen)) as unknown as FakeElement;
    wrap.querySelector(".cm-md-fence-code")!.dispatchEvent(new FakeEvent("mousedown"));
    assert.equal(seen.pos, 7, "mousedown must move the cursor inside the fence");
  } finally {
    restore();
  }
});

test("widgets compare equal only on identical source and language", () => {
  assert.ok(new CodeBlockWidget("a", "go").eq(new CodeBlockWidget("a", "go")));
  assert.ok(!new CodeBlockWidget("a", "go").eq(new CodeBlockWidget("b", "go")));
  assert.ok(!new CodeBlockWidget("a", "go").eq(new CodeBlockWidget("a", "python")));
});

test("buildCodeNodes reassembles the source as text and styled spans", () => {
  const { restore } = installFakeDom();
  try {
    const src = `let x = "<img src=x onerror=alert(1)>"`;
    const nodes = buildCodeNodes(src, [
      { from: 0, to: 3, cls: "tok-keyword" },
      { from: 10, to: src.length, cls: "tok-string" },
    ]);
    const code = document.createElement("code");
    code.append(...nodes.map((n) => n.cloneNode(true)));
    assert.equal(code.textContent, src, "the nodes must reproduce the source exactly");
    const spans = code.querySelectorAll("span");
    assert.equal(spans.length, 2);
    assert.equal(spans[0].className, "tok-keyword");
    assert.equal(spans[0].textContent, "let");
    // Whatever the code names stays text: no element comes out of it.
    assert.equal(code.querySelector("img"), null);
  } finally {
    restore();
  }
});
