import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

// Drives index.ts's real openRebindProject / applyReboundProject in a sandbox,
// the same extract-and-run harness remove_task_committed.test.ts uses, so the
// test exercises the shipped handler rather than a copy of its logic.

function topLevelFunction(source: string, name: string): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  const node = ast.statements.find(statement =>
    ts.isFunctionDeclaration(statement) && statement.name?.text === name);
  assert.ok(node, `index.ts must declare ${name}`);
  return node.getText(ast);
}

interface Project { id: string; root: string }
interface Deferred { resolve(p: Project): void; reject(e: Error): void }

function harness(state: { registeredProjects: Project[]; selectedProject: string | null }) {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const events: string[] = [];
  const submits: Array<(path: string) => void> = [];
  const pending: Deferred[] = [];
  const context = {
    events,
    store: {
      get: () => state,
      set: (patch: Partial<typeof state>) => Object.assign(state, patch),
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
    switchProject: (root: string) => {
      events.push(`switch:${root}`);
      state.selectedProject = root;
    },
    showTransientNotice: (msg: string) => events.push(`notice:${msg}`),
    surfaceTabError: (e: Error) => events.push(`toast:${e.message}`),
    refreshRegisteredProjects: () => events.push("refresh"),
    isMutationCommittedError: () => false,
    errorText: (e: Error) => e.message,
    listDirectory: () => Promise.resolve({}),
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null, rebindInFlight = null;
    function openModal(next) { modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${topLevelFunction(source, "applyReboundProject")}
    ${topLevelFunction(source, "openRebindProject")}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);
  const app = context as typeof context & {
    openRebindProject(id: string, label: string): void;
    closeModal(): void;
  };
  return { app, events, submits, pending };
}

const settle = () => new Promise(resolve => setImmediate(resolve));

test("escape mid-rebind cannot reopen rebind and race a second mutation", async () => {
  const state = { registeredProjects: [{ id: "prj_A", root: "/old" }], selectedProject: "/old" };
  const { app, events, submits, pending } = harness(state);

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

test("a rejection after the modal was dismissed surfaces as a toast, not in a later modal", async () => {
  const state = { registeredProjects: [{ id: "prj_A", root: "/old" }], selectedProject: "/old" };
  const { app, events, submits, pending } = harness(state);

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/taken");
  app.closeModal();
  pending[0].reject(new Error("path is already bound to another project"));
  await settle();

  assert.ok(events.includes("toast:path is already bound to another project"));
  assert.ok(!events.some(e => e.startsWith("error:")), "a dismissed modal's error must not be set inline");
});

test("a successful rebind keeps the selected project on its rebound root", async () => {
  const state = {
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  };
  const { app, submits, pending } = harness(state);

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();

  assert.equal(state.selectedProject, "/new", "the selection must follow the registration to its new root");
  assert.deepEqual(
    state.registeredProjects.map(p => p.root),
    ["/new", "/other"],
    "the record keeps its id and takes the new root before the refetch lands",
  );
});

test("a rebind does not move a selection the user changed mid-flight", async () => {
  const state = {
    registeredProjects: [{ id: "prj_A", root: "/old" }, { id: "prj_B", root: "/other" }],
    selectedProject: "/old",
  };
  const { app, submits, pending } = harness(state);

  app.openRebindProject("prj_A", "alpha");
  submits[0]("/new");
  app.closeModal();
  state.selectedProject = "/other"; // the user switched projects while the daemon decided
  pending[0].resolve({ id: "prj_A", root: "/new" });
  await settle();

  assert.equal(state.selectedProject, "/other");
});
