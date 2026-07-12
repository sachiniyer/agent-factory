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
  ApiError,
  archiveSession,
  clearToken,
  createSession,
  type CreateSessionInput,
  fetchSnapshot,
  killSession,
  loadToken,
  probeToken,
  sendPrompt,
  storeToken,
} from "./api.js";
import { EventStream, type EventStreamStatus } from "./events.js";
import { confirmModal, type ModalHandle, newSessionModal, promptModal } from "./modals.js";
import { applyEvent, pickSelection, upsertSession } from "./sessions.js";
import { Store } from "./store.js";
import { AttachTerminal, type TerminalStatus } from "./terminal.js";
import { AppShell, deriveProjects, orderedSessions, renderLogin, type AppState } from "./ui.js";
import type { SessionData, WireEvent } from "./types.js";

const store = new Store<AppState>({
  phase: "login",
  connecting: false,
  loginError: null,
  sessions: [],
  selectedId: null,
  live: "connecting",
  termStatus: "connecting",
});

// The credential and the push stream are process-local singletons: one token
// drives the REST resync fetch, the events WS, and the PTY stream; one events
// stream is live at a time (started on connect, stopped on disconnect).
let token: string | null = null;
let stream: EventStream | null = null;
// Debounces the re-Snapshot that archived/restored events and reconnects trigger,
// so a burst of events collapses into a single authoritative refetch.
let resyncTimer: number | null = null;

// The app-phase DOM (built once per login) and the persistent terminal host that
// lives inside it. Keeping the host stable across renders is what lets the focused
// xterm survive rail updates (see ui.ts AppShell).
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

// The one live attach terminal and the session id it is bound to. Rebuilt only
// when the selected id changes; disposed on deselect/logout.
let terminal: AttachTerminal | null = null;
let terminalId: string | null = null;

let root: HTMLElement | null = null;

function mount(): void {
  root = document.getElementById("app");
  if (!root) {
    throw new Error("af-web: #app root element missing from index.html");
  }
  store.subscribe(rerender);
  rerender();

  // Keyboard navigation over the rail (arrows / j / k), wired once at the document
  // level. The handler ignores keys while a form field OR the terminal textarea has
  // focus, so typing into the agent never moves the rail selection.
  document.addEventListener("keydown", onKeydown);

  // Resume within the tab: sessionStorage keeps the token across a reload, so a
  // returning tab re-probes it silently and skips the login form on success.
  const existing = loadToken();
  if (existing) {
    void connect(existing);
  }
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
    disposeTerminal();
    closeModal();
    renderLogin(root, state, actions);
    return;
  }
  if (!shell) {
    shell = new AppShell(actions, termHost, modalHost);
    root.replaceChildren(shell.el);
  }
  shell.update(state);
  syncTerminal(state);
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
  storeToken(candidate);
  store.set({
    phase: "app",
    connecting: false,
    loginError: null,
    sessions,
    selectedId: pickSelection(sessions, store.get().selectedId),
    live: "connecting",
  });
  startStream(candidate);
}

/** Forgets the token, tears down the push stream + terminal, and returns to login. */
function disconnect(): void {
  stopStream();
  closeModal();
  token = null;
  clearToken();
  store.set({
    phase: "login",
    connecting: false,
    loginError: null,
    sessions: [],
    selectedId: null,
    live: "connecting",
  });
}

/** Selects a session row by its stable id (click or keyboard). */
function select(id: string): void {
  store.set({ selectedId: id });
}

// --- lifecycle actions (modals) --------------------------------------------

/** The title of the currently selected session, or null. */
function selectedTitle(): string | null {
  const { sessions, selectedId } = store.get();
  return sessions.find((s) => s.id === selectedId)?.title ?? null;
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
        if (!tok || !modal) {
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
              store.set({ sessions, selectedId: created.id });
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
  const title = selectedTitle();
  if (!title) {
    return;
  }
  openModal(
    promptModal(title, {
      onSubmit: (text: string) => {
        const tok = token;
        if (!tok || !modal) {
          return;
        }
        const m = modal;
        m.setBusy(true);
        void sendPrompt(title, text, tok)
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
  const title = selectedTitle();
  if (!title) {
    return;
  }
  openModal(
    confirmModal({
      action,
      sessionTitle: title,
      onConfirm: () => {
        const tok = token;
        if (!tok || !modal) {
          return;
        }
        const m = modal;
        m.setBusy(true);
        const run = action === "kill" ? killSession(title, tok) : archiveSession(title, tok);
        void run.then(closeModal).catch((e) => {
          m.setBusy(false);
          m.setError(describeError(e));
        });
      },
      onCancel: closeModal,
    }),
  );
}

/** The action callbacks the shell + login view invoke. */
const actions = {
  connect,
  disconnect,
  select,
  newSession,
  sendPrompt: openSendPrompt,
  kill: () => openConfirm("kill"),
  archive: () => openConfirm("archive"),
};

// --- attach terminal wiring ------------------------------------------------

/** Opens/closes the attach terminal to match the current selection. Rebuilds only
 *  when the selected id actually changes, so a live rail event never disturbs an
 *  open, focused terminal. */
function syncTerminal(state: AppState): void {
  const selId = state.selectedId;
  if (selId === terminalId) {
    return;
  }
  disposeTerminal();
  terminalId = selId; // set before constructing so the onStatus re-render is a no-op
  const tok = token;
  if (selId && tok) {
    terminal = new AttachTerminal(termHost, selId, tok, {
      onStatus: (s: TerminalStatus) => store.set({ termStatus: s }),
    });
  }
}

function disposeTerminal(): void {
  if (terminal) {
    terminal.dispose();
    terminal = null;
  }
  terminalId = null;
  termHost.replaceChildren(); // clear any leftover xterm DOM
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
  const { sessions, needsResync } = applyEvent(store.get().sessions, ev);
  store.set({ sessions, selectedId: pickSelection(sessions, store.get().selectedId) });
  if (needsResync) {
    requestResync();
  }
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
    if (!tok) {
      return;
    }
    void fetchSnapshot(tok)
      .then((sessions) => {
        store.set({ sessions, selectedId: pickSelection(sessions, store.get().selectedId) });
      })
      .catch(() => {
        // Transport/auth failure: the events stream owns reconnection; a later
        // reconnect fires onResync again. Nothing to surface here.
      });
  }, 150);
}

// --- keyboard navigation ---------------------------------------------------

function onKeydown(e: KeyboardEvent): void {
  const state = store.get();
  if (state.phase !== "app") {
    return;
  }
  // A modal owns the keyboard while open: Escape cancels it, and rail navigation is
  // suppressed so arrows/j/k move within the form, not the rail behind it.
  if (modal) {
    if (e.key === "Escape") {
      e.preventDefault();
      closeModal();
    }
    return;
  }
  // Don't hijack typing in a form field or the terminal (xterm's helper textarea):
  // j/k and arrows there belong to the agent, not the rail.
  const target = e.target as HTMLElement | null;
  if (target && (target.tagName === "INPUT" || target.tagName === "TEXTAREA")) {
    return;
  }
  let delta = 0;
  if (e.key === "ArrowDown" || e.key === "j") {
    delta = 1;
  } else if (e.key === "ArrowUp" || e.key === "k") {
    delta = -1;
  } else {
    return;
  }
  const ordered = orderedSessions(state.sessions);
  if (ordered.length === 0) {
    return;
  }
  e.preventDefault();
  const cur = ordered.findIndex((s) => s.id === state.selectedId);
  // From no selection, ArrowDown lands on the first row and ArrowUp on the last.
  let next: number;
  if (cur === -1) {
    next = delta > 0 ? 0 : ordered.length - 1;
  } else {
    next = Math.min(Math.max(cur + delta, 0), ordered.length - 1);
  }
  const target2 = ordered[next];
  if (target2 && target2.id) {
    select(target2.id);
  }
}

/** Turns a probe failure into a message that tells the operator what to fix. */
function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 401) {
      return "That token was rejected. Check `af token show` on the host and try again.";
    }
    if (e.status === 0) {
      return `Couldn't reach the daemon. Confirm the listener address and TLS, then retry. (${e.message})`;
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
