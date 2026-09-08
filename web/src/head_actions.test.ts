import { test } from "node:test";
import assert from "node:assert/strict";
import { AppShell, type AppState } from "./ui.js";
import type { SessionData } from "./types.js";

test("fallback header rebuilds its callback when root identity is backfilled", () => {
  let callbacks: (() => boolean)[] = [];
  const shell = {
    headActionSig: "",
    headActions: { hidden: true, replaceChildren: (...next: (() => boolean)[]) => { callbacks = next; } },
    sessionActionButtons: (session: SessionData) => [() => session.is_root === true],
  };
  const patch = (AppShell.prototype as unknown as {
    patchHeadActions: (state: AppState, session: SessionData) => void;
  }).patchHeadActions;
  const state = { sessions: [], selectedProject: null } as unknown as AppState;
  const session: SessionData = { id: "root-id", title: "coordinator", branch: "main", can_kill: true };
  patch.call(shell, state, session);
  const before = callbacks[0];
  assert.equal(before(), false);
  patch.call(shell, state, { ...session, is_root: true });
  assert.notEqual(callbacks[0], before, "a new projection must replace the captured callback");
  assert.equal(callbacks[0](), true);
  const rooted = callbacks[0];
  patch.call(shell, state, { ...session, is_root: true });
  assert.equal(callbacks[0], rooted, "identical safety projections retain the controls");
});
