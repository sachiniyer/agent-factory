import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

// Drives index.ts's real openAddProject — both its path half and its W9.10
// git-URL half (project.add.url) — in a sandbox, the same extract-and-run
// harness rebind_project_flow.test.ts uses, so the test exercises the shipped
// handler rather than a copy of its logic. The URL detector is the REAL one
// extracted from api.ts, so the routing table the test pins is the one
// production runs — a drifted SCP_LIKE_GIT_URL copy would pass a stubbed
// detector while silently routing real input to the wrong request field.

function topLevelFunction(source: string, name: string): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  const node = ast.statements.find(statement =>
    ts.isFunctionDeclaration(statement) && statement.name?.text === name);
  assert.ok(node, `index.ts must declare ${name}`);
  // `export` survives getText; the sandbox runs a script, not a module.
  return node.getText(ast).replace(/^export\s+/, "");
}

/** Pulls one module-level const line verbatim — the real routing regex, not a
 *  re-typed copy that could drift from it. */
function topLevelConst(source: string, name: string): string {
  const line = new RegExp(`^const ${name} = .+$`, "m").exec(source);
  assert.ok(line, `api.ts must declare ${name}`);
  return line[0];
}

interface Project { id: string; root: string }
interface Deferred<T> { resolve(v: T): void; reject(e: Error): void }

/** An error the stubbed api classifies: "refused" is a definitive daemon
 *  refusal (daemon_rejected envelope), "uncertain" a lost reply / transport
 *  failure the daemon may still be acting on. */
class StubError extends Error {
  constructor(message: string, readonly kind: "refused" | "uncertain") { super(message); }
}

function harness() {
  const indexSource = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const apiSource = readFileSync(new URL("./api.ts", import.meta.url), "utf8");
  const events: string[] = [];
  const submits: Array<(value: string) => void> = [];
  // The in-flight RPCs, answered by the test.
  const pendingPaths: Array<Deferred<Project>> = [];
  const pendingURLs: Array<Deferred<Project>> = [];
  const context = {
    store: {
      get: () => ({}),
      set: () => undefined,
    },
    addProjectModal: (opts: { onSubmit: (value: string) => void }) => {
      submits.push(opts.onSubmit);
      const n = submits.length;
      return {
        // el is the backdrop a remount refocuses into; a bare stand-in suffices.
        el: { querySelector: () => null },
        close: () => events.push(`close:${n}`),
        setBusy: (busy: boolean) => events.push(`busy:${n}:${busy}`),
        setError: (msg: string) => events.push(`error:${n}:${msg}`),
      };
    },
    listDirectory: () => Promise.resolve({}),
    registerProject: (path: string, _token: string) => {
      events.push(`rpc:path:${path}`);
      return new Promise<Project>((resolve, reject) => pendingPaths.push({ resolve, reject }));
    },
    registerProjectURL: (url: string, _token: string) => {
      events.push(`rpc:url:${url}`);
      return new Promise<Project>((resolve, reject) => pendingURLs.push({ resolve, reject }));
    },
    showTransientNotice: (msg: string) => events.push(`notice:${msg}`),
    surfaceTabError: (e: Error) => events.push(`toast:${e.message}`),
    surfaceMutationError: (e: Error, kind = "failed") => events.push(`outcome:${kind}:${e.message}`),
    refreshRegisteredProjects: () => events.push("refetch"),
    isMutationOutcomeUncertain: (e: StubError) => e.kind !== "refused",
    errorText: (e: Error) => e.message,
    captureModalInvoker: () => ({ actionLabel: null, header: false }),
    // Non-modal overlay owners — the remount guard must not reap them. Context
    // properties (not prelude lets) so a test can open one mid-flight.
    configAssistant: null as { close(): void } | null,
    accountLogin: null as { close(): void } | null,
    // The sandbox prelude calls back through this so a remount is observable —
    // the difference between "the refused form came back" and a dropped error.
    recordEvent: (msg: string) => events.push(msg),
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null;
    function openModal(next) { recordEvent("openModal"); modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${topLevelConst(apiSource, "SCP_LIKE_GIT_URL")}
    ${topLevelFunction(apiSource, "looksLikeGitURL")}
    ${topLevelFunction(indexSource, "openAddProject")}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);
  const app = context as typeof context & {
    openAddProject(): void;
    looksLikeGitURL(value: string): boolean;
  };
  return { app, context, events, submits, pendingPaths, pendingURLs };
}

const settle = () => new Promise(resolve => setImmediate(resolve));

// --- the routing rule mirrors projectclone.LooksLikeURL ---------------------
//
// The Go test table (internal/projectclone TestLooksLikeURLRoutesPathsAndURLs)
// is the contract all three surfaces route on: scheme-bearing or scp-like is a
// URL — INCLUDING file://, which routing must still send to the daemon's
// Validate for its own refusal — and anything else stays a path.

test("the URL routing table matches projectclone.LooksLikeURL", () => {
  const { app } = harness();
  for (const u of ["https://x/y", "file:///etc", "ssh://h/p", "git@github.com:o/r.git"]) {
    assert.equal(app.looksLikeGitURL(u), true, u);
  }
  for (const p of [".", "./repo", "/srv/repo", "~/repo", "repo", "C:repo", "a:b"]) {
    assert.equal(app.looksLikeGitURL(p), false, p);
  }
});

// --- path half: unchanged #2456 flow -----------------------------------------

test("a path routes to RegisterProject's path half and waits inline", async () => {
  const { app, events, submits, pendingPaths, pendingURLs } = harness();
  app.openAddProject();
  submits[0]("/srv/repo");
  assert.deepEqual(events.filter(e => e.startsWith("rpc:")), ["rpc:path:/srv/repo"]);
  assert.equal(pendingURLs.length, 0, "a path must not reach the url field");
  assert.ok(events.includes("busy:1:true"), "the modal holds while the daemon resolves");

  pendingPaths[0].resolve({ id: "prj_A", root: "/srv/repo" });
  await settle();
  assert.ok(events.includes("close:1"), "a registered path closes the modal");
});

test("a refused path keeps the modal open with the daemon's message inline", async () => {
  const { app, events, submits, pendingPaths } = harness();
  app.openAddProject();
  submits[0]("/tmp/not-a-repo");
  pendingPaths[0].reject(new StubError("resolve git common directory: not a git repository", "refused"));
  await settle();
  assert.deepEqual(events.slice(-2), ["busy:1:false", "error:1:resolve git common directory: not a git repository"]);
  assert.ok(!events.includes("close:1"), "the refused form stays open to correct");
});

// --- url half (W9.10): close, report, remount on refusal ---------------------

test("a git URL routes to RegisterProject's url half and releases the modal for the clone", async () => {
  const { app, events, submits, pendingURLs, pendingPaths } = harness();
  app.openAddProject();
  submits[0]("https://github.com/owner/repo");
  assert.deepEqual(events.filter(e => e.startsWith("rpc:")), ["rpc:url:https://github.com/owner/repo"]);
  assert.equal(pendingPaths.length, 0, "a URL must not reach the path field");
  assert.ok(events.includes("close:1"), "the modal releases — the clone outlives a held form");
  assert.ok(
    events.includes("notice:Cloning https://github.com/owner/repo on the daemon host — the project appears in the switcher when it lands."),
    "the in-flight clone is named so the quiet window reads as progress, not silence",
  );

  pendingURLs[0].resolve({ id: "prj_A", root: "/daemon/projects/repo" });
  await settle();
  assert.ok(events.includes("refetch"), "a landed registration re-reads the registry");
});

test("a scp-like URL takes the url half too", async () => {
  const { app, events, submits } = harness();
  app.openAddProject();
  submits[0]("git@github.com:owner/repo.git");
  assert.deepEqual(events.filter(e => e.startsWith("rpc:")), ["rpc:url:git@github.com:owner/repo.git"]);
});

test("a refused URL remounts the form with the daemon's message and the input intact", async () => {
  const { app, events, submits, pendingURLs } = harness();
  app.openAddProject();
  submits[0]("https://user:secret@example.com/o/r.git");
  assert.ok(events.includes("close:1"));

  pendingURLs[0].reject(new StubError("git URL must not embed a password", "refused"));
  await settle();

  assert.deepEqual(events.slice(-3), ["busy:1:false", "error:1:git URL must not embed a password", "openModal"],
    "the daemon's refusal returns inline on the SAME remounted form — the typed URL is still there to fix");
  assert.ok(!events.some(e => e.startsWith("outcome:")), "a definite refusal is not an outcome notice");
});

test("a refused URL does not remount over a live overlay", async () => {
  const { app, context, events, submits, pendingURLs } = harness();
  app.openAddProject();
  submits[0]("https://user:secret@example.com/o/r.git");
  assert.ok(events.includes("close:1"));

  // A login/assistant overlay opened while the clone ran — reaping it for the
  // remount would strand the user's in-flight pane.
  context.accountLogin = { close: () => undefined };
  pendingURLs[0].reject(new StubError("git URL must not embed a password", "refused"));
  await settle();

  assert.equal(events.filter(e => e === "openModal").length, 1,
    "a live overlay is never reaped for the remount — only the initial open may mount");
  assert.ok(events.some(e => e.startsWith("outcome:failed:")),
    "the refusal lands as an outcome notice instead of the inline remount");
});

test("an uncertain URL outcome re-reads the registry and says so", async () => {
  const { app, events, submits, pendingURLs } = harness();
  app.openAddProject();
  submits[0]("https://github.com/owner/repo");
  pendingURLs[0].reject(new StubError("connection reset", "uncertain"));
  await settle();

  assert.ok(events.includes("refetch"), "the registry is re-read — the clone may have landed");
  assert.ok(events.some(e => e.startsWith("outcome:uncertain:") && e.includes("could not be confirmed")),
    "the outcome is reported unknown, not failed");
  assert.ok(!events.some(e => e.startsWith("error:") || e.startsWith("toast:")), "no failure is claimed");
});
