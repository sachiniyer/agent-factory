import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

// Drives index.ts's real openRebindProject / commitRegisteredProjects /
// takeRebindFollow in a sandbox, the same extract-and-run harness
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
interface Deferred { resolve(p: Project): void; reject(e: Error): void }
interface State {
  registeredProjects: Project[];
  selectedProject: string | null;
  sessions: unknown[];
  tasks: unknown[];
}

/** An error the stubbed api classifies: "refused" is a definitive daemon refusal,
 *  "uncertain" a lost reply / unverified intermediary error. */
class StubError extends Error {
  constructor(message: string, readonly kind: "refused" | "uncertain") { super(message); }
}

function harness(
  initial: { registeredProjects: Project[]; selectedProject: string | null },
  opts: { storage?: boolean } = {},
) {
  const storage = opts.storage ?? true;
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const state: State = { ...initial, sessions: [], tasks: [] };
  let persisted: string | null = storage ? initial.selectedProject : null;
  const events: string[] = [];
  const submits: Array<(path: string) => void> = [];
  const pending: Deferred[] = [];
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
    rebindProject: (id: string, path: string) => {
      events.push(`rpc:${id}:${path}`);
      return new Promise<Project>((resolve, reject) => pending.push({ resolve, reject }));
    },
    // Mirrors index.ts switchProject: persist (when storage works), and count an
    // explicit switch only when the selection really changes.
    switchProject: (root: string) => {
      events.push(`switch:${root}`);
      if (storage) persisted = root;
      if (state.selectedProject === root) return;
      (context as unknown as { projectChoiceGeneration: number }).projectChoiceGeneration++;
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
    isMutationCommittedError: () => false,
    isMutationOutcomeUncertain: (e: StubError) => e.kind === "uncertain",
    errorText: (e: Error) => e.message,
    listDirectory: () => Promise.resolve({}),
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null, rebindInFlight = null, rebindFollow = null, rebindAttempts = 0;
    var projectChoiceGeneration = 0; // var: shared with the harness's switchProject stub
    const REBIND_ANSWER_MS = 30000;
    function openModal(next) { modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${topLevelFunction(source, "takeRebindFollow")}
    ${topLevelFunction(source, "commitRegisteredProjects")}
    ${topLevelFunction(source, "openRebindProject")}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);
  const app = context as typeof context & {
    openRebindProject(id: string, label: string): void;
    commitRegisteredProjects(projects: Project[]): void;
    closeModal(): void;
  };
  return { app, state, events, submits, pending, fire, timers };
}

const settle = () => new Promise(resolve => setImmediate(resolve));

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

test("a definitive refusal re-arms the open modal inline", async () => {
  const { app, events, submits, pending } = harness({ registeredProjects: [{ id: "prj_A", root: "/old" }], selectedProject: "/old" });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/taken");
  pending[0].reject(new StubError("path is already bound to another project", "refused"));
  await settle();

  assert.deepEqual(events.slice(-2), ["busy:1:false", "error:1:path is already bound to another project"]);
});

test("an uncertain outcome closes the modal, re-reads the registry, and does not re-arm", async () => {
  const { app, events, submits, pending } = harness({ registeredProjects: [{ id: "prj_A", root: "/old" }], selectedProject: "/old" });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  pending[0].reject(new StubError("connection reset", "uncertain"));
  await settle();

  assert.ok(!events.includes("busy:1:false"), "an uncertain outcome must not re-enable the form");
  assert.ok(events.includes("close:1"), "the form closes so a second path cannot be submitted into it");
  assert.ok(events.includes("refetch"), "the registry is re-read to learn what actually happened");
  assert.ok(events.some(e => e.startsWith("outcome:uncertain:") && e.includes("alpha")), "the notice says the outcome is unknown");
});

test("a successful rebind follows the selection to the root the registry read reports", async () => {
  const { app, state, submits, pending, events } = harness({
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  assert.ok(events.includes("refetch"), "success must trigger a fenced registry read");

  app.commitRegisteredProjects([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);
  assert.equal(state.selectedProject, "/new", "the selection must follow the registration to its new root");
});

test("a newer registry state is not overwritten by the rebind's own echo", async () => {
  const { app, state, submits, pending } = harness({
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  // Another client deletes prj_A after this rebind commits, and that refetch
  // lands before this request's delayed reply.
  app.commitRegisteredProjects([{ id: "prj_B", root: "/other" }]);
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();

  assert.deepEqual(state.registeredProjects.map(p => p.root), ["/other"], "the echo must not resurrect a deleted record");

  // The follow-up read still does not know prj_A: nothing to follow, no phantom.
  app.commitRegisteredProjects([{ id: "prj_B", root: "/other" }]);
  assert.notEqual(state.selectedProject, "/new");
});

test("a rebind does not move a selection the user changed mid-flight", async () => {
  const { app, state, submits, pending } = harness({
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  app.closeModal();
  // The user explicitly switched projects while the daemon decided.
  (app as unknown as { switchProject(r: string): void }).switchProject("/other");
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  app.commitRegisteredProjects([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);

  assert.equal(state.selectedProject, "/other");
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
  const outcomes = events.filter(e => e.startsWith("outcome:"));
  assert.equal(outcomes.length, 1);
  assert.match(outcomes[0], /^outcome:uncertain:.*alpha.*unknown/, "the notice says the outcome is unknown, not success or failure");
  assert.ok(!events.some(e => e.startsWith("error:") || e.startsWith("toast:")), "no failure is claimed");

  app.openRebindProject("prj_A", "alpha");
  assert.equal(submits.length, 2, "the in-flight guard must not outlive the attempt");

  // A reply that finally arrives only re-reads the registry: no second notice,
  // and it must not touch the new attempt's guard.
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

test("a rebind that commits after the bounded wait still moves the selection", async () => {
  const { app, state, submits, pending, fire } = harness({
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  fire(); // 30s pass: outcome unknown, a registry read starts
  // That read lands before the daemon commits: the record still names /old.
  app.commitRegisteredProjects([{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }]);
  assert.equal(state.selectedProject, "/old");

  // The daemon commits; its late reply triggers the read that observes the move.
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  app.commitRegisteredProjects([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);

  assert.equal(state.selectedProject, "/new", "the follow intent must survive a read that has not seen the move yet");
});

test("a late definitive refusal disarms the follow intent", async () => {
  const { app, state, submits, pending, fire } = harness({
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  fire();
  app.commitRegisteredProjects([{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }]);
  pending[0].reject(new StubError("path is already bound to another project", "refused"));
  await settle();

  // Much later, another client moves the record. This rebind never landed, so
  // its intent must not drag the selection along.
  app.commitRegisteredProjects([{ id: "prj_A", root: "/elsewhere" }, { id: "prj_B", root: "/other" }]);
  assert.notEqual(state.selectedProject, "/elsewhere");
});

test("a late refusal from an older attempt cannot clear a newer attempt's follow", async () => {
  const { app, state, submits, pending, fire } = harness({
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  });

  // Attempt A outlives the bounded wait, which releases the guard.
  app.openRebindProject("prj_A", "alpha");
  submits[0]("/first");
  fire();
  // Attempt B, for the same project, succeeds and arms its own follow.
  app.openRebindProject("prj_A", "alpha");
  submits[1]("/second");
  pending[1].resolve({ id: "prj_A", root: "/second" });
  await settle();
  // A's definitive refusal arrives before B's registry read commits.
  pending[0].reject(new StubError("path is already bound to another project", "refused"));
  await settle();

  app.commitRegisteredProjects([{ id: "prj_A", root: "/second" }, { id: "prj_B", root: "/other" }]);
  assert.equal(state.selectedProject, "/second", "B's follow must survive A's late refusal");
});

test("a late success from an older attempt cannot replace a newer attempt's follow", async () => {
  const { app, state, submits, pending, fire } = harness({
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  });

  // Attempt A (project A) outlives the bounded wait, which releases the guard.
  app.openRebindProject("prj_A", "alpha");
  submits[0]("/first");
  fire();
  // The user moves to project B and rebinds it; B succeeds and arms its follow.
  (app as unknown as { switchProject(r: string): void }).switchProject("/other");
  app.openRebindProject("prj_B", "beta");
  submits[1]("/other-new");
  pending[1].resolve({ id: "prj_B", root: "/other-new" });
  await settle();
  // A finally answers, late, with a success.
  pending[0].resolve({ id: "prj_A", root: "/first" });
  await settle();

  app.commitRegisteredProjects([{ id: "prj_A", root: "/first" }, { id: "prj_B", root: "/other-new" }]);
  assert.equal(state.selectedProject, "/other-new", "B's follow must survive A's late success");
});

test("without local storage, a reconcile fallback does not cancel the follow", async () => {
  const { app, state, submits, pending } = harness(
    { registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }], selectedProject: "/old" },
    { storage: false },
  );

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  // projects.changed lands before the HTTP reply: that read already drops /old,
  // and reconciliation (no persisted choice to fall back on) moves the selection.
  app.commitRegisteredProjects([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);
  assert.notEqual(state.selectedProject, "/old", "precondition: reconciliation moved off the vanished root");

  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();
  app.commitRegisteredProjects([{ id: "prj_A", root: "/new" }, { id: "prj_B", root: "/other" }]);

  assert.equal(state.selectedProject, "/new", "a reconcile fallback is not the user leaving; the follow must still land");
});

test("a confirmed rebind that changed nothing does not follow a later, unrelated move", async () => {
  const { app, state, submits, pending } = harness({
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  });

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/old/via-symlink");
  // The daemon canonicalizes it to the root the record already has.
  pending[0].resolve({ id: "prj_A", root: "/old" });
  await settle();
  app.commitRegisteredProjects([{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }]);
  assert.equal(state.selectedProject, "/old");

  // Much later another client rebinds prj_A. This attempt is long settled.
  app.commitRegisteredProjects([{ id: "prj_A", root: "/elsewhere" }, { id: "prj_B", root: "/other" }]);
  assert.notEqual(state.selectedProject, "/elsewhere", "a settled no-op rebind must not claim someone else's move");
});
