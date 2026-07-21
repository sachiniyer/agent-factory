// Entry point for the Agent Factory web client (#1592 Phase 5). PR3 filled the
// authed app with the live session rail; PR4 makes selecting a session open its
// LIVE TERMINAL — a real xterm.js pane over the daemon's binary WS PTY stream
// (terminal.ts). Selection keys off the stable session id (not the title, which
// collides across repos): the rail highlights the id, and the terminal dials
// /v1/sessions/{id}/stream for it.
//
// The bundle is fully self-contained (esbuild output + imported CSS incl. xterm's,
// no CDN, no off-origin fetch) so the daemon's CSP holds — the only network calls
// are same-origin /v1/ requests, the /v1/events WebSocket, and the per-session
// /v1/sessions/{id}/stream WebSocket.

import "./styles.css";
import {
  addTask,
  ApiError,
  archiveSession,
  clearToken,
  closeTab,
  createSession,
  type CreateSessionInput,
  createTab,
  createVSCodeTab,
  deleteProject,
  errorText,
  fetchSnapshot,
  killSession,
  getConfig,
  listBackends,
  listPrograms,
  listTasks,
  setConfigValue,
  loadToken,
  probeAuthRequired,
  probeToken,
  removeTask,
  renameTab,
  reorderTab,
  restoreSession,
  resumeFromLimit,
  shouldForgetToken,
  storeToken,
  triggerTask,
  updateTask,
} from "./api.js";
import { createKeyedQueue } from "./config.js";
import { EventStream, type EventStreamStatus } from "./events.js";
import { confirmDeleteProjectModal, confirmModal, type ModalHandle, newSessionModal } from "./modals.js";
import { InstallAffordance } from "./install.js";
import { decideKey, type KeyboardFocus, type View } from "./nav.js";
import { defaultFilter, filterSessions, loadFilter, persistFilter, withKind } from "./filter.js";
import { loadProjectChoice, persistProjectChoice, pickerProjects, reconcileProject, scopeToProject } from "./project.js";
import {
  applyEvent,
  clampActiveTab,
  pickSelection,
  rebindTargetAfterAwait,
  tabToKeepOnClose,
  upsertSession,
} from "./sessions.js";
import { SplitView } from "./split.js";
import { isArchived, isCreating, type RowKind } from "./status.js";
import { isRenameableTab } from "./tablabel.js";
import { Store } from "./store.js";
import { registerServiceWorker } from "./serviceworker.js";
import { bootStampTheme, persistThemeChoice, stampTheme, type ThemeChoice } from "./theme.js";
import { addTaskModal, type AddTaskInput, buildTask, editTaskModal } from "./tasks.js";
import type { ProgramCatalog } from "./programs.js";
import type { TerminalStatus } from "./terminal.js";
import {
  AppShell,
  orderedSessions,
  renderLogin,
  sessionTabs,
  canManageTabs,
  tabIdentity,
  tabRealId,
  type AppState,
  type NewTabKind,
} from "./ui.js";
import type { SessionData, TaskData, WireEvent } from "./types.js";

// Boot stamp (redesign PR1): apply the saved theme choice to <html> BEFORE the app
// mounts (before first paint), so an explicit light/dark choice shows no flash. This
// is the CSP-safe stamp — it ships in the same-origin bundle, not an inline <script>
// the daemon's `default-src 'self'` would block. Runs at module top, above the store.
const initialThemeChoice = bootStampTheme();

// Register the service worker (feat: PWA). Best-effort and side-effect-free for the
// app: it is what clears Chrome's installability bar, and it deliberately does
// nothing on an insecure context. See serviceworker.ts and src/sw.js.
registerServiceWorker();

const store = new Store<AppState>({
  phase: "login",
  view: "sessions",
  config: [],
  configPath: "",
  configStatus: null,
  selectedProject: null,
  authRequired: true,
  // Start in the connecting state: mount() immediately probes /v1/auth-info, and
  // showing the paste form before that resolves would flash a token field a
  // tokenless (loopback / require_token=false) daemon doesn't need (#1696). The
  // login view renders a neutral placeholder while connecting; bootstrap clears
  // this only when it lands on the real paste-token form.
  connecting: true,
  loginError: null,
  sessions: [],
  selectedId: null,
  live: "connecting",
  termStatus: "connecting",
  focus: "rail",
  activeTab: 0,
  shownTabs: [0],
  tabError: null,
  tasks: [],
  themeChoice: initialThemeChoice,
  // Resume the persisted filter (feat: hide archived by default) before first paint,
  // for the same reason the theme is boot-stamped: rendering the default set first
  // would flash rows the user has filtered away. Falls back to the default — every
  // state but archived — when nothing is stored.
  statusFilter: loadFilter(),
});

// The credential and the push stream are process-local singletons: one token
// drives the REST resync fetch, the events WS, and the PTY stream; one events
// stream is live at a time (started on connect, stopped on disconnect).
//
// THREE-STATE, not truthy (#1696): `null` = logged out / not connected; a
// non-empty string = a real bearer token; the EMPTY STRING = connected but the
// daemon requires no token for this client (loopback, or require_token=false) —
// a fully authorized connection. Every "am I connected?" guard MUST test
// `token === null`, never `!token`, or a tokenless client's create/kill/archive/
// restore/retry/attach would be silently skipped because `!"" === true`.
let token: string | null = null;
let stream: EventStream | null = null;

/** Fetches the agent catalog for a project (#1970), shared by the three forms that
 *  offer a program picker: new session, add task, edit task. One helper rather than
 *  three copies of the token dance — the hardcoded-list bug this RPC exists to
 *  delete started as exactly that kind of duplication.
 *
 *  Tokenless ("") is a valid credential (#1696), so a null token — not a falsy one —
 *  is the "not authorized yet" case. Rejecting there is safe: every caller degrades
 *  to the repo default rather than blocking its form. */
const loadPrograms = (repoPath: string): Promise<ProgramCatalog> =>
  token === null ? Promise.reject(new Error("not authorized")) : listPrograms(repoPath, token);
// Debounces the re-Snapshot that archived/restored events and reconnects trigger,
// so a burst of events collapses into a single authoritative refetch.
let resyncTimer: number | null = null;
// Debounces the ListTasks refetch that task.* events trigger (#1592 Phase 5 PR8),
// so a burst of task deltas collapses into one authoritative refetch.
let taskResyncTimer: number | null = null;
// The auto-dismiss timer for the operation-error toast, and how long it shows.
let tabErrorTimer: number | null = null;
const TAB_ERROR_MS = 6000;

// The app-phase DOM (built once per login) and the persistent terminal host that
// lives inside it. Keeping the host stable across renders is what lets the focused
// xterm survive rail updates (see ui.ts AppShell). The host now holds a SPLIT LAYOUT
// (split.ts) rather than a single xterm: one leaf by default (today's behavior), or
// multiple concurrent panes when the user drags tabs into splits.
let shell: AppShell | null = null;
const termHost = document.createElement("div");
termHost.className = "af-term-host";

// The persistent overlay layer modals mount into (empty unless a modal is open),
// and the one live modal at a time. Modals are managed imperatively (not via the
// store) so their inputs keep focus/typed text across a busy/error cycle — the
// same reason the terminal host lives outside the re-rendered tree.
const modalHost = document.createElement("div");
modalHost.className = "af-modal-host";
let modal: ModalHandle | null = null;

// The appbar's "Install app" affordance (feat: PWA). Built ONCE here, for the same
// reason termHost and modalHost are: rerender() drops `shell` on logout and builds a
// fresh AppShell on the next login, so constructing this inside the shell would stack
// another pair of window listeners on every cycle — and strand the stashed
// beforeinstallprompt on a discarded copy. It stays hidden unless the browser says
// the app is installable; see install.ts.
const installAffordance = new InstallAffordance();

// The split-pane view bound to termHost: it owns the per-instance layout tree and one
// live AttachTerminal per pane, rebuilt/reconciled as the selection, tab list, or
// layout changes (see syncSplit). It diffs the session internally, so syncSplit can
// call setSession on every store change without tearing down unchanged panes.
const splitView = new SplitView(termHost, {
  onStatus: (s: TerminalStatus) => store.set({ termStatus: s }),
  // Keep the nav-vs-terminal mode (#1693) in sync with real xterm focus across every
  // pane, so a click straight into a pane enters terminal mode (and clicking/tabbing
  // away from all panes returns to rail mode) without going through Enter/Escape.
  onFocusChange: (focused: boolean) => store.set({ focus: focused ? "terminal" : "rail" }),
  // Mirror the layout into the store so the tab bar can highlight the focused pane's
  // tab (activeTab) and flag which tabs are shown across panes (shownTabs). Fired only
  // on a real change, so this write never loops back through rerender → syncSplit.
  onLayout: ({ focusedTab, shownTabs }) => store.set({ activeTab: focusedTab, shownTabs }),
});

let root: HTMLElement | null = null;

function mount(): void {
  root = document.getElementById("app");
  if (!root) {
    throw new Error("af-web: #app root element missing from index.html");
  }
  store.subscribe(rerender);
  rerender();

  // The keyboard/focus state machine (#1693), wired once at the document level in
  // the CAPTURE phase so it decides BEFORE xterm's textarea handler: in rail mode
  // j/k always navigate; in terminal mode keys fall through to the agent except
  // Escape, which we swallow here (stopPropagation) so detaching never leaks a
  // stray ESC byte into the PTY.
  document.addEventListener("keydown", onKeydown, true);

  // Follow the OS theme while the choice is Auto (redesign PR1).
  watchSystemTheme();

  void bootstrap();
}

/** Decides the opening view (#1696): probe whether the daemon requires a token for
 *  THIS client. If not (loopback, or require_token=false), connect straight through
 *  with no credential — and store nothing. If it does, resume the token kept in
 *  localStorage (so a new tab / a browser restart logs in once, not every visit),
 *  else land on the paste-token login. A resumed token the daemon rejects clears
 *  itself and falls back to the paste form, once. A probe transport failure also
 *  falls back to the token login — never auto-connects on uncertainty. */
async function bootstrap(): Promise<void> {
  let required = true;
  try {
    required = await probeAuthRequired();
  } catch {
    // Can't reach the probe (daemon down / wrong host): fail safe to the
    // token login — the user can still paste a token and retry.
    required = true;
  }
  if (!required) {
    // Tokenless client (loopback / require_token=false): connect straight through
    // with no credential. `connecting` stays true so the placeholder shows until
    // connect() flips the phase to "app" — the paste form never flashes.
    store.set({ authRequired: false });
    void connect("");
    return;
  }
  const existing = loadToken();
  if (existing) {
    // Resume a stored token silently — again, placeholder until connect() lands.
    store.set({ authRequired: true });
    void connect(existing);
    return;
  }
  // Token required and none stored: reveal the paste-token form.
  store.set({ authRequired: true, connecting: false });
}

/** Reflects the current store state into the DOM: the login view, or the app shell
 *  (built lazily) patched in place plus the terminal lifecycle synced to selection. */
function rerender(): void {
  if (!root) {
    return;
  }
  const state = store.get();
  if (state.phase === "login") {
    if (shell) {
      shell = null; // dropped from the tree by renderLogin below
    }
    disposeSplit();
    closeModal();
    renderLogin(root, state, actions);
    return;
  }
  if (!shell) {
    shell = new AppShell(actions, termHost, modalHost, installAffordance.el);
    root.replaceChildren(shell.el);
  }
  shell.update(state);
  syncSplit(state);
}

/** Validates the token (Snapshot probe), and on success persists it, seeds the rail
 *  from the probe's snapshot, subscribes to /v1/events, and shows the app; on
 *  failure surfaces an actionable error on the login view — forgetting the stored
 *  token only when the daemon REJECTED it (shouldForgetToken). */
async function connect(candidate: string): Promise<void> {
  store.set({ connecting: true, loginError: null });
  let sessions: SessionData[];
  try {
    sessions = await probeToken(candidate);
  } catch (e) {
    // A rejected credential is forgotten so the next load prompts cleanly instead of
    // retrying a dead token forever; a transport failure keeps it, because "the
    // daemon is down" is not evidence the token is bad (see shouldForgetToken).
    // Either way we land on the login view exactly once — no retry loop.
    if (shouldForgetToken(e)) {
      clearToken();
    }
    store.set({ phase: "login", connecting: false, loginError: describeError(e) });
    return;
  }
  token = candidate;
  // Persist only a REAL token so the next visit can resume it; the empty-token
  // sentinel (no-auth client, #1696) is never stored — bootstrap re-probes
  // /v1/auth-info on every load, so a tokenless daemon needs nothing on disk.
  storeToken(candidate);
  // Fetch the tasks BEFORE choosing the initial project scope (redesign PR2, Greptile
  // follow-on Fix 2): the persisted selection must reconcile against the FULL project
  // list — sessions AND tasks — so a persisted TASK-ONLY project restores AS ITSELF,
  // not as a temporary session-backed fallback that would then stick (reconcile keeps
  // a valid current selection). A transport failure degrades to no tasks (the events
  // plane / a view switch refetches); the scope then falls back until they load.
  let tasks: TaskData[] = [];
  try {
    tasks = await listTasks(candidate);
  } catch {
    tasks = [];
  }
  // Scope to a project on connect: resume the persisted choice if it is still a real
  // project (session- OR task-derived), else the most-recently-active default.
  const selectedProject = reconcileProject(sessions, tasks, loadProjectChoice(), null);
  store.set({
    phase: "app",
    view: "sessions",
    selectedProject,
    connecting: false,
    loginError: null,
    sessions,
    selectedId: pickSelection(sessions, store.get().selectedId),
    live: "connecting",
    focus: "rail",
    activeTab: 0,
    shownTabs: [0],
    tabError: null,
    tasks,
  });
  startStream(candidate);
}

/** Forgets the token — in memory AND in storage — tears down the push stream +
 *  terminal, and returns to login. This is the "forget the saved token" affordance
 *  now that the credential persists across visits: on a shared machine, or after a
 *  rotation, Disconnect is what makes the next load prompt again. */
function disconnect(): void {
  stopStream();
  closeModal();
  token = null;
  clearToken();
  store.set({
    phase: "login",
    view: "sessions",
    selectedProject: null,
    connecting: false,
    loginError: null,
    sessions: [],
    selectedId: null,
    live: "connecting",
    focus: "rail",
    activeTab: 0,
    shownTabs: [0],
    tabError: null,
    tasks: [],
  });
}

// --- keyboard focus (nav vs terminal, #1693) -------------------------------

/** The ordered tab identities of `id` in `list` — the list a retained layout's leaf
 *  ordinals mean something relative to. Empty for a session that isn't there (a
 *  never-shown or just-removed one), which reads as "nothing to remap against". */
function tabIdsOf(list: SessionData[], id: string | null): string[] {
  const s = id ? list.find((x) => x.id === id) : null;
  return s ? sessionTabs(s).map(tabIdentity) : [];
}

/** Moves the rail selection to a session by its stable id and puts the keyboard in
 *  rail-nav mode. This is the keyboard path (j/k): selecting a row never steals the
 *  keyboard into the terminal — you attach explicitly with Enter. Resetting focus to
 *  "rail" here also clears any stale "terminal" mode left by a since-disposed
 *  terminal, so j/k keep working after the selected session goes away. The active tab
 *  follows the layout the split view will actually show (settledTab) — a session shown
 *  before keeps its retained pane, and only one never shown starts on its agent tab. */
function moveSelection(id: string): void {
  clearTabError();
  store.set({
    selectedId: id,
    focus: "rail",
    // Clamped like syncSplit's initialTab, so the store's claim and the pane's binding
    // are the same statement even if the roster shrank since the tree was retained.
    activeTab: clampActiveTab(store.get().sessions, id, splitView.settledTab(id, tabIdsOf(store.get().sessions, id))),
  });
}

/** The click/Enter path: selects the session AND hands the keyboard to its terminal
 *  (attach). moveSelection rebuilds the terminal synchronously (via rerender), so
 *  focusTerminal then focuses the fresh instance. Also switches to the sessions view:
 *  this is the projects pane's jump-to-session affordance too (a project's session
 *  row calls open), so opening a session always lands on the sessions view. */
function openFromRail(id: string): void {
  if (store.get().view !== "sessions") {
    store.set({ view: "sessions" });
  }
  moveSelection(id);
  focusTerminal();
}

/** Gives the keyboard to the selected session's FOCUSED pane (attach): keys now reach
 *  the agent. The xterm focus event echoes back through onFocusChange and confirms
 *  the "terminal" mode.
 *
 *  When the focused pane has NO terminal — a web or VS Code tab, which renders an
 *  iframe — there is nothing to attach to, so we fall back to the rail instead of
 *  claiming "terminal" mode over a terminal that does not exist. That state strands
 *  the user: nav.ts resolves every non-Escape key to {kind:"none"} while focus is
 *  "terminal", so j/k/digits/t/w reach neither an xterm nor the rail handler and are
 *  silently swallowed until Escape.
 *
 *  The decision lives HERE, behind SplitView.focus()'s boolean, rather than at each
 *  call site: openTab, createSessionTab and openFromRail all route through this
 *  function, so a new caller (or a new tab kind without a PTY) cannot drift back into
 *  the bug. The rail — not the iframe — is the fallback because rail keys keep working
 *  there, and a cross-origin iframe's focus is not reliably observable anyway.
 *  switchTab already sets focus:"rail" for exactly this reason. */
function focusTerminal(): void {
  if (!splitView.focus()) {
    focusRail();
    return;
  }
  store.set({ focus: "terminal" });
}

/** Returns the keyboard to the rail (Escape / detach): blurs every pane so
 *  document-level j/k navigate again. */
function focusRail(): void {
  store.set({ focus: "rail" });
  splitView.blur();
}

// --- top-level view switching (#1592 Phase 5 PR8) --------------------------

/** Switches the top-level view (sessions | tasks). Leaving the sessions
 *  view hands the keyboard back to the rail AND blurs the (still-live but now hidden)
 *  terminal, so a stray key in the tasks view never leaks to the agent — the
 *  view switch composes with the #1694 focus model instead of fighting it. Switching
 *  INTO the tasks view refreshes the task list so it is current on arrival. */
function switchView(view: View): void {
  if (store.get().view === view) {
    return;
  }
  clearTabError();
  if (view !== "sessions") {
    splitView.blur();
  }
  store.set({ view, focus: "rail" });
  if (view === "tasks") {
    refreshTasks();
  }
  if (view === "config") {
    refreshConfig();
  }
}

/**
 * Shows/hides one session state in the rail (feat: hide archived by default) and
 * persists the whole filter so the choice sticks per browser.
 *
 * The SELECTION is deliberately left alone, even when this hides the selected row.
 * The filter governs what the rail draws, not what you are attached to: a session's
 * state changes on its own (a Ready agent starts working the moment it is prompted),
 * so tying detach to visibility would rip the terminal out from under a user who
 * filtered to "Ready" and then typed into one. The row leaves the rail; the pane
 * keeps streaming, and the rail re-highlights it when its state is shown again.
 */
function setStatusFilter(kind: RowKind, on: boolean): void {
  const next = withKind(store.get().statusFilter, kind, on);
  persistFilter(next);
  store.set({ statusFilter: next });
}

/** Restores the default filter — every state but archived. */
function resetStatusFilter(): void {
  const next = defaultFilter();
  persistFilter(next);
  store.set({ statusFilter: next });
}

/** Switches the active project (redesign PR2): scope the rail + views to `root`,
 *  persist it so a reload resumes it, and drop a selection that doesn't belong to the
 *  new project (its terminal detaches). A no-op when already on that project. */
function switchProject(root: string): void {
  if (store.get().selectedProject === root) {
    return;
  }
  clearTabError();
  persistProjectChoice(root);
  // Keep the current selection only if it lives in the newly selected project; else
  // clear it so the main pane returns to its empty state instead of showing a session
  // hidden from the scoped rail.
  const sel = selectedSessionData();
  const keep = sel && sel.worktree?.repo_path === root ? store.get().selectedId : null;
  splitView.blur();
  // A KEPT selection keeps its pane too, so its active tab is whatever that pane shows
  // — not 0, which would desync the bar from it (#1855, same as moveSelection).
  store.set({
    selectedProject: root,
    selectedId: keep,
    focus: "rail",
    activeTab: keep
      ? clampActiveTab(store.get().sessions, keep, splitView.settledTab(keep, tabIdsOf(store.get().sessions, keep)))
      : 0,
  });
}

// --- lifecycle actions (modals) --------------------------------------------

/** The currently selected session's stable id + display title, or null if none.
 *  The id is the collision-proof key the daemon resolves actions by; the title is
 *  for the modal chrome and the daemon's lifecycle event (#1592 Phase 5 follow-up). */
function selectedSession(): { id: string; title: string } | null {
  const { sessions, selectedId } = store.get();
  const s = sessions.find((x) => x.id === selectedId);
  // A legacy/disk-only row may carry no id; send "" and the daemon falls back to
  // its title lookup, exactly as before this fix (the id is the fast path, not a
  // hard requirement).
  return s ? { id: s.id ?? "", title: s.title } : null;
}

/** Closes and clears the open modal, if any. */
function closeModal(): void {
  if (modal) {
    modal.close();
    modal = null;
  }
}

/** Mounts a fresh modal, replacing any currently open one. */
function openModal(m: ModalHandle): void {
  closeModal();
  modal = m;
  modalHost.replaceChildren(m.el);
}

/** Opens the new-session modal, its picker seeded from the live projects. Submit
 *  closes immediately: the daemon publishes its authoritative OpCreating row on
 *  the events stream, then either replaces it with the completed projection or
 *  removes it. A failure surfaces through the shared operation toast because the
 *  form is deliberately no longer held open by the RPC. */
function newSession(): void {
  const projects = pickerProjects(store.get().sessions, store.get().tasks);
  openModal(
    newSessionModal(projects, store.get().selectedProject, {
      // The backend catalog is per-repo and read at choose time (#1933), so the
      // modal asks for the picked project's on open and on every project change.
      // Tokenless ("") is a valid credential (#1696), so a null token — not a
      // falsy one — is the "not authorized yet" case.
      loadBackends: (repoPath: string) => (token === null ? Promise.reject(new Error("not authorized")) : listBackends(repoPath, token)),
      // The agent catalog, same contract (#1970): the daemon owns the enum.
      loadPrograms,
      onSubmit: (values: CreateSessionInput) => {
        const tok = token;
        // `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696).
        if (tok === null || !modal) {
          return;
        }
        const m = modal;
        clearTabError();
        // Latch the detached form against a duplicate submit, then release the UI
        // immediately. The row that follows is NOT synthesized here: it comes from
        // the daemon's session.updated OpCreating projection.
        m.setBusy(true);
        closeModal();
        void createSession(values, tok)
          .then((created) => {
            // Upsert the created row AND select it in one update, so it opens
            // attached immediately. Upserting here (not just setting selectedId)
            // matters: the async created event may not have landed yet, and
            // selecting an id whose row isn't in the list would leave the pane
            // stuck empty (the shell only re-renders the main pane on a selection
            // change). CreateSession returns the full projection, so the row is
            // complete; the later created event just upserts the same id again.
            if (created.id) {
              const sessions = upsertSession(store.get().sessions, created);
              // Reset to the agent tab: selecting a DIFFERENT session must show its
              // tab 0 (a new session has only the agent tab), or a stale activeTab
              // would stream ?tab=<n> for a tab this session doesn't have. This is
              // the same invariant moveSelection enforces for the keyboard path.
              store.set({ sessions, selectedId: created.id, activeTab: 0, tabError: null });
            }
          })
          .catch((e) => {
            // The daemon publishes session.killed for the provisional id. Resync as a
            // drop-slow/reconnect fallback so even a missed delete event cannot strand a
            // phantom creating row, and surface the daemon's unmodified error text.
            requestResync();
            surfaceTabError(e);
          });
      },
      onCancel: closeModal,
    }),
  );
}

/** Opens the kill/archive/restore confirm modal for the selected session. Restore
 *  is the reverse of archive (#1932): the archived row's lifecycle glyph becomes
 *  Restore, and confirming it POSTs RestoreSession. */
function openConfirm(action: "kill" | "archive" | "restore"): void {
  const sel = selectedSession();
  if (!sel) {
    return;
  }
  openModal(
    confirmModal({
      action,
      sessionTitle: sel.title,
      onConfirm: () => {
        const tok = token;
        // `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696).
        if (tok === null || !modal) {
          return;
        }
        const m = modal;
        m.setBusy(true);
        // Restore resolves the target by TITLE only — its daemon request has no id
        // field (see restoreSession), unlike kill/archive which key by sel.id.
        const run =
          action === "kill"
            ? killSession(sel.id, sel.title, tok)
            : action === "archive"
              ? archiveSession(sel.id, sel.title, tok)
              : restoreSession(sel.title, tok);
        void run.then(closeModal).catch((e) => {
          m.setBusy(false);
          m.setError(describeError(e));
        });
      },
      onCancel: closeModal,
    }),
  );
}

/** Opens the reversible delete-project confirm for a project row (#1735). On
 *  confirm it archives the repo's live sessions via DeleteProject; the archived
 *  events + projects.changed resync the rail and drop the project from the view. */
function openDeleteProject(root: string, label: string, sessionCount: number): void {
  openModal(
    confirmDeleteProjectModal({
      projectLabel: label,
      sessionCount,
      onConfirm: () => {
        const tok = token;
        // `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696).
        if (tok === null || !modal) {
          return;
        }
        const m = modal;
        m.setBusy(true);
        void deleteProject(root, tok)
          .then(closeModal)
          .catch((e) => {
            m.setBusy(false);
            m.setError(describeError(e));
          });
      },
      onCancel: closeModal,
    }),
  );
}

// --- tab management (#1592 Phase 5 PR7) ------------------------------------

/** The full projection of the selected session, or null — the source of its tab
 *  list and backend type, which the id/title-only selectedSession() omits. */
function selectedSessionData(): SessionData | null {
  const { sessions, selectedId } = store.get();
  return sessions.find((s) => s.id === selectedId) ?? null;
}

/** Points the FOCUSED pane at `index` WITHOUT attaching (the 1-9 keys). The split
 *  view rebuilds that pane's terminal and mirrors the new focused tab back into the
 *  store (onLayout → activeTab); the keyboard stays in rail mode, mirroring how j/k
 *  select a row without attaching. */
function switchTab(index: number): void {
  splitView.setFocusedTab(index);
  store.set({ focus: "rail" });
}

/** Points the focused pane at a tab AND attaches it (a tab-bar click). The split view
 *  rebuilds that pane's terminal synchronously, so focusTerminal then focuses the
 *  fresh instance — the same pattern as openFromRail. */
function openTab(index: number): void {
  splitView.setFocusedTab(index);
  focusTerminal();
}

/** Runs a tab mutation whose post-await step re-points the FOCUSED pane, applying the
 *  two guards every such rebind needs so a new async gesture can't forget them
 *  (#2000). Both create and close await a round trip and then point the focused pane
 *  at a tab resolved against the roster they mutated — but splitView.trees is
 *  per-session and setFocusedTab carries no session of its own, so re-pointing from an
 *  intent formed BEFORE the await would yank the pane once the user has since selected
 *  another session or focused another pane (the #1815 hazard closeSessionTab first
 *  named, which createSessionTab shipped without).
 *
 *  `run` issues the RPC and resolves to the refreshed roster; `resolve` names the tab
 *  the pane should end on in that post-await roster BY IDENTITY, returning -1 when it's
 *  gone (never an ordinal snapshotted before the await, which names a tab only relative
 *  to one roster). The roster is committed unconditionally so the grown/shrunk tab list
 *  always lands; only the pane rebind is gated, by sessions.rebindTargetAfterAwait —
 *  the one place the guard lives, so both call sites (and the next) stay in step.
 *  `attach` attaches the focused terminal after (a create/open, mirroring the TUI's
 *  `t`), or leaves the keyboard where it is (a close). Errors (e.g. a remote session,
 *  or the tab cap) surface on the pane header's status line. */
function guardedTabRebind(
  selId: string,
  run: () => Promise<SessionData[]>,
  resolve: (sessions: SessionData[]) => number,
  attach: boolean,
): void {
  // Pinned BEFORE the RPC is issued, exactly where closeSessionTab captured `gen`.
  const gen = splitView.layoutGeneration();
  void run()
    .then((sessions) => {
      const targetIdx = resolve(sessions);
      // Commit the roster first (rerender → syncSplit re-validates the tree against the
      // new tabCount), then re-point the focused pane only if the pinned intent still
      // holds. selectedId is read AFTER the set so it reflects any session switch the
      // user made during the await (pickSelection keeps their newer choice).
      store.set({ sessions, selectedId: pickSelection(sessions, store.get().selectedId) });
      const idx = rebindTargetAfterAwait(gen, selId, splitView.layoutGeneration(), store.get().selectedId, targetIdx);
      if (idx >= 0) {
        splitView.setFocusedTab(idx);
        if (attach) {
          focusTerminal();
        }
      }
    })
    .catch((e) => surfaceTabError(e));
}

/** Creates the requested tab on the selected session (`t` sends shell; the
 *  labelled New tab menu can also send vscode), then
 *  resyncs to pull the grown tab list, selects the new tab, and attaches it —
 *  mirroring the TUI's `t`, which opens the fresh tab as a pane. The resync is
 *  kept for THIS window's own mutation because it must select+attach the new tab
 *  synchronously, which needs the tab in hand rather than an event later; a tab
 *  changed by any OTHER actor now arrives on its own, since CreateTab/CloseTab
 *  publish session.updated with the refreshed roster (#1812). */
function createSessionTab(kind: NewTabKind = "shell"): void {
  const sel = selectedSessionData();
  const tok = token;
  // `tok === null` not `!tok`: "" is the authorized-tokenless credential (#1696),
  // so a loopback client can still create tabs.
  if (!sel || tok === null || !canManageTabs(sel)) {
    return;
  }
  clearTabError();
  const selId = sel.id ?? "";
  const create = kind === "vscode" ? createVSCodeTab : createTab;
  // createTab returns the daemon's resolved, collision-suffixed name; hold it so the
  // rebind lands on the tab THIS create made.
  let createdName = "";
  guardedTabRebind(
    selId,
    () =>
      create(selId, sel.title, tok).then((name) => {
        createdName = name;
        return fetchSnapshot(tok);
      }),
    // Focus the created tab BY its resolved name, never `length - 1`: the last slot is
    // an ordinal, and a concurrent create from another client landing inside this
    // round trip would make it THEIR tab. Resolving the name to its current ordinal in
    // the post-await roster is the create-side twin of closeSessionTab's keepId, and a
    // -1 (name gone, or session vanished) bails rather than guessing (#2000).
    (sessions) => {
      const grown = sessions.find((s) => s.id === selId);
      return grown ? sessionTabs(grown).findIndex((t) => t.name === createdName) : -1;
    },
    true,
  );
}

/** Closes the tab at `index` of the selected session (the `w` key / × button),
 *  then resyncs the shrunk tab list and re-points the active tab. The agent tab
 *  (index 0) is unclosable. */
function closeSessionTab(index: number): void {
  const sel = selectedSessionData();
  const tok = token;
  // `tok === null` not `!tok`: "" is the authorized-tokenless credential (#1696).
  if (!sel || tok === null || index <= 0 || !canManageTabs(sel)) {
    return;
  }
  const tabs = sessionTabs(sel);
  const target = tabs[index];
  if (!target) {
    return;
  }
  clearTabError();
  const selId = sel.id ?? "";
  // Decide WHICH TAB the pane should end on by IDENTITY, not by arithmetic on an
  // index. Closing tab N shifts every higher tab down by one, and the tempting fix is
  // to subtract that shift — but an ordinal only names a tab relative to one roster,
  // and this close spans two. Both directions of that arithmetic are wrong:
  //
  //  - Reading activeTab AFTER the awaits double-subtracts. The daemon's tab event
  //    (#1812) can land mid-flight, rerendering with the shrunk roster, remapping each
  //    pane to follow its own tab (#1779) and writing the ALREADY-shifted index back —
  //    so subtracting again lands a tab too far left.
  //  - Subtracting from a PRE-await snapshot instead goes stale the other way: another
  //    client closing a LOWER tab in the same window shifts the roster underneath, and
  //    the pre-computed ordinal then names a neighbour. (Roster events deliberately
  //    bump no generation, so the guard below can't catch that one.)
  //
  // Naming the tab itself sidesteps both: whatever the roster does meanwhile, the pane
  // follows the tab the user was actually looking at — the same principle #1779 put in
  // the layout tree, applied to the store's index. See sessions.tabToKeepOnClose, which
  // holds the decision as a pure function so both directions above are unit-tested.
  const keepId = tabToKeepOnClose(tabs.map(tabIdentity), index, store.get().activeTab);
  // tabRealId, NOT tabIdentity: this crosses the wire, so it must be an id the DAEMON
  // can resolve — never the synthesized kind:name tabIdentity falls back to for a
  // pre-#1738 record (see renameSessionTab). The keepId above is the opposite case,
  // local bookkeeping, which is why the two helpers appear within a few lines of each
  // other here and are not interchangeable.
  //
  // The post-await rebind — and its layoutGeneration + selectedId guard, once
  // spelled out here and the source of the #1815 finding — now lives in
  // guardedTabRebind, which createSessionTab shares so neither can forget it (#2000).
  // A close only re-points the focused pane; it does not attach (attach = false).
  guardedTabRebind(
    selId,
    () => closeTab(selId, sel.title, target.name, tabRealId(target), tok).then(() => fetchSnapshot(tok)),
    // Resolve the kept tab's CURRENT ordinal in the post-close roster; -1 (a concurrent
    // close took it too) leaves the pane where syncSplit's identity remap already
    // settled it rather than guessing again.
    (sessions) => {
      const shrunk = sessions.find((s) => s.id === selId);
      return shrunk ? sessionTabs(shrunk).map(tabIdentity).indexOf(keepId) : -1;
    },
    false,
  );
}

/**
 * Renames the tab with the stable IDENTITY `id` on the selected session (#1813) — the
 * commit of the tab bar's inline edit.
 *
 * Resolved by IDENTITY, not by the ordinal the edit began at, for the reason
 * closeSessionTab spells out at length: an ordinal names a tab only relative to one
 * roster, and this op spans two. The window is not a narrow race but the ORDINARY
 * path — another window reordering or closing a lower tab publishes session.updated,
 * whose repaint is itself what ends the edit and fires this commit — so an ordinal
 * would reliably name whichever tab had shifted into the slot, and rename THAT.
 * An identity survives the shift; if it resolves to nothing the tab was closed while
 * being edited, and renaming nothing is the right answer (falling back to the ordinal
 * would be precisely the bug).
 *
 * The tab is handed to the DAEMON by its stable id, with its current name alongside as
 * the fallback (#1929). The id is what closes the residual this comment used to
 * describe: when the RPC was name-only, another client could free the name between the
 * read here and the daemon's handling, and the request would land on whatever tab had
 * taken it. Now an id that no longer resolves is refused outright — a tab that went
 * away reads as an error, never as a silent wrong-tab rename.
 *
 * Nothing is applied optimistically: the daemon decides the final name (it sanitizes
 * and dup-suffixes exactly as a create does, `dup` → `dup-2`), so the authoritative
 * Snapshot is what repaints the bar and the pane headers, never the string that was
 * typed. Other windows learn of it from the session.updated the daemon publishes
 * (#1812); the resync here is only so THIS window doesn't depend on its own event
 * round trip.
 */
function renameSessionTab(id: string, name: string, editedSessionId: string): void {
  const sel = selectedSessionData();
  const tok = token;
  // `tok === null` not `!tok`: "" is the authorized-tokenless credential (#1696).
  if (!sel || tok === null || !canManageTabs(sel)) {
    return;
  }
  // The edit spanned a session switch: the input opened on `editedSessionId` and the
  // render that reparented the bar to a DIFFERENT session blurred it, firing this commit
  // against the now-selected one. The user navigated away — abandon the half-typed rename
  // silently. Reporting a miss here would be the lie this whole surface exists to prevent:
  // the edited tab is not gone, it is still in the session they left. Only a miss WITHIN
  // the same session is a genuine vanish worth surfacing.
  if ((sel.id ?? "") !== editedSessionId) {
    return;
  }
  // tabIdentity, not tabRealId: this is LOCAL bookkeeping (re-find the edited tab in
  // the CURRENT roster), never an id handed to the daemon — so the synthesized
  // kind:name of a pre-#1738 record is a usable key here, and still names the tab
  // across a reorder that an ordinal would not survive. Its documented residual (a
  // close+recreate of the same name) is the same one #1738 closes everywhere else.
  const target = sessionTabs(sel).find((t) => tabIdentity(t) === id);
  if (!target) {
    // The tab the edit was OPENED on is no longer in the roster — closed by another
    // client (and possibly replaced by a same-named tab, which is invisible to the bar's
    // render signature) while the input was open, WITHIN the same session (the switch
    // case returned above). Reported rather than resolved to whatever now holds that
    // slot: landing the rename on a tab the user never edited is the failure the
    // id-keying exists to prevent, and dropping it in silence is one the user would
    // answer by retyping into the same void. There is no third option worth having —
    // an ordinal fallback is the first failure with a friendlier face.
    surfaceTabError(new Error("That tab is gone — nothing was renamed."));
    return;
  }
  // Re-checked here and not merely at the affordance: an agent/shell tab renders a
  // fixed label and ignores its name, so the daemon refuses the rename — firing one
  // anyway could only produce a guaranteed-to-fail call (see isRenameableTab).
  if (!isRenameableTab(target.kind)) {
    return;
  }
  clearTabError();
  const selId = sel.id ?? "";
  // tabRealId, NOT tabIdentity: this one crosses the wire, so it must be an id the
  // DAEMON can resolve — never the synthesized kind:name tabIdentity falls back to for
  // a pre-#1738 record. An empty id sends no tab_id and the daemon resolves by name,
  // which is the documented fallback for a roster that has no ids to key on (#1929).
  void renameTab(selId, sel.title, target.name, name, tabRealId(target), tok)
    .then(() => fetchSnapshot(tok))
    .then((sessions) => {
      store.set({ sessions, selectedId: pickSelection(sessions, store.get().selectedId) });
    })
    .catch((e) => surfaceTabError(e));
}

/**
 * Moves the tab at `from` to the 0-based `to` (#1813) — the commit of a drag within
 * the tab bar.
 *
 * Neither index may be 0: Go's Tabs[0] is a load-bearing invariant (archive and the
 * agent's own conversation/tmux all index it), so the agent tab can neither move nor
 * be displaced, and the daemon refuses both. The bar already declines to offer such a
 * drop; this is the backstop, on the same principle as closeSessionTab's `index <= 0`.
 *
 * No pane is re-pointed afterwards, and that is the assertion rather than an
 * omission: panes are bound to tab IDENTITIES, so the resync alone re-points each one
 * at wherever its OWN tab now sits (split.ts remapByIdentity) and a pane whose tab
 * merely moved is followed, not rebuilt — its stream and scrollback survive. Anything
 * this function did with an ordinal here could only fight that.
 */
function reorderSessionTab(from: number, to: number): void {
  const sel = selectedSessionData();
  const tok = token;
  if (!sel || tok === null || !canManageTabs(sel)) {
    return;
  }
  const tabs = sessionTabs(sel);
  const target = tabs[from];
  if (!target || from <= 0 || to <= 0 || to >= tabs.length || from === to) {
    return;
  }
  clearTabError();
  const selId = sel.id ?? "";
  // The id names WHICH tab moves; `to` is only WHERE it lands. A reorder is the verb
  // most likely to be racing another client's, so resolving the mover by id rather
  // than by a name/ordinal read from a roster that may already have shifted is the
  // whole point (#1929). See renameSessionTab for why tabRealId and not tabIdentity.
  void reorderTab(selId, sel.title, target.name, to, tabRealId(target), tok)
    .then(() => fetchSnapshot(tok))
    .then((sessions) => {
      store.set({ sessions, selectedId: pickSelection(sessions, store.get().selectedId) });
    })
    .catch((e) => surfaceTabError(e));
}

/** Surfaces a failed asynchronous operation as a transient toast. Tab mutations
 *  have no modal, and session creation closes its modal before awaiting the daemon;
 *  the message is therefore the visible failure path for both. It auto-clears after
 *  a few seconds, and a fresh failure resets the timer. */
function surfaceTabError(e: unknown): void {
  // The raw error message — NOT describeError, whose "Login failed…/Couldn't reach
  // the daemon…" framing is for the login probe. Operations carry the daemon's own
  // message (e.g. create failure or tab cap) or a fail-closed refusal verbatim.
  const msg = errorText(e);
  console.error("af-web: operation failed:", msg);
  if (tabErrorTimer !== null) {
    window.clearTimeout(tabErrorTimer);
  }
  store.set({ tabError: msg });
  tabErrorTimer = window.setTimeout(() => {
    tabErrorTimer = null;
    store.set({ tabError: null });
  }, TAB_ERROR_MS);
}

/** Clears the tab-error toast and cancels its auto-dismiss timer (on a selection
 *  change or a fresh successful op). */
function clearTabError(): void {
  if (tabErrorTimer !== null) {
    window.clearTimeout(tabErrorTimer);
    tabErrorTimer = null;
  }
  if (store.get().tabError !== null) {
    store.set({ tabError: null });
  }
}

// --- task actions (#1592 Phase 5 PR8) --------------------------------------

/** Refetches the authoritative task list and commits it to the store. Failures are
 *  swallowed: the events plane / a later action retries, exactly like the Snapshot
 *  resync. `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696). */
/** Re-reads the config manifest and the user's live values.
 *
 *  Always from the daemon, never from a cached copy: config.toml is hand-editable
 *  by design, so it may have changed under us (a hand-edit, `af config set`, the
 *  config agent). A form rendered from a stale copy would show the user a value the
 *  file does not hold.
 *
 *  A failure leaves the last-known list up and is surfaced in the tab-error line
 *  rather than swallowed — an empty config screen would read as "you have no
 *  settings". */
function refreshConfig(): void {
  const tok = token;
  if (tok === null) {
    return;
  }
  void getConfig(tok)
    .then((resp) => {
      store.set({ config: resp.entries, configPath: resp.path });
    })
    .catch((err: unknown) => {
      // Surfaced, not swallowed: an empty config screen would read as "you have
      // no settings" rather than "the read failed".
      surfaceTabError(err);
    });
}

/** Writes one config key and reports the outcome.
 *
 *  The value goes to the daemon unvalidated BY DESIGN: it hands it to the same
 *  validator and the same file-locked atomic writer `af config set` uses, so the
 *  rules live in exactly one place. A refusal comes back as the validator's own
 *  message and is shown verbatim under the field.
 *
 *  On success the manifest is re-read rather than patched locally, so the form
 *  shows what the FILE holds (the canonical value the writer chose), not what the
 *  browser sent. The daemon's restart notice rides along with the echo: config.toml
 *  is read at startup, and an editor that changed a value the running daemon then
 *  ignored — without saying so — would be lying by omission. */
/** Serializes saves per config key — see createKeyedQueue for why the client is
 *  the only side that can keep the user's order. */
const queueConfigSave = createKeyedQueue();

function applyConfigValue(key: string, value: string): void {
  const tok = token;
  if (tok === null) {
    return;
  }
  queueConfigSave(key, () => applyConfigValueNow(key, value, tok));
}

function applyConfigValueNow(key: string, value: string, tok: string): Promise<void> {
  return setConfigValue(key, value, tok)
    .then((resp) => {
      store.set({
        configStatus: {
          key: resp.result.key,
          // Echo the CANONICAL value the writer reported, not the one sent.
          value: resp.result.value,
          notice: resp.result.requires_restart ? resp.restart_notice : "",
          error: "",
        },
      });
      refreshConfig();
    })
    .catch((err: unknown) => {
      store.set({ configStatus: { key, value: "", notice: "", error: errorText(err) } });
    });
}

function refreshTasks(): void {
  const tok = token;
  if (tok === null) {
    return;
  }
  void listTasks(tok)
    .then((tasks) => {
      // Reconcile the project scope against the new task set (redesign PR2): a
      // task-only project appears once its tasks load, and drops when its last task
      // is removed (if it also has no live sessions). This is what makes a task-only
      // repo reachable in the switcher and its tasks scoped to it.
      const selectedProject = reconcileProject(store.get().sessions, tasks, loadProjectChoice(), store.get().selectedProject);
      store.set({ tasks, selectedProject });
    })
    .catch(() => {
      // Transport/auth failure: leave the last-known list up; a task.* event or the
      // next mutation refetches. Nothing to surface here.
    });
}

/** Debounced task refetch for a burst of task.* events, mirroring requestResync. */
function requestTaskResync(): void {
  if (taskResyncTimer !== null) {
    return;
  }
  taskResyncTimer = window.setTimeout(() => {
    taskResyncTimer = null;
    refreshTasks();
  }, 150);
}

/** Opens the add-task modal, its project picker seeded from the live projects. On
 *  submit it builds a task.Task (a fresh id, exactly one trigger) and POSTs AddTask;
 *  the created task also arrives via a task.created event, and a refetch reconciles.
 *  Errors (a bad cron expression, a duplicate) surface in the modal for a retry. */
function openAddTask(): void {
  // The picker's projects come from sessions AND tasks (redesign PR2, Greptile
  // follow-on Fix 1), so a TASK-ONLY project is selectable and the default lands on
  // the currently-scoped project — adding a task targets ITS repo, and is never
  // blocked by the absence of a session.
  const projects = pickerProjects(store.get().sessions, store.get().tasks);
  openModal(
    addTaskModal(projects, store.get().selectedProject, {
      loadPrograms,
      onSubmit: (input: AddTaskInput) => {
        const tok = token;
        // `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696).
        if (tok === null || !modal) {
          return;
        }
        const m = modal;
        m.setBusy(true);
        void addTask(buildTask(input), tok)
          .then(() => {
            closeModal();
            refreshTasks();
          })
          .catch((e) => {
            m.setBusy(false);
            m.setError(describeError(e));
          });
      },
      onCancel: closeModal,
    }),
  );
}

/** Opens the edit-task modal seeded from the task, then submits the collected fields
 *  as a field-level UpdateTask patch (#1935) keyed by the task's stable id; the
 *  task.updated event + a refetch reconcile. Errors surface in the modal for a retry.
 *
 *  `enabled` is intentionally NOT in the patch — the toggle owns that bit, and
 *  omitting it preserves a disabled task's state across an edit (the field-level
 *  merge leaves an unsent field as-stored, #1700). Every other field the form
 *  collects IS sent, as an INLINE literal: the surface-parity field audit derives the
 *  web's reach from the values passed here (not the TS type), so each editable field
 *  must appear at this exact call site. The unused trigger is cleared to "" — safe on
 *  the HTTP/JSON path, which (unlike the CLI's gob socket) never elides "" to nil. */
function openEditTask(task: TaskData): void {
  const projects = pickerProjects(store.get().sessions, store.get().tasks);
  openModal(
    editTaskModal(projects, task, {
      loadPrograms,
      onSubmit: (input: AddTaskInput) => {
        const tok = token;
        // `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696).
        if (tok === null || !modal) {
          return;
        }
        const m = modal;
        m.setBusy(true);
        void updateTask(
          task.id,
          {
            name: input.name,
            prompt: input.prompt,
            cron_expr: input.trigger === "cron" ? input.cron : "",
            watch_cmd: input.trigger === "watch" ? input.watchCmd : "",
            target_session: input.targetSession,
            project_path: input.projectPath,
            program: input.program,
          },
          tok,
        )
          .then(() => {
            closeModal();
            refreshTasks();
          })
          .catch((e) => {
            m.setBusy(false);
            m.setError(describeError(e));
          });
      },
      onCancel: closeModal,
    }),
  );
}

/** Enables/disables a task (UpdateTask with `enabled` flipped), then refetches. A
 *  failure surfaces as the shared transient toast (task ops have no modal). Keys off
 *  the task's stable id — requireTaskID in the api layer refuses a missing one. */
function toggleTask(task: TaskData): void {
  const tok = token;
  if (tok === null) {
    return;
  }
  // Ship ONLY the flipped bit as a field-level patch (#1700): the toggle must
  // not carry the rest of this (possibly-stale) cached task, or it could revert a
  // concurrent edit another client made to the prompt/trigger/target.
  void updateTask(task.id, { enabled: !task.enabled }, tok)
    .then(refreshTasks)
    .catch((e) => surfaceTabError(e));
}

/** Fires a task now (TriggerTask), then refetches to pick up the new last-run. The
 *  tasks pane only offers this for enabled cron tasks (the daemon refuses disabled +
 *  watch tasks); a failure still surfaces as a toast. */
function doTriggerTask(task: TaskData): void {
  const tok = token;
  if (tok === null) {
    return;
  }
  void triggerTask(task.id, tok)
    .then(refreshTasks)
    .catch((e) => surfaceTabError(e));
}

/**
 * Resumes the selected session from its usage-limit wall (#1934) — the web's
 * analogue of the TUI's `c`.
 *
 * No confirm, matching the TUI: it is not destructive, and it re-delivers the
 * prompt the session was already going to run.
 *
 * No optimistic clear, DEPARTING from the TUI — deliberately. The TUI clears the
 * row's limit state locally for instant feedback; the web is a read-only
 * projection of the daemon's state (#960 single writer), so it lets the resulting
 * session.updated event drop the ◆ badge. Faking the cleared state here would put
 * the projection ahead of the daemon and show a resumed session that, if the
 * resume failed downstream, is still parked.
 *
 * The daemon refuses a session that is not actually limit-blocked, so a click that
 * races the limit clearing itself surfaces an error rather than an unwanted prompt.
 */
function doRetryLimit(): void {
  const sel = selectedSession();
  const tok = token;
  // `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696).
  if (!sel || tok === null) {
    return;
  }
  void resumeFromLimit(sel.id, sel.title, tok).catch((e) => surfaceTabError(e));
}

/** Removes a task (RemoveTask), then refetches. Keys off the stable id. */
function doRemoveTask(task: TaskData): void {
  const tok = token;
  if (tok === null) {
    return;
  }
  void removeTask(task.id, tok)
    .then(refreshTasks)
    .catch((e) => surfaceTabError(e));
}

/** The action callbacks the shell + login view invoke. */
// --- theme (redesign PR1) --------------------------------------------------

/** Applies a theme choice: persist it, stamp data-theme on <html> (the CSS resolves
 *  the right token layer synchronously), reflect it in the store so the appbar toggle
 *  highlights it, and re-theme the live terminals (xterm can't read CSS vars, so it's
 *  repainted from theme.ts's derived palette). */
function setTheme(choice: ThemeChoice): void {
  if (store.get().themeChoice === choice) {
    return;
  }
  persistThemeChoice(choice);
  stampTheme(choice);
  store.set({ themeChoice: choice });
  splitView.applyTheme();
}

/** While the choice is Auto, follow the OS: a prefers-color-scheme flip re-themes the
 *  terminals to match (the CSS chrome already reacts via the media query). An explicit
 *  Light/Dark choice ignores the OS. */
function watchSystemTheme(): void {
  let mql: MediaQueryList;
  try {
    mql = window.matchMedia("(prefers-color-scheme: dark)");
  } catch {
    return; // no matchMedia (very old / headless): nothing to follow
  }
  const onChange = (): void => {
    if (store.get().themeChoice === "auto") {
      splitView.applyTheme();
    }
  };
  if (typeof mql.addEventListener === "function") {
    mql.addEventListener("change", onChange);
  } else if (typeof mql.addListener === "function") {
    // Safari < 14 fallback.
    mql.addListener(onChange);
  }
}

const actions = {
  connect,
  disconnect,
  open: openFromRail,
  newSession,
  kill: () => openConfirm("kill"),
  archive: () => openConfirm("archive"),
  restore: () => openConfirm("restore"),
  retryLimit: doRetryLimit,
  switchTab,
  openTab,
  newTab: createSessionTab,
  closeTab: closeSessionTab,
  renameTab: renameSessionTab,
  reorderTab: reorderSessionTab,
  switchView,
  setConfigValue: applyConfigValue,
  switchProject,
  setStatusFilter,
  resetStatusFilter,
  addTask: openAddTask,
  editTask: openEditTask,
  toggleTask,
  triggerTask: doTriggerTask,
  removeTask: doRemoveTask,
  deleteProject: openDeleteProject,
  setTheme,
};

// --- attach terminal wiring ------------------------------------------------

/** Syncs the split view to the current selection: shows the selected session's layout
 *  (its retained tree, or a fresh single leaf), reconciling live terminals. Called on
 *  every store change, it is cheap on a no-op — the split view only rebuilds terminals
 *  when the session changes or the tab list shrinks/grows, so a live rail event never
 *  disturbs an open, focused pane. */
function syncSplit(state: AppState): void {
  const selId = state.selectedId;
  const tok = token;
  // The initial tab for a session shown for the first time: the store's activeTab
  // (reset to 0 on a fresh selection), clamped to the session's real tab list so a
  // stale index can never bind a pane to a tab that doesn't exist.
  const initialTab = clampActiveTab(state.sessions, selId, state.activeTab);
  const selected = selId ? state.sessions.find((s) => s.id === selId) : null;
  // The ordered tab identities, so the split view can (a) know the live tab count and
  // (b) reject a drop whose drag-time snapshot no longer matches (a mid-drag reorder).
  const tabIds = selected ? sessionTabs(selected).map(tabIdentity) : ["0:"];
  // The REAL daemon ids ("" where a tab has none), parallel to tabIds. The split view
  // needs BOTH: tabIds is its local identity/change-detection key (always non-empty,
  // synthesized when needed), while these are the only values it may send as a
  // ?tab_id= or trust as collision-proof (#1779).
  const tabRealIds = selected ? sessionTabs(selected).map(tabRealId) : [""];
  // The per-tab iframe target for web tabs (undefined for terminal tabs), parallel
  // to tabIds, so the split view can iframe a web leaf.
  const tabTargets = selected ? sessionTabs(selected).map((t) => t.url) : [];
  // The kind of each tab, parallel to tabIds — the split view reads it to tell a web
  // tab from a terminal one now that the identity is the opaque stable id (#1738).
  const tabKinds = selected ? sessionTabs(selected).map((t) => t.kind) : [];
  // Each tab's NAME, parallel to tabIds — what the pane headers render, with the kind,
  // through the same tablabel.ts derivation the tab bar uses (#1813). Before this the
  // panes had no name to render and drew a positional "Tab N" instead.
  const tabNames = selected ? sessionTabs(selected).map((t) => t.name) : [];
  // Whether the shown session is archived (#1809 follow-up): an archived session's
  // preserved web tabs render an inert placeholder rather than a live frame, and the
  // flip re-renders them on archive/restore even when the tab list is unchanged.
  const archived = selected ? isArchived(selected) : false;
  // `tok !== null` not `tok`: "" is the authorized-tokenless credential (#1696), so a
  // loopback client still attaches its live panes.
  splitView.setSession(
    tok !== null ? selId : null,
    tok,
    tabIds,
    initialTab,
    tabTargets,
    tabKinds,
    tabRealIds,
    archived,
    tabNames,
  );
}

function disposeSplit(): void {
  splitView.dispose();
  termHost.replaceChildren(); // clear any leftover pane DOM
}

// --- events plane wiring ---------------------------------------------------

function startStream(tok: string): void {
  stopStream();
  stream = new EventStream(tok, {
    onEvent,
    onResync: requestResync,
    onStatus: (s: EventStreamStatus) => store.set({ live: s }),
  });
  stream.start();
}

function stopStream(): void {
  if (resyncTimer !== null) {
    window.clearTimeout(resyncTimer);
    resyncTimer = null;
  }
  if (taskResyncTimer !== null) {
    window.clearTimeout(taskResyncTimer);
    taskResyncTimer = null;
  }
  if (stream) {
    stream.stop();
    stream = null;
  }
}

/**
 * Applies one events-plane delta to the store via the pure reducer (sessions.ts):
 * created/updated upsert in place, killed removes the row, and archived/restored
 * request a debounced re-Snapshot. The selection is re-validated (by id) against
 * the new list so a killed selected row clears — which syncTerminal then tears the
 * terminal down for.
 */
function onEvent(ev: WireEvent): void {
  // Task deltas (#1592 Phase 5 PR8) don't touch the session list; the daemon owns
  // tasks.json, so a task.created/updated/removed event just triggers a debounced
  // ListTasks refetch (the authoritative task projection). The session reducer
  // no-ops these, so run both: the reducer for session.* and the task refetch for
  // task.*.
  if (ev.type === "task.created" || ev.type === "task.updated" || ev.type === "task.removed") {
    requestTaskResync();
    return;
  }
  const { sessions, needsResync } = applyEvent(store.get().sessions, ev);
  applySessions(sessions);
  if (needsResync) {
    requestResync();
  }
}

/** Commits an externally-derived session list (an event delta or a resync) to the
 *  store, re-validating the selection AND the active tab against it. If the
 *  selection survives, the active tab is clamped to the (possibly changed) tab list
 *  so a tab closed/created out-of-band by another client can't leave the visible
 *  tab or the streamed tab pointing past the end; if the selection changed (e.g.
 *  the selected session was killed), the active tab resets to the agent tab. */
function applySessions(sessions: SessionData[]): void {
  const prevSel = store.get().selectedId;
  // Reconcile the project scope against the new session set (redesign PR2): a project
  // that vanished (its last session gone) falls back gracefully to the persisted/
  // default one, so the rail is never pinned to a dead root.
  const selectedProject = reconcileProject(sessions, store.get().tasks, loadProjectChoice(), store.get().selectedProject);
  let selectedId = pickSelection(sessions, prevSel);
  // Drop a selection that no longer belongs to the scoped project, so the terminal
  // never stays attached to a session hidden from the (now re-scoped) rail.
  if (selectedId) {
    const sel = sessions.find((s) => s.id === selectedId);
    if (sel && sel.worktree?.repo_path !== selectedProject) {
      selectedId = null;
    }
  }
  // An unchanged selection keeps its active tab; one the snapshot MOVED (the selected
  // session was archived/killed, so pickSelection landed elsewhere) takes the tab its
  // retained layout will settle on rather than asserting 0 (#1855, as moveSelection).
  const settled =
    selectedId === prevSel
      ? store.get().activeTab
      : splitView.settledTab(selectedId ?? "", tabIdsOf(sessions, selectedId));
  const activeTab = clampActiveTab(sessions, selectedId, settled);
  store.set({ sessions, selectedProject, selectedId, activeTab });
}

/** Re-fetches the authoritative Snapshot (debounced) and replaces the rail — the
 *  resync path for reconnects and partial archived/restored events. Failures are
 *  swallowed: the stream keeps running and a later event/reconnect retries. */
function requestResync(): void {
  if (resyncTimer !== null) {
    return;
  }
  resyncTimer = window.setTimeout(() => {
    resyncTimer = null;
    const tok = token;
    // `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696).
    if (tok === null) {
      return;
    }
    void fetchSnapshot(tok)
      .then((sessions) => {
        applySessions(sessions);
      })
      .catch(() => {
        // Transport/auth failure: the events stream owns reconnection; a later
        // reconnect fires onResync again. Nothing to surface here.
      });
  }, 150);
}

// --- keyboard navigation ---------------------------------------------------

/** Whether an element is a native control that should keep its own keys (a button,
 *  link, or form field) — as opposed to the rail/body or the terminal, which the
 *  nav state machine drives. Used to avoid hijacking Enter on a focused + New /
 *  Disconnect / pane-action button, or typing into a modal input. */
function isNativeControl(el: HTMLElement | null): boolean {
  if (!el) {
    return false;
  }
  return (
    el.tagName === "BUTTON" ||
    el.tagName === "INPUT" ||
    el.tagName === "TEXTAREA" ||
    el.tagName === "SELECT" ||
    el.tagName === "A"
  );
}

function onKeydown(e: KeyboardEvent): void {
  const state = store.get();
  if (state.phase !== "app") {
    return;
  }
  // Let native controls handle their own keys, EXCEPT the terminal (whose textarea
  // lives in termHost and is driven by the nav state machine) and Escape (which must
  // still close a modal / detach the terminal even from a focused field). Without
  // this a focused + New / Disconnect / pane-action button would have its Enter
  // hijacked as an attach, and modal typing would move the rail.
  const target = e.target as HTMLElement | null;
  const inTerminal = target ? termHost.contains(target) : false;
  if (!inTerminal && e.key !== "Escape" && isNativeControl(target)) {
    return;
  }
  // The mode is "terminal" only when a session is selected (so a pane exists to own
  // the keyboard) AND the sessions view is showing it; a killed selection or a switch
  // to the projects/tasks view (which hides the panes) must not leave the stored focus
  // stale, so we never honor terminal mode without a live, visible pane. The pure state
  // machine (nav.ts) decides the rest; index.ts only performs the effect.
  const focus: KeyboardFocus = state.selectedId && state.view === "sessions" ? state.focus : "rail";
  // The selected session's tab shape drives the nav-mode tab keys (1-9 / t / w).
  const selected = selectedSessionData();
  const action = decideKey(
    e.key,
    {
      focus,
      modalOpen: modal !== null,
      view: state.view,
      // j/k navigate only the VISIBLE rail: the ids in the order the rail shows them,
      // restricted to the selected project (redesign PR2) AND to the states the status
      // filter shows. Both restrictions are required — j/k must walk exactly the rows
      // on screen, or the selection lands on a row the user cannot see (the archive is
      // the whole point: hundreds of hidden rows would otherwise still be in the j/k
      // path, and holding j would appear to hang on nothing).
      orderedIds: filterSessions(
        orderedSessions(scopeToProject(state.sessions, state.selectedProject)),
        state.statusFilter,
      )
        // Creating rows are visible daemon state but not selectable: no terminal
        // exists until CreateSession resolves, and success owns the one atomic
        // upsert+selection that opens it attached.
        .filter((s) => !isCreating(s))
        .map((s) => s.id ?? "")
        .filter((id) => id !== ""),
      selectedId: state.selectedId,
      tabCount: selected ? sessionTabs(selected).length : 1,
      activeTab: state.activeTab,
      tabManagement: selected ? canManageTabs(selected) : false,
    },
    { alt: e.altKey },
  );
  if (action.kind === "none") {
    return;
  }
  // A handled key is ours: preventDefault AND stopPropagation so it never also
  // reaches xterm's textarea (capture phase) — this is what suppresses the stray
  // ESC byte when Escape detaches the terminal.
  e.preventDefault();
  e.stopPropagation();
  switch (action.kind) {
    case "closeModal":
      closeModal();
      break;
    case "select":
      moveSelection(action.id);
      break;
    case "attach":
      focusTerminal();
      break;
    case "toRail":
      focusRail();
      break;
    case "switchTab":
      switchTab(action.index);
      break;
    case "newTab":
      createSessionTab();
      break;
    case "closeTab":
      closeSessionTab(store.get().activeTab);
      break;
    case "switchView":
      switchView(action.view);
      break;
    case "cyclePane":
      splitView.cyclePane(action.delta);
      break;
    case "closePane":
      splitView.closeFocusedPane();
      break;
  }
}

/** Turns a probe failure into a message that tells the operator what to fix. All
 *  branches route the underlying value through errorText(), so a thrown non-Error
 *  (or an envelope error object) still renders as readable text rather than
 *  "[object Object]" or "undefined". */
function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 401) {
      return "That token was rejected. Check `af token show` on the host and try again.";
    }
    if (e.status === 0) {
      return `Couldn't reach the daemon. Confirm the listener address, then retry. (${errorText(e)})`;
    }
    return `Login failed: ${errorText(e)}`;
  }
  return `Login failed: ${errorText(e)}`;
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", mount, { once: true });
} else {
  mount();
}
