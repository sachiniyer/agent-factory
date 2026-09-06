import { test } from "node:test";
import assert from "node:assert/strict";
import { OptimisticSessions } from "./optimistic.js";
import { InFlightOp, Liveness, type SessionData } from "./types.js";

const first: SessionData = { id: "one", title: "One", branch: "main", liveness: Liveness.Ready,
  lifecycle_action: "archive", can_kill: true, worktree: { repo_path: "/repo" } };
const second: SessionData = { ...first, id: "two", title: "Two" };
const input = { title: "New", repoPath: "/repo", prompt: "Keep my prompt", program: "" };
function state(): OptimisticSessions {
  const model = new OptimisticSessions();
  model.reset([first, second]);
  return model;
}

test("create appears before an acknowledgement and rollback removes only its own placeholder", () => {
  const model = state();
  const a = model.beginCreate(input);
  const b = model.beginCreate(input);
  const rows = model.project();
  assert.equal(rows.length, 4);
  assert.equal(rows[2].in_flight_op, InFlightOp.Creating);
  assert.equal(rows[2].lifecycle_action, undefined);
  assert.equal(rows[2].can_kill, undefined);
  assert.equal(rows[2].worktree?.repo_path, "/repo");
  assert.notEqual(rows[2].id, rows[3].id);
  assert.equal(model.fail(a), true);
  assert.deepEqual(model.project(), [first, second, rows[3]]);
  assert.equal(model.succeed(b, { ...first, id: "new", title: "New-2" }), true);
  assert.deepEqual(model.project().map(row => row.id), ["one", "two", "new"]);
});

test("pending create survives stale and unrelated snapshots, then reconciles to daemon identity", () => {
  const model = state();
  const before = model.snapshotFence();
  const ticket = model.beginCreate(input);
  assert.equal(model.snapshot([first], before), false);
  assert.equal(model.snapshot([first], model.snapshotFence()), true);
  assert.equal(model.project().length, 2);
  const during = model.snapshotFence();
  model.succeed(ticket, { ...second, id: "new" });
  assert.equal(model.snapshot([first], during), false);
  assert.deepEqual(model.project().map(row => row.id), ["one", "new"]);
});

for (const [kind, op] of [["archive", InFlightOp.Archiving], ["kill", InFlightOp.Killing]] as const) {
  test(`${kind} projects before RPC and failure reveals latest authoritative row`, () => {
    const model = state();
    const ticket = model.begin(kind, first)!;
    assert.equal(model.project()[0].in_flight_op, op);
    assert.equal(model.project()[0].can_kill, false);
    assert.equal(model.project()[0].lifecycle_action, undefined);
    assert.equal(model.begin(kind, first), null);
    const changed = { ...first, branch: "updated-during-RPC" };
    model.event({ type: "session.updated", data: changed });
    assert.equal(model.project()[0].in_flight_op, op);
    model.fail(ticket);
    assert.deepEqual(model.project(), [changed, second]);
    assert.equal(model.fail(ticket), false);
  });

  test(`${kind} acknowledgement retains feedback until a fresh snapshot`, () => {
    const model = state();
    const ticket = model.begin(kind, first)!;
    const fence = model.snapshotFence();
    model.succeed(ticket);
    assert.equal(model.project()[0].in_flight_op, op);
    assert.equal(model.snapshot([first, second], fence), false);
    const finalRows = kind === "kill" ? [second] : [{ ...first, liveness: Liveness.Archived }, second];
    assert.equal(model.snapshot(finalRows, model.snapshotFence()), true);
    assert.deepEqual(model.project(), finalRows);
  });

  test(`${kind} completion event confirms before RPC; later failure cannot resurrect prior state`, () => {
    const model = state();
    const ticket = model.begin(kind, first)!;
    const data = kind === "archive" ? { ...first, liveness: Liveness.Archived } : first;
    model.event({ type: kind === "archive" ? "session.archived" : "session.killed", data });
    const confirmed = model.project();
    assert.deepEqual(confirmed, kind === "archive" ? [data, second] : [second]);
    assert.equal(model.fail(ticket), true);
    assert.deepEqual(model.project(), confirmed);
  });
}

test("overlapping operations on different sessions reconcile independently", () => {
  const model = state();
  const archive = model.begin("archive", first)!;
  const kill = model.begin("kill", second)!;
  model.succeed(archive);
  const archived = { ...first, liveness: Liveness.Archived };
  model.snapshot([archived, second], model.snapshotFence());
  assert.equal(model.project()[1].in_flight_op, InFlightOp.Killing);
  model.fail(kill);
  assert.deepEqual(model.project(), [archived, second]);
});

test("partial archive event requests resync and does not confirm a projected state", () => {
  const model = state();
  const ticket = model.begin("archive", first)!;
  assert.equal(model.event({ type: "session.archived", data: { id: "one", title: "One", branch: "" } }), true);
  assert.equal(model.project()[0].in_flight_op, InFlightOp.Archiving);
  model.fail(ticket);
  assert.deepEqual(model.project(), [first, second]);
});

test("same-credential disconnect fences late success/failure and stale snapshot", () => {
  const model = state();
  const create = model.beginCreate(input);
  const archive = model.begin("archive", first)!;
  const fence = model.snapshotFence();
  model.reset([second]);
  const next = model.beginCreate(input);
  assert.equal(model.succeed(create, first), false);
  assert.equal(model.fail(archive), false);
  assert.equal(model.snapshot([first], fence), false);
  assert.equal(model.isCurrent(next), true);
  assert.equal(model.project().length, 2);
  assert.equal(model.project()[0], second);
});

test("an event crossing a snapshot fences the older result", () => {
  const model = state();
  const fence = model.snapshotFence();
  model.event({ type: "session.killed", data: first });
  assert.equal(model.snapshot([first, second], fence), false);
  assert.deepEqual(model.project(), [second]);
});

test("create response replaces the placeholder without duplicating an already delivered event", () => {
  const model = state();
  const ticket = model.beginCreate(input);
  const created = { ...first, id: "created", title: "New" };
  model.event({ type: "session.created", data: created });
  model.succeed(ticket, created);
  assert.deepEqual(model.project(), [first, second, created]);
});

test("acknowledged kill is confirmed by its completion event without another snapshot", () => {
  const model = state();
  const ticket = model.begin("kill", first)!;
  model.succeed(ticket);
  model.event({ type: "session.killed", data: first });
  assert.equal(model.isCurrent(ticket), false);
  assert.deepEqual(model.project(), [second]);
});

test("one accepted snapshot fences another request started against older state", () => {
  const model = state();
  const fence = model.snapshotFence();
  assert.equal(model.snapshot([second], fence), true);
  assert.equal(model.snapshot([first, second], fence), false);
  assert.deepEqual(model.project(), [second]);
});

test("matching daemon feedback hides the placeholder without assigning RPC ownership", () => {
  const model = state();
  const ticket = model.beginCreate(input);
  const other = { ...first, id: "other-client", title: input.title, in_flight_op: InFlightOp.Creating };
  model.event({ type: "session.updated", data: other });
  assert.deepEqual(model.project(), [first, second, other]);
  model.fail(ticket);
  assert.deepEqual(model.project(), [first, second, other]);
});

test("creating a collision-suffixed title still shows feedback beside its existing namesake", () => {
  const model = state();
  model.beginCreate({ ...input, title: first.title });
  assert.equal(model.project().length, 3);
  assert.equal(model.project()[2].in_flight_op, InFlightOp.Creating);
});

test("one authoritative Creating row only coalesces one of two pending requests", () => {
  const model = state();
  const a = model.beginCreate(input);
  const b = model.beginCreate(input);
  const creating = { ...first, id: "new", title: input.title, in_flight_op: InFlightOp.Creating };
  model.event({ type: "session.updated", data: creating });
  assert.equal(model.project().length, 4);
  assert.equal(model.project().filter(row => row.id?.startsWith("optimistic:")).length, 1);
  model.succeed(a, { ...creating, in_flight_op: InFlightOp.None });
  assert.equal(model.project().length, 4);
  assert.equal(model.project().filter(row => row.id?.startsWith("optimistic:")).length, 1);
  model.fail(b);
  assert.equal(model.project().length, 3);
});

test("completed same-title row never hides another pending request", () => {
  const model = state();
  model.beginCreate(input);
  model.event({ type: "session.created", data: { ...first, id: "other", title: input.title } });
  assert.equal(model.project().length, 4);
  assert.equal(model.project()[3].in_flight_op, InFlightOp.Creating);
});

test("delayed create response cannot resurrect a session killed after its create event", () => {
  const model = state();
  const ticket = model.beginCreate(input);
  const created = { ...first, id: "new", title: input.title };
  model.event({ type: "session.created", data: created });
  model.event({ type: "session.killed", data: created });
  model.succeed(ticket, created);
  assert.deepEqual(model.project(), [first, second]);
});

test("delayed create response preserves a newer completed update", () => {
  const model = state();
  const ticket = model.beginCreate(input);
  const created = { ...first, id: "new", title: input.title };
  const updated = { ...created, branch: "changed-after-create" };
  model.event({ type: "session.created", data: created });
  model.event({ type: "session.updated", data: updated });
  model.succeed(ticket, created);
  assert.deepEqual(model.project(), [first, second, updated]);
});

test("create response replaces the earlier provisional Creating projection", () => {
  const model = state();
  const ticket = model.beginCreate(input);
  const created = { ...first, id: "new", title: input.title };
  model.event({ type: "session.updated", data: { ...created, in_flight_op: InFlightOp.Creating } });
  model.succeed(ticket, created);
  assert.deepEqual(model.project(), [first, second, created]);
});
