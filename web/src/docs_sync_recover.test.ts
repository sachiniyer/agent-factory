import assert from "node:assert/strict";
import { test } from "node:test";
import { type Draft, type Event, type Model, initialModel, statusOf, step } from "./docs_sync_model.js";

type Loose = Event | { t: "save_ok" | "save_conflict" | "save_fail" | "save_fatal" | "reload_ok" | "reload_fail" | "reconcile_ok" | "reconcile_fail"; [k: string]: unknown };

/** Runs events; a reply without a request id is addressed to the request in flight. */
function run(m: Model, ...events: Loose[]): { m: Model; fx: ReturnType<typeof step>[1] } {
  let fx: ReturnType<typeof step>[1] = [];
  for (const e of events) {
    const withSeq = "seq" in e || !/^(save_|reload_|reconcile_)/.test(e.t) ? e : { ...e, seq: m.flight?.seq ?? m.readSeq ?? 0 };
    [m, fx] = step(m, withSeq as Event);
  }
  return { m, fx };
}

const open = (doc: { text: string; commit: string }, draft: Draft | null = null, ...between: Event[]): ReturnType<typeof run> =>
  run(initialModel(), { t: "load_ok", doc }, ...between, { t: "draft_read", draft });

const loaded = (): Model => open({ text: "v1", commit: "c1" }).m;

test("a final save that fails keeps the buffer in storage and reconciles before it retries", () => {
  // The user leaves with an unsaved edit; the final save goes out...
  let r = run(loaded(), { t: "edit", text: "v1 edited" }, { t: "close" });
  assert.deepEqual(r.fx.filter((f) => f.t === "save").map((f) => (f as { final: boolean }).final), [true]);
  assert.ok(r.m.stashed !== null, "the edit is in storage before the save is sent");
  // ...and its outcome is unknown (a 5xx, a timeout, the network).
  r = run(r.m, { t: "save_fail", error: "503 service unavailable" });
  assert.equal(r.m.stashed !== null, true, "an unknown outcome does not clear the stored edit");
  assert.equal(r.fx.some((f) => f.t === "save"), false, "nothing is resent blind");
  assert.deepEqual(r.fx.filter((f) => f.t === "load").map((f) => (f as { kind: string }).kind), ["reconcile"], "the daemon's head is read first");
  // The head is unchanged: the save did not land, so the closed tab sends it again.
  r = run(r.m, { t: "reconcile_ok", doc: { text: "v1", commit: "c1" } });
  assert.equal(r.fx.some((f) => f.t === "save"), false, "not in a loop of save and read: the retry waits");
  assert.deepEqual(r.fx.filter((f) => f.t === "arm").map((f) => (f as { ms: number }).ms), [5000]);
  r = run(r.m, { t: "timer" });
  assert.deepEqual(r.fx.filter((f) => f.t === "save").map((f) => (f as { text: string }).text), ["v1 edited"]);
  r = run(r.m, { t: "save_ok", result: { kind: "saved", text: "v1 edited", commit: "c2", merged: false } });
  assert.equal(r.m.stashed, null, "storage is cleared once a save has landed");
  assert.ok(r.fx.some((f) => f.t === "unstash"));
});

test("a tab that opens on stored unsent edits shows them as recovered and saves them again", () => {
  const draft = { buffer: "v1 typed before the page closed", base: "v1", theirs: "", head: "", kind: "unsent" as const };
  const r = open({ text: "v1", commit: "c1" }, draft);
  assert.equal(r.m.buffer, "v1 typed before the page closed");
  assert.equal(statusOf(r.m).status, "recovered");
  assert.ok(r.fx.some((f) => f.t === "arm"), "autosave is armed");
});

test("recovered edits are rebased onto what the daemon holds now", () => {
  const draft = { buffer: "a2\nb\nc", base: "a\nb\nc", theirs: "", head: "", kind: "unsent" as const };
  const r = open({ text: "a\nb\nC", commit: "c9" }, draft);
  assert.equal(r.m.buffer, "a2\nb\nC", "the agent's edit and the recovered one both survive");
  assert.equal(statusOf(r.m).status, "recovered");
});

test("recovered edits that collide with the agent's open a conflict instead of overwriting it", () => {
  const draft = { buffer: "mine", base: "base", theirs: "", head: "", kind: "unsent" as const };
  const r = open({ text: "theirs", commit: "c9" }, draft);
  assert.equal(statusOf(r.m).status, "conflict");
  assert.equal(r.m.buffer, "mine");
});

test("a save whose reply was lost is reconciled against the head, and a landed one is not resent", () => {
  let r = run(loaded(), { t: "edit", text: "v1 a" }, { t: "timer" });
  r = run(r.m, { t: "save_fail", error: "network error: reply lost" });
  assert.equal(r.m.unknown, true);
  r = run(r.m, { t: "reconcile_ok", doc: { text: "v1 a", commit: "c2" } });
  assert.equal(statusOf(r.m).status, "saved", "it landed: the buffer is clean on the new base");
  assert.equal(r.m.base, "c2");
  assert.equal(r.fx.some((f) => f.t === "save"), false);
});

test("an undo back to the saved text keeps the draft while a different save is in flight", () => {
  let r = run(loaded(), { t: "edit", text: "v1 B" }, { t: "timer" }); // B is on its way
  r = run(r.m, { t: "edit", text: "v1" }); // the user undoes back to what is saved
  assert.equal(r.m.buffer, r.m.saved);
  assert.notEqual(r.m.stashed, null, "B may land: the undo must stay recoverable");
  r = run(r.m, { t: "save_ok", result: { kind: "saved", text: "v1 B", commit: "c2", merged: false } });
  assert.equal(r.m.buffer, "v1");
  assert.equal(statusOf(r.m).status, "pending", "B landed, so the undone text is unsaved work");
});

test("typing after a save with a lost reply is rebased onto the head that save left, undo included", () => {
  let r = run(open({ text: "a\nb\nc", commit: "c1" }).m, { t: "edit", text: "a\nb\nC" }, { t: "timer" });
  r = run(r.m, { t: "save_fail", error: "network error: reply lost" }); // it may have landed
  r = run(r.m, { t: "edit", text: "a2\nb\nc" }); // typed line 1, undid line 3
  assert.equal(r.fx.some((f) => f.t === "save"), false, "nothing is sent while the outcome is unknown");
  // The head holds that save: reading it first is what lets the undo survive a merge.
  r = run(r.m, { t: "reconcile_ok", doc: { text: "a\nb\nC", commit: "c2" } });
  assert.equal(r.m.buffer, "a2\nb\nc", "the undo is not replaced by the earlier save");
  assert.equal(r.m.base, "c2");
  assert.equal(statusOf(r.m).status, "pending", "and it goes out on the new base");
});

test("a reply that carries an old request id is ignored", () => {
  let r = run(loaded(), { t: "edit", text: "v1 a" }, { t: "timer" });
  const first = r.m.flight!.seq;
  r = run(r.m, { t: "save_fail", seq: first, error: "timed out" });
  r = run(r.m, { t: "reconcile_ok", doc: { text: "v1", commit: "c1" } }, { t: "timer" }); // not landed; resend
  assert.equal(r.m.phase, "saving");
  const second = r.m.flight!.seq;
  assert.notEqual(second, first);
  // The first request's reply finally arrives (a duplicate, or out of order).
  const late = step(r.m, { t: "save_ok", seq: first, result: { kind: "saved", text: "v1 a", commit: "c9", merged: false } });
  assert.equal(late[0].phase, "saving", "it is not taken for the reply to the second request");
  assert.equal(late[0].base, "c1");
});

test("a doc deleted under unsaved edits is kept, not dropped", () => {
  let r = run(loaded(), { t: "edit", text: "v1 mine" });
  r = run(r.m, { t: "gone" });
  assert.equal(statusOf(r.m).status, "gone");
  assert.notEqual(r.m.stashed, null, "the edits stay in storage");
  assert.equal(r.fx.some((f) => f.t === "save" || f.t === "load"), false);
  r = run(r.m, { t: "revive", head: { text: "template", commit: "c7" } });
  assert.equal(r.m.base, "c7");
  assert.equal(r.m.buffer, "v1 mine");
  assert.equal(statusOf(r.m).status, "pending", "restored onto the new doc, and saved again");
});

test("a clean doc deleted remotely just ends", () => {
  const r = run(loaded(), { t: "gone" });
  assert.equal(r.m.dead, true);
});

test("a 4xx refusal is not retried and does not leave the outcome unknown", () => {
  let r = run(loaded(), { t: "edit", text: "v1 too large" }, { t: "timer" });
  r = run(r.m, { t: "save_fatal", error: "doc is larger than the limit" });
  assert.equal(r.m.unknown, false);
  assert.equal(r.fx.some((f) => f.t === "arm" || f.t === "save" || f.t === "load"), false);
  r = run(r.m, { t: "edit", text: "v1 smaller" });
  assert.ok(r.fx.some((f) => f.t === "arm"), "editing again tries again");
});

test("a suspended sync (its doc is being deleted) neither retries nor arms a timer; abandon ends it", () => {
  let r = run(loaded(), { t: "edit", text: "v1 edited" }, { t: "timer" });
  r = run(r.m, { t: "suspend" });
  r = run(r.m, { t: "save_fail", error: "404 doc not found" });
  assert.equal(r.fx.some((f) => f.t === "arm"), false, "no retry while the doc is being deleted");
  r = run(r.m, { t: "timer" });
  assert.equal(r.fx.some((f) => f.t === "save"), false);
  assert.notEqual(r.m.stashed, null, "the edits stay stored until the delete has landed");
  r = run(r.m, { t: "abandon", keepDraft: false });
  assert.ok(r.fx.some((f) => f.t === "unstash"), "a landed delete drops the draft");
  r = run(r.m, { t: "edit", text: "x" }, { t: "timer" }, { t: "reconnect" });
  assert.equal(r.fx.length, 0, "an abandoned sync does nothing");
});

test("a failed delete keeps the draft for the tab that reopens the doc", () => {
  let r = run(loaded(), { t: "edit", text: "v1 edited" }, { t: "timer" });
  r = run(r.m, { t: "suspend" });
  r = run(r.m, { t: "abandon", keepDraft: true });
  assert.equal(r.fx.some((f) => f.t === "unstash"), false);
});

test("a stored draft whose save landed beside an unrelated agent edit keeps the undo", () => {
  // The user saved A over a, undid to a, and the page closed before the reply: the draft
  // holds the buffer a and the text that may have landed. The agent has since edited line e.
  const draft = { buffer: "a\nb\nc\nd\ne", base: "a\nb\nc\nd\ne", theirs: "", head: "", kind: "unsent" as const, maybe: ["A\nb\nc\nd\ne"] };
  const r = open({ text: "A\nb\nc\nd\nE", commit: "c9" }, draft);
  assert.equal(r.m.buffer, "a\nb\nc\nd\nE", "the undo is applied on top of what landed, and the agent's edit stays");
  assert.ok(!r.fx.some((f) => f.t === "unstash"), "the draft is not dropped");
  assert.equal(statusOf(r.m).status, "recovered");
});

test("a stored draft whose maybe-text did not land is rebased from its base", () => {
  const draft = { buffer: "a\nb\nc\nd\ne2", base: "a\nb\nc\nd\ne", theirs: "", head: "", kind: "unsent" as const, maybe: ["a\nb\nc\nd\ne2"] };
  const r = open({ text: "A\nb\nc\nd\ne", commit: "c9" }, draft);
  assert.equal(r.m.buffer, "A\nb\nc\nd\ne2");
});

test("Discard on a deleted doc leaves no draft behind: an abandoned sync does not mirror its buffer", () => {
  let r = run(loaded(), { t: "edit", text: "v1 edited" }, { t: "gone" });
  assert.equal(statusOf(r.m).status, "gone");
  r = run(r.m, { t: "abandon", keepDraft: false });
  assert.deepEqual(r.fx.map((f) => f.t).filter((t) => t === "stash" || t === "unstash"), ["unstash"], "removed, and not written back");
  assert.equal(r.m.stashed, null);
  r = run(r.m, { t: "edit", text: "v1 edited again" });
  assert.equal(r.fx.some((f) => f.t === "stash"), false, "and later edits are inert");
});

test("a deleted doc keeps an undone buffer in storage when the save that was out may have landed", () => {
  // load v1, edit v2, save (out), undo to v1, then the doc is deleted before the reply.
  let r = run(loaded(), { t: "edit", text: "v2" }, { t: "flush" }, { t: "edit", text: "v1" });
  r = run(r.m, { t: "gone" });
  assert.equal(statusOf(r.m).status, "gone");
  assert.ok(!r.fx.some((f) => f.t === "unstash"), "the recovery entry stays");
  assert.deepEqual(r.m.inDoubt, ["v2"]);
});

test("a closed tab whose saves keep failing waits longer each time, while its reads succeed", () => {
  let r = run(loaded(), { t: "edit", text: "v2" }, { t: "close" });
  const delays: number[] = [];
  for (let i = 0; i < 6; i++) {
    r = run(r.m, { t: "save_fail", error: "503" });
    r = run(r.m, { t: "reconcile_ok", doc: { text: "v1", commit: "c1" } });
    assert.equal(r.fx.some((f) => f.t === "save"), false);
    delays.push(...r.fx.filter((f) => f.t === "arm").map((f) => (f as { ms: number }).ms));
    r = run(r.m, { t: "timer" });
  }
  assert.deepEqual(delays, [5000, 10000, 20000, 40000, 60000, 60000]);
});

test("a closed tab whose timed-out save ends later sends no corrective save of its own", () => {
  // save B (timed out, still out), undo to A, the tab closes and goes quiet; the request then ends.
  let r = run(loaded(), { t: "edit", text: "v2" }, { t: "flush" });
  r = run(r.m, { t: "save_fail", error: "the daemon did not answer in time", outstanding: true });
  r = run(r.m, { t: "reconcile_ok", doc: { text: "v1", commit: "c1" } });
  r = run(r.m, { t: "edit", text: "v1" }, { t: "close" });
  const seq = r.m.late[0]!.seq;
  r = run(r.m, { t: "late_done", seq });
  assert.equal(r.fx.some((f) => f.t === "save" || f.t === "load"), false, "it reads nothing and sends nothing: the next pane restores its draft");
  assert.equal(r.fx.some((f) => f.t === "unstash"), false, "and the draft, which may be the only copy of the undo, stays");
  assert.ok(r.m.stashed !== null);
});

test("an adopted draft that equals the head but names a save that may still be out keeps it in doubt", () => {
  // The closed pane stored buffer a (the undo) with the save of A possibly still out.
  const draft = { buffer: "a", base: "a", theirs: "", head: "", kind: "unsent" as const, maybe: ["A"] };
  let r = open({ text: "a", commit: "c1" }, draft);
  assert.ok(!r.fx.some((f) => f.t === "unstash"), "the draft is kept");
  assert.deepEqual(r.fx.filter((f) => f.t === "give_up").map((f) => (f as { ms: number }).ms), [300_000]);
  // The request lands and the change event arrives: the head is read with A in doubt, and
  // the undo goes back on top of it.
  r = run(r.m, { t: "remote", commit: "c2" });
  assert.deepEqual(r.fx.filter((f) => f.t === "load").map((f) => (f as { kind: string }).kind), ["reconcile"], "not a plain reload");
  r = run(r.m, { t: "reconcile_ok", doc: { text: "A", commit: "c2" } });
  assert.equal(r.m.buffer, "a", "the undo stands over what landed");
});

test("a merged save reply that brings in a doubted save keeps the user's revert of it", () => {
  // B ("A" on the first line) was sent and timed out; the user reverted it and edited the last line; the
  // daemon had applied B first, so the merge of the later save keeps B's first line, which the revert undid.
  const a = "a\nb\nc\nd\ne";
  let r = run(open({ text: a, commit: "c1" }).m, { t: "edit", text: "A\nb\nc\nd\ne" }, { t: "flush" });
  r = run(r.m, { t: "save_fail", error: "the daemon did not answer in time", outstanding: true });
  r = run(r.m, { t: "reconcile_ok", doc: { text: a, commit: "c1" } }); // nothing landed yet; the request is still out
  r = run(r.m, { t: "edit", text: "a\nb\nc\nd\nE" }, { t: "flush" });
  assert.ok(r.fx.some((f) => f.t === "save"));
  r = run(r.m, { t: "save_ok", result: { kind: "saved", text: "A\nb\nc\nd\nE", commit: "c3", merged: true } });
  assert.equal(r.m.buffer, "a\nb\nc\nd\nE", "the revert stands; it is saved again on top");
  assert.equal(r.m.saved, "A\nb\nc\nd\nE");
});

test("a restore that completes after its tab was closed does not bring the tab back to write", () => {
  let r = run(loaded(), { t: "edit", text: "v2" }, { t: "gone" });
  r = run(r.m, { t: "close" });
  r = run(r.m, { t: "revive", head: { text: "v1", commit: "c9" } });
  assert.equal(r.fx.some((f) => f.t === "save" || f.t === "arm"), false);
  assert.equal(r.m.gone, true, "it stays with the stored draft for the next pane");
});

test("a refused save keeps its error visible when a reconcile read completes (#6372)", () => {
  let r = run(loaded(), { t: "edit", text: "mine" }, { t: "timer" }); // the save goes out
  r = run(r.m, { t: "save_fatal", error: "413 doc is larger than the limit" });
  assert.equal(statusOf(r.m).status, "error");
  assert.equal(r.m.fatal, true);
  // An earlier timed-out save is still owed a read: the daemon's head is read after the refusal.
  r.m.unknown = true;
  r = run(r.m, { t: "timer" });
  const read = r.fx.find((f) => f.t === "load");
  assert.ok(read, "the owed reconcile read starts under a refusal");
  r = run(r.m, { t: "reconcile_ok", doc: { text: "v1", commit: "c1" } });
  assert.equal(r.m.fatal, true, "the refusal still blocks the autosave");
  assert.equal(statusOf(r.m).status, "error", "the pane still says the save was refused, not that it is about to save");
});

test("an undo stands when the save it undid landed beside an agent's edits to the lines next to it", () => {
  const base = "a8\na11\na9\nu1\na3";
  const doubted = "u17\na11\na9\nu1\na3";
  // The save of `doubted` loses its connection, and the user undoes the line before the read comes back.
  let r = open({ text: base, commit: "c1" });
  r = run(r.m, { t: "edit", text: doubted }, { t: "timer" });
  r = run(r.m, { t: "save_fail", error: "cannot reach the daemon", outstanding: true });
  r = run(r.m, { t: "edit", text: base });
  // The read finds the save landed, with the agent's edits to the two lines after it. The merge of the
  // save onto this head is a conflict, because the edits touch, so it cannot tell the save landed.
  r = run(r.m, { t: "reconcile_ok", doc: { text: "u17\na15\na14\nu1\na3", commit: "c2" } });
  // The undo is not silently replaced by the landed text: it stands as the user's side of a conflict
  // with the agent's edits (their text is theirs to keep or take), where before it was reloaded away.
  assert.notEqual(r.m.buffer, "u17\na15\na14\nu1\na3", "the buffer is not reloaded over the undo");
  assert.equal(r.m.phase, "conflict");
  assert.equal(r.m.conflict?.ours, base, "the undo is the user's side");
  assert.equal(r.m.conflict?.theirs, "u17\na15\na14\nu1\na3");
});
