import assert from "node:assert/strict";
import test from "node:test";

import { shortcutRepeatGuard, tabCreateKey } from "./tab_create.js";

const down = (code: string, repeat: boolean) => ({ code, repeat });

test("repeats of the creating key are swallowed until keyup, whatever owns focus", () => {
  const guard = shortcutRepeatGuard();
  assert.equal(guard.swallow(down("KeyT", false)), false);
  guard.arm("KeyT");
  assert.equal(guard.swallow(down("KeyT", true)), true, "a repeat after the terminal took focus must not reach xterm");
  assert.equal(guard.swallow(down("KeyJ", true)), false, "only the originating key is held");
  guard.release("KeyT");
  assert.equal(guard.swallow(down("KeyT", true)), false);
});

test("a fresh press clears a guard whose keyup was missed", () => {
  const guard = shortcutRepeatGuard();
  guard.arm("KeyT");
  assert.equal(guard.swallow(down("KeyT", false)), false);
  assert.equal(guard.swallow(down("KeyT", true)), false);
});

test("a disconnect scopes in-flight create latches to the old connection", () => {
  assert.notEqual(tabCreateKey(1, "s", "shell"), tabCreateKey(2, "s", "shell"));
  assert.notEqual(tabCreateKey(1, "s", "shell"), tabCreateKey(1, "s", "vscode"));
  assert.equal(tabCreateKey(1, "s", "shell"), tabCreateKey(1, "s", "shell"));
});
