import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

// Drives index.ts's real openRebindProject / followConfirmedRebind /
// commitRegisteredProjects in a sandbox, the same extract-and-run harness
// remove_task_committed.test.ts uses, so the test exercises the shipped handlers
// rather than a copy of their logic.

function topLevelFunction(source: string, name: string): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  const node = ast.statements.find(statement =>
    ts.isFunctionDeclaration(statement) && statement.name?.text === name);
  assert.ok(node, `index.ts must declare ${name}`);
  return node.getText(ast);
}

interface Project { id: string; root: string }
interface Deferred<T> { resolve(v: T): void; reject(e: Error): void }
interface State {
  registeredProjects: Project[];
  selectedProject: string | null;
  sessions: unknown[];
  tasks: unknown[];
}

/** An error the stubbed api classifies: "refused" is a definitive daemon refusal,
 *  "uncertain" a lost reply / unverified intermediary error, "committed" a rebind
 *  that landed before a follow-up step failed, and "rebound" the definitive
 *  refusal of a rebind whose expected root another rebind moved (#4822). */
class StubError extends Error {
  constructor(message: string, readonly kind: "refused" | "uncertain" | "committed" | "rebound") { super(message); }
}

const UNKNOWN = (label: string) => `outcome:uncertain:Rebind of ${label} · outcome unknown · check the project list`;

function harness(initial: { registeredProjects: Project[]; selectedProject: string | null }) {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const state: State = { ...initial, sessions: [], tasks: [] };
  let persisted: string | null = initial.selectedProject;
  const events: string[] = [];
  const submits: Array<(path: string) => void> = [];
  // The expected root each rebind request carried, in order.
  const expected: Array<string | null> = [];
  const pending: Array<Deferred<Project>> = [];
  // Each followConfirmedRebind registry read, answered by the test.
  const reads: Array<Deferred<Project[]>> = [];
  // Controllable timers: fire() runs every armed callback, as if the bounded wait
  // elapsed; clearTimeout disarms one.
  const timers = new Map<number, () => void>();
  let nextTimer = 1;
  const fire = () => { for (const [id, fn] of [...timers]) { timers.delete(id); fn(); } };
  const context = {
    window: {
      setTimeout: (fn: () => void) => { const id = nextTimer++; timers.set(id, fn); return id; },
      clearTimeout: (id: number) => { timers.delete(id); },
    },
    store: {
      get: () => state,
      set: (patch: Partial<State>) => Object.assign(state, patch),
    },
    rebindProjectModal: (opts: { onSubmit: (path: string) => void }) => {
      submits.push(opts.onSubmit);
      const n = submits.length;
      return {
        close: () => events.push(`close:${n}`),
        setBusy: (busy: boolean) => events.push(`busy:${n}:${busy}`),
        setError: (msg: string) => events.push(`error:${n}:${msg}`),
      };
    },
    rebindProject: (id: string, path: string, _token: string, expectedRoot: string | null) => {
      events.push(`rpc:${id}:${path}`);
      expected.push(expectedRoot);
      return new Promise<Project>((resolve, reject) => pending.push({ resolve, reject }));
    },
    listProjects: () => {
      events.push("read");
      return new Promise<Project[]>((resolve, reject) => reads.push({ resolve, reject }));
    },
    switchProject: (root: string) => {
      events.push(`switch:${root}`);
      persisted = root;
      state.selectedProject = root;
    },
    loadProjectChoice: () => persisted,
    projectRoots: (projects: Project[]) => projects.map(p => p.root),
    // The real reconcile's shape: keep current if valid, else persisted, else none.
    reconcileProject: (_s: unknown, _t: unknown, saved: string | null, current: string | null, roots: string[]) =>
      current && roots.includes(current) ? current : saved && roots.includes(saved) ? saved : null,
    showTransientNotice: (msg: string) => events.push(`notice:${msg}`),
    surfaceTabError: (e: Error) => events.push(`toast:${e.message}`),
    surfaceMutationError: (e: Error, kind = "failed") => events.push(`outcome:${kind}:${e.message}`),
    refreshRegisteredProjects: () => events.push("refetch"),
    isMutationCommittedError: (e: StubError) => e.kind === "committed",
    isMutationOutcomeUncertain: (e: StubError) => e.kind !== "refused" && e.kind !== "rebound",
    isProjectReboundError: (e: StubError) => e.kind === "rebound",
    errorText: (e: Error) => e.message,
    listDirectory: () => Promise.resolve({}),
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null, rebindInFlight = null;
    var connectionGeneration = 0, rebindInFlightGeneration = 0; // var: tests bump the connection
    const REBIND_ANSWER_MS = 30000;
    function openModal(next) { modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${topLevelFunction(source, "rebindOutcomeUnknown")}
    ${topLevelFunction(source, "followConfirmedRebind")}
    ${topLevelFunction(source, "commitRegisteredProjects")}
    ${topLevelFunction(source, "openRebindProject")}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);
  const app = context as typeof context & {
    openRebindProject(id: string, label: string): void;
    commitRegisteredProjects(projects: Project[]): void;
    closeModal(): void;
  };
  // Simulates a reconnect (connect() bumps the generation too, without going
  // through disconnect()), so the fence is tested on its own.
  const reconnect = () => { (context as unknown as { connectionGeneration: number }).connectionGeneration++; };
  return { app, state, events, submits, pending, reads, fire, timers, reconnect, expected };
}

const settle = () => new Promise(resolve => setImmediate(resolve));
const twoProjects = () => ({
  registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
  selectedProject: "/old",
});

// --- single flight and stale replies -------------------------------------------

test("escape mid-rebind cannot reopen rebind and race a second mutation", async () => {
  const { app, events, submits, pending } = harness({ registeredProjects: [{ id: "prj_A", root: "/old" }], selectedProject: "/old" });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/first");
  app.closeModal(); // Escape: the modal is disposed while the RPC is still running

  app.openRebindProject("prj_A", "alpha");
  assert.equal(submits.length, 1, "Rebind must not reopen while the first request is in flight");
  assert.equal(events.filter(e => e.startsWith("rpc:")).length, 1, "only one registry mutation may be in flight");
  assert.ok(events.some(e => e.startsWith("notice:") && e.includes("alpha")), "the refusal must say which rebind is running");

  pending[0].resolve({ id: "prj_A", root: "/first" });
  await settle();
  app.openRebindProject("prj_A", "alpha");
  assert.equal(submits.length, 2, "once the first rebind settles, Rebind opens again");
});

test("a refusal after the modal was dismissed surfaces as a toast, not in a later modal", async () => {
  const { app, events, submits, pending } = harness({ registeredProjects: [{ id: "prj_A", root: "/old" }], selectedProject: "/old" });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/taken");
  app.closeModal();
  pending[0].reject(new StubError("path is already bound to another project", "refused"));
  await settle();

  assert.ok(events.includes("toast:path is already bound to another project"));
  assert.ok(!events.some(e => e.startsWith("error:")), "a dismissed modal's error must not be set inline");
});

test("a rebind from a previous connection neither blocks nor acts on the new one", async () => {
  const { app, state, events, submits, pending, fire, reconnect } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/first");
  app.closeModal();
  reconnect();

  app.openRebindProject("prj_A", "alpha");
  assert.equal(submits.length, 2, "an old connection's in-flight guard must not block the new connection");

  const before = events.length;
  fire(); // the old attempt's bounded wait
  pending[0].resolve({ id: "prj_A", root: "/first" }); // and its late reply
  await settle();
  assert.deepEqual(events.slice(before).filter(e => !e.startsWith("busy:")), [],
    "the old attempt must not notify, refetch, read, or follow on the new connection");
  assert.equal(state.selectedProject, "/old");
});

// --- definitive refusal: inline error, re-armed form, no follow ---------------

test("a definitive refusal re-arms the open modal inline", async () => {
  const { app, state, events, submits, pending } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/taken");
  pending[0].reject(new StubError("path is already bound to another project", "refused"));
  await settle();

  assert.deepEqual(events.slice(-2), ["busy:1:false", "error:1:path is already bound to another project"]);
  assert.ok(!events.includes("read"), "a refusal changed nothing: nothing to follow");
  assert.equal(state.selectedProject, "/old");
});

// --- confirmed, in-time success: follow the registry, never the echo ----------

test("a successful rebind follows the selection to the root the registry read reports", async () => {
  const { app, state, submits, pending, reads, events } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  assert.ok(events.includes("close:1"));
  assert.equal(reads.length, 1, "a confirmed success reads the registry once, after the reply");

  reads[0].resolve([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);
  await settle();
  assert.equal(state.selectedProject, "/new", "the selection must follow the registration to its new root");
  assert.ok(events.includes("refetch"), "a fenced refetch follows to settle anything that raced the read");
});

test("a newer registry state is not overwritten by the rebind's own echo", async () => {
  const { app, state, submits, pending, reads } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  // Another client rebinds prj_A again after this request commits; the reply
  // still echoes /new, but the registry read says /newer.
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  reads[0].resolve([{ id: "prj_A", root: "/newer" }, { id: "prj_B", root: "/other" }]);
  await settle();

  assert.equal(state.selectedProject, "/newer", "follow the registry's current binding, not the echo");
  assert.deepEqual(state.registeredProjects.map(p => p.root), ["/newer", "/other"]);
});

test("a confirmed success does not follow a record the registry no longer has", async () => {
  const { app, state, submits, pending, reads } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  reads[0].resolve([{ id: "prj_B", root: "/other" }]); // deleted by another client meanwhile
  await settle();

  assert.notEqual(state.selectedProject, "/new", "a deleted record must not be resurrected by the echo");
});

test("without local storage, a reconcile fallback before the reply does not stop the follow", async () => {
  const { app, state, submits, pending, reads } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  // projects.changed lands before the HTTP reply: that read already drops /old
  // and reconciliation moves the selection. The modal is still open — the user
  // is still in this rebind — so the confirmed success still follows.
  app.commitRegisteredProjects([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  reads[0].resolve([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);
  await settle();

  assert.equal(state.selectedProject, "/new");
});

test("a rebind does not move a selection the user changed mid-flight", async () => {
  const { app, state, submits, pending, events } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  app.closeModal();
  // The user explicitly switched projects while the daemon decided.
  (app as unknown as { switchProject(r: string): void }).switchProject("/other");
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();

  assert.equal(state.selectedProject, "/other");
  assert.ok(!events.includes("read"), "no follow read for a user who has left this rebind");
  assert.ok(events.includes("refetch"), "the registry is still re-read");
});

test("a committed-then-failed rebind follows like a success and reports the follow-up failure", async () => {
  const { app, state, events, submits, pending, reads } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  pending[0].reject(new StubError("rebind committed, but publishing projects.changed failed", "committed"));
  await settle();
  reads[0].resolve([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);
  await settle();

  assert.equal(state.selectedProject, "/new");
  assert.ok(events.includes("outcome:confirmed:rebind committed, but publishing projects.changed failed"));
});

// --- unknown outcome: no follow, re-read, one notice ---------------------------

test("an uncertain outcome closes the modal, re-reads the registry, and neither re-arms nor follows", async () => {
  const { app, state, events, submits, pending } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  pending[0].reject(new StubError("connection reset", "uncertain"));
  await settle();

  assert.ok(!events.includes("busy:1:false"), "an uncertain outcome must not re-enable the form");
  assert.ok(events.includes("close:1"), "the form closes so a second path cannot be submitted into it");
  assert.ok(events.includes("refetch"), "the registry is re-read to learn what actually happened");
  assert.ok(!events.includes("read"), "no follow on an unknown outcome");
  assert.deepEqual(events.filter(e => e.startsWith("outcome:")), [UNKNOWN("alpha")]);

  // Even if the registry shows the move, the selection is left where it was.
  app.commitRegisteredProjects([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);
  assert.notEqual(state.selectedProject, "/new");
});

test("a rebind that never answers releases the guard and reports the outcome as unknown", async () => {
  const { app, events, submits, pending, fire } = harness({ registeredProjects: [{ id: "prj_A", root: "/old" }], selectedProject: "/old" });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  // The request never settles: no reply, no rejection. Only the bounded wait runs.
  fire();
  await settle();

  assert.ok(events.includes("close:1"), "the stalled form closes");
  assert.ok(events.includes("refetch"), "the registry is re-read so the project list shows the truth");
  assert.deepEqual(events.filter(e => e.startsWith("outcome:")), [UNKNOWN("alpha")],
    "one notice: the outcome is unknown, not success or failure");
  assert.ok(!events.some(e => e.startsWith("error:") || e.startsWith("toast:")), "no failure is claimed");

  app.openRebindProject("prj_A", "alpha");
  assert.equal(submits.length, 2, "the in-flight guard must not outlive the attempt");

  // A reply that finally arrives only re-reads the registry: no second notice,
  // no follow, and it must not touch the new attempt's guard.
  const before = events.length;
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  assert.deepEqual(events.slice(before), ["refetch"]);
  submits[1]("/newer");
  assert.equal(events.filter(e => e.startsWith("rpc:")).length, 2, "the second attempt is still admitted");
});

test("a rebind that answers in time disarms the bounded wait", async () => {
  const { app, submits, pending, timers } = harness({ registeredProjects: [{ id: "prj_A", root: "/old" }], selectedProject: "/old" });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  assert.equal(timers.size, 1, "submitting arms the bounded wait");
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  assert.equal(timers.size, 0, "a settled attempt leaves no timer to report a stale unknown outcome");
});

test("a late success after the bounded wait does not auto-follow", async () => {
  const { app, state, events, submits, pending, fire } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  fire(); // 30s pass: outcome unknown, the registry is re-read
  pending[0].resolve({ id: "prj_A", root: "/new" }); // the daemon commits, late
  await settle();
  app.commitRegisteredProjects([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);

  assert.ok(!events.includes("read"), "a late reply never starts a follow");
  assert.notEqual(state.selectedProject, "/new", "the user was told to check the project list; nothing moves them");
});

// --- rebound elsewhere (#4822): a definitive refusal, re-armed after a refresh --

const REBOUND = "rebind project: project prj_A was rebound elsewhere: it is now bound to /theirs, not /old — refresh and retry";

test("a rebind carries the root the switcher showed as its expected root", async () => {
  const { app, submits, expected } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");

  assert.deepEqual(expected, ["/old"], "the daemon can only refuse a stale rebind if it is told what the client saw");
});

test("a rebound-elsewhere refusal re-reads the registry, then re-arms against the current root", async () => {
  const { app, state, events, submits, pending, reads, expected } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/mine");
  pending[0].reject(new StubError(REBOUND, "rebound"));
  await settle();

  assert.ok(events.includes("refetch"), "the registry is refreshed");
  assert.ok(!events.includes("close:1"), "a refusal keeps the form open");
  assert.ok(!events.includes("busy:1:false"), "the form re-arms only after the refresh answers");
  assert.ok(!events.some(e => e.startsWith("outcome:")), "a refusal is not reported as an unknown outcome");

  reads[0].resolve([{ id: "prj_A", root: "/theirs" }, { id: "prj_B", root: "/other" }]);
  await settle();
  assert.deepEqual(events.slice(-2), [
    "busy:1:false",
    "error:1:Rebound elsewhere, to /theirs · submit again to move it from there",
  ]);
  assert.equal(state.selectedProject, "/old", "a refusal changed nothing: no follow");

  submits[0]("/mine");
  assert.deepEqual(expected, ["/old", "/theirs"], "the retry expects the root the refresh found");
});

test("a rebound-elsewhere refusal still re-arms when the refresh fails", async () => {
  const { app, events, submits, pending, reads } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/mine");
  pending[0].reject(new StubError(REBOUND, "rebound"));
  await settle();
  reads[0].reject(new Error("network down"));
  await settle();

  assert.deepEqual(events.slice(-2), ["busy:1:false", `error:1:${REBOUND}`]);
});

// A control: a dismissed modal's refusal was already a toast before #4822, and
// the refresh step must not change where it lands.
test("a rebound-elsewhere refusal after the modal was dismissed surfaces as a toast", async () => {
  const { app, events, submits, pending, reads } = harness(twoProjects());

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/mine");
  app.closeModal();
  pending[0].reject(new StubError(REBOUND, "rebound"));
  await settle();
  reads[0]?.resolve([{ id: "prj_A", root: "/theirs" }]);
  await settle();

  assert.ok(events.includes(`toast:${REBOUND}`));
  assert.ok(!events.some(e => e.startsWith("error:")), "a dismissed modal's error must not be set inline");
});
