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
  deleteProject,
  fetchSnapshot,
  killSession,
  listTasks,
  loadToken,
  probeAuthRequired,
  probeToken,
  removeTask,
  sendPrompt,
  storeToken,
  triggerTask,
  updateTask,
} from "./api.js";
import { EventStream, type EventStreamStatus } from "./events.js";
import { confirmDeleteProjectModal, confirmModal, type ModalHandle, newSessionModal, promptModal } from "./modals.js";
import { decideKey, type KeyboardFocus, type View } from "./nav.js";
import { applyEvent, clampActiveTab, pickSelection, upsertSession } from "./sessions.js";
import { SplitView } from "./split.js";
import { Store } from "./store.js";
import { bootStampTheme, persistThemeChoice, stampTheme, type ThemeChoice } from "./theme.js";
import { addTaskModal, type AddTaskInput, buildTask } from "./tasks.js";
import type { TerminalStatus } from "./terminal.js";
import {
  AppShell,
  deriveProjects,
  orderedSessions,
  renderLogin,
  sessionTabs,
  supportsTabManagement,
  tabIdentity,
  type AppState,
} from "./ui.js";
import type { SessionData, TaskData, WireEvent } from "./types.js";

// Boot stamp (redesign PR1): apply the saved theme choice to <html> BEFORE the app
// mounts (before first paint), so an explicit light/dark choice shows no flash. This
// is the CSP-safe stamp — it ships in the same-origin bundle, not an inline <script>
// the daemon's `default-src 'self'` would block. Runs at module top, above the store.
const initialThemeChoice = bootStampTheme();

const store = new Store<AppState>({
  phase: "login",
  view: "sessions",
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
// send-prompt/attach would be silently skipped because `!"" === true`.
let token: string | null = null;
let stream: EventStream | null = null;
// Debounces the re-Snapshot that archived/restored events and reconnects trigger,
// so a burst of events collapses into a single authoritative refetch.
let resyncTimer: number | null = null;
// Debounces the ListTasks refetch that task.* events trigger (#1592 Phase 5 PR8),
// so a burst of task deltas collapses into one authoritative refetch.
let taskResyncTimer: number | null = null;
// The auto-dismiss timer for the tab-error toast, and how long it shows.
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
 *  with no credential. If it does, resume a token kept in sessionStorage across a
 *  reload, else land on the paste-token login. A probe transport failure falls back
 *  to the token login — never auto-connects on uncertainty. */
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
    shell = new AppShell(actions, termHost, modalHost);
    root.replaceChildren(shell.el);
  }
  shell.update(state);
  syncSplit(state);
}

/** Validates the token (Snapshot probe), and on success persists it, seeds the rail
 *  from the probe's snapshot, subscribes to /v1/events, and shows the app; on
 *  failure clears it and surfaces an actionable error on the login view. */
async function connect(candidate: string): Promise<void> {
  store.set({ connecting: true, loginError: null });
  let sessions: SessionData[];
  try {
    sessions = await probeToken(candidate);
  } catch (e) {
    clearToken();
    store.set({ phase: "login", connecting: false, loginError: describeError(e) });
    return;
  }
  token = candidate;
  // Persist only a REAL token so a reload can resume it; the empty-token sentinel
  // (no-auth client, #1696) is never worth storing — bootstrap re-probes on reload.
  if (candidate !== "") {
    storeToken(candidate);
  }
  store.set({
    phase: "app",
    view: "sessions",
    connecting: false,
    loginError: null,
    sessions,
    selectedId: pickSelection(sessions, store.get().selectedId),
    live: "connecting",
    focus: "rail",
    activeTab: 0,
    shownTabs: [0],
    tabError: null,
    tasks: [],
  });
  startStream(candidate);
  // Seed the tasks view alongside the rail: one ListTasks fetch so the tasks pane is
  // populated the moment the user switches to it. Task deltas then arrive via the
  // task.* events plane (onEvent), which triggers a debounced refetch.
  refreshTasks();
}

/** Forgets the token, tears down the push stream + terminal, and returns to login. */
function disconnect(): void {
  stopStream();
  closeModal();
  token = null;
  clearToken();
  store.set({
    phase: "login",
    view: "sessions",
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

/** Moves the rail selection to a session by its stable id and puts the keyboard in
 *  rail-nav mode. This is the keyboard path (j/k): selecting a row never steals the
 *  keyboard into the terminal — you attach explicitly with Enter. Resetting focus to
 *  "rail" here also clears any stale "terminal" mode left by a since-disposed
 *  terminal, so j/k keep working after the selected session goes away. Selecting a
 *  session always resets the active tab to its agent tab (index 0). */
function moveSelection(id: string): void {
  clearTabError();
  store.set({ selectedId: id, focus: "rail", activeTab: 0 });
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
 *  the "terminal" mode; setting it here first keeps the indicator immediate. */
function focusTerminal(): void {
  store.set({ focus: "terminal" });
  splitView.focus();
}

/** Returns the keyboard to the rail (Escape / detach): blurs every pane so
 *  document-level j/k navigate again. */
function focusRail(): void {
  store.set({ focus: "rail" });
  splitView.blur();
}

// --- top-level view switching (#1592 Phase 5 PR8) --------------------------

/** Switches the top-level view (sessions | projects | tasks). Leaving the sessions
 *  view hands the keyboard back to the rail AND blurs the (still-live but now hidden)
 *  terminal, so a stray key in the projects/tasks view never leaks to the agent — the
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

/** Opens the new-session modal, its picker seeded from the live projects. On
 *  submit it creates the session via the daemon; the created row arrives via the
 *  events stream. Errors (e.g. a bad repo) surface in the modal for a retry. */
function newSession(): void {
  const projects = deriveProjects(store.get().sessions);
  openModal(
    newSessionModal(projects, {
      onSubmit: (values: CreateSessionInput) => {
        const tok = token;
        // `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696).
        if (tok === null || !modal) {
          return;
        }
        const m = modal;
        m.setBusy(true);
        void createSession(values, tok)
          .then((created) => {
            closeModal();
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
            m.setBusy(false);
            m.setError(describeError(e));
          });
      },
      onCancel: closeModal,
    }),
  );
}

/** Opens the send-prompt modal for the selected session. */
function openSendPrompt(): void {
  const sel = selectedSession();
  if (!sel) {
    return;
  }
  openModal(
    promptModal(sel.title, {
      onSubmit: (text: string) => {
        const tok = token;
        // `=== null` not `!tok`: "" is the authorized-tokenless credential (#1696).
        if (tok === null || !modal) {
          return;
        }
        const m = modal;
        m.setBusy(true);
        void sendPrompt(sel.id, sel.title, text, tok)
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

/** Opens the kill/archive confirm modal for the selected session. */
function openConfirm(action: "kill" | "archive"): void {
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
        const run =
          action === "kill" ? killSession(sel.id, sel.title, tok) : archiveSession(sel.id, sel.title, tok);
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

/** Creates a $SHELL tab on the selected session (the `t` key / + button), then
 *  resyncs to pull the grown tab list (the daemon emits no event for a tab change),
 *  selects the new tab, and attaches it — mirroring the TUI's `t`, which opens the
 *  fresh tab as a pane. Errors (e.g. a remote session, or the tab cap) surface on
 *  the pane header's status line. */
function createSessionTab(): void {
  const sel = selectedSessionData();
  const tok = token;
  // `tok === null` not `!tok`: "" is the authorized-tokenless credential (#1696),
  // so a loopback client can still create tabs.
  if (!sel || tok === null || !supportsTabManagement(sel)) {
    return;
  }
  clearTabError();
  const selId = sel.id ?? "";
  void createTab(selId, sel.title, tok)
    .then(() => fetchSnapshot(tok))
    .then((sessions) => {
      const grown = sessions.find((s) => s.id === selId);
      const newIdx = grown ? sessionTabs(grown).length - 1 : store.get().activeTab;
      // Commit the grown tab list first (rerender → syncSplit sees the new tabCount),
      // then point the focused pane at the fresh tab and attach it — mirroring the
      // TUI's `t`, which opens the new tab as the active pane.
      store.set({ sessions, selectedId: pickSelection(sessions, store.get().selectedId) });
      splitView.setFocusedTab(newIdx);
      focusTerminal();
    })
    .catch((e) => surfaceTabError(e));
}

/** Closes the tab at `index` of the selected session (the `w` key / × button),
 *  then resyncs the shrunk tab list and re-points the active tab. The agent tab
 *  (index 0) is unclosable. */
function closeSessionTab(index: number): void {
  const sel = selectedSessionData();
  const tok = token;
  // `tok === null` not `!tok`: "" is the authorized-tokenless credential (#1696).
  if (!sel || tok === null || index <= 0 || !supportsTabManagement(sel)) {
    return;
  }
  const tabs = sessionTabs(sel);
  const target = tabs[index];
  if (!target) {
    return;
  }
  clearTabError();
  const selId = sel.id ?? "";
  void closeTab(selId, sel.title, target.name, tok)
    .then(() => fetchSnapshot(tok))
    .then((sessions) => {
      // The close shifts every higher tab down by one: if the closed tab was at or
      // before the active one, the active index moves left to keep showing the same
      // tab (or its left neighbor when the active tab itself was closed).
      const cur = store.get().activeTab;
      const shrunk = sessions.find((s) => s.id === selId);
      const n = shrunk ? sessionTabs(shrunk).length : 1;
      const next = Math.min(Math.max(index <= cur ? cur - 1 : cur, 0), n - 1);
      // Commit the shrunk tab list first (rerender → syncSplit re-validates the tree,
      // clamping any pane past the new end), then re-point the focused pane.
      store.set({ sessions, selectedId: pickSelection(sessions, store.get().selectedId) });
      splitView.setFocusedTab(next);
    })
    .catch((e) => surfaceTabError(e));
}

/** Surfaces a failed tab mutation as a transient toast (there is no modal for tab
 *  ops, unlike create/kill/archive). The terminal keeps streaming; the message is
 *  the cue to fix the cause (e.g. the tab cap or a remote session). It auto-clears
 *  after a few seconds, and a fresh failure resets the timer. */
function surfaceTabError(e: unknown): void {
  // The raw error message — NOT describeError, whose "Login failed…/Couldn't reach
  // the daemon…" framing is for the login probe. A tab op carries the daemon's own
  // message (e.g. the tab cap) or the fail-closed "no stable id" refusal verbatim.
  const msg = e instanceof ApiError ? e.message : (e as Error).message;
  console.error("af-web: tab operation failed:", msg);
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
function refreshTasks(): void {
  const tok = token;
  if (tok === null) {
    return;
  }
  void listTasks(tok)
    .then((tasks) => store.set({ tasks }))
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
  const projects = deriveProjects(store.get().sessions);
  openModal(
    addTaskModal(projects, {
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
  sendPrompt: openSendPrompt,
  kill: () => openConfirm("kill"),
  archive: () => openConfirm("archive"),
  switchTab,
  openTab,
  newTab: createSessionTab,
  closeTab: closeSessionTab,
  switchView,
  addTask: openAddTask,
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
  // The per-tab iframe target for web tabs (undefined for terminal tabs), parallel
  // to tabIds, so the split view can iframe a web leaf.
  const tabTargets = selected ? sessionTabs(selected).map((t) => t.url) : [];
  // `tok !== null` not `tok`: "" is the authorized-tokenless credential (#1696), so a
  // loopback client still attaches its live panes.
  splitView.setSession(tok !== null ? selId : null, tok, tabIds, initialTab, tabTargets);
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
  const selectedId = pickSelection(sessions, prevSel);
  const activeTab = selectedId === prevSel ? clampActiveTab(sessions, selectedId, store.get().activeTab) : 0;
  store.set({ sessions, selectedId, activeTab });
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
      orderedIds: orderedSessions(state.sessions)
        .map((s) => s.id ?? "")
        .filter((id) => id !== ""),
      selectedId: state.selectedId,
      tabCount: selected ? sessionTabs(selected).length : 1,
      activeTab: state.activeTab,
      tabManagement: selected ? supportsTabManagement(selected) : false,
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

/** Turns a probe failure into a message that tells the operator what to fix. */
function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 401) {
      return "That token was rejected. Check `af token show` on the host and try again.";
    }
    if (e.status === 0) {
      return `Couldn't reach the daemon. Confirm the listener address, then retry. (${e.message})`;
    }
    return `Login failed: ${e.message}`;
  }
  return `Login failed: ${(e as Error).message}`;
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", mount, { once: true });
} else {
  mount();
}
