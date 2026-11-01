import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";

import { type FakeElement, FakeEvent, installFakeDom } from "./backlog_fakedom.js";
import type { ModalHandle } from "./components.js";
import { askConfirm } from "./modals.js";
import { type NavContext, decideKey } from "./nav.js";

let dom: ReturnType<typeof installFakeDom>;

beforeEach(() => {
  dom = installFakeDom();
});

afterEach(() => dom.restore());

const OPTS = { title: "Budget gate closed", body: "The plan is at 60% of the week, over the 50% limit.", confirmLabel: "Start anyway" };

function open(): { result: Promise<boolean>; modal: ModalHandle; dismissed: ModalHandle[] } {
  let modal: ModalHandle | null = null;
  const dismissed: ModalHandle[] = [];
  const result = askConfirm(OPTS, (m) => {
    modal = m;
    dom.doc.body.append(m.el as unknown as FakeElement);
  }, (m) => {
    dismissed.push(m);
    m.close();
  });
  return { result, modal: modal!, dismissed };
}

test("Escape closes the confirm through the handle, and the ask resolves no", async () => {
  const { result, modal } = open();
  // index.ts runs closeModal() for this action, which closes the open handle.
  assert.deepEqual(decideKey("Escape", { modalOpen: true } as NavContext), { kind: "closeModal" });
  modal.close();
  assert.equal(await result, false);
});

test("a confirm resolves yes once, and a later close does not change it", async () => {
  const { result, modal, dismissed } = open();
  const form = (modal.el as unknown as FakeElement).querySelector("form")!;
  form.dispatchEvent(new FakeEvent("submit"));
  assert.equal(await result, true);
  assert.deepEqual(dismissed, [modal]);
  modal.close();
  assert.equal(await result, true);
});

test("a replacing overlay closes the handle and the ask resolves no", async () => {
  const { result, modal } = open();
  open();
  modal.close();
  assert.equal(await result, false);
});

test("Cancel resolves no", async () => {
  const { result, modal } = open();
  const cancel = (modal.el as unknown as FakeElement).querySelectorAll("button").find((b) => b.textContent === "Cancel")!;
  cancel.click();
  assert.equal(await result, false);
});
