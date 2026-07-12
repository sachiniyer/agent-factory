// Entry point for the Agent Factory web client (#1592 Phase 5). PR3 fills the
// authed app with the live session rail: on login the app fetches Snapshot to seed
// the rail, then subscribes to /v1/events and applies deltas so the list stays in
// lock-step with the daemon — no polling. Creating/killing a session in the TUI or
// via the CLI shows up in the browser through the push stream (design §2.1).
//
// The bundle is fully self-contained (esbuild output + the imported CSS, no CDN,
// no off-origin fetch) so the daemon's `Content-Security-Policy: default-src
// 'self'` holds — the only network calls are same-origin /v1/ requests + the
// same-origin /v1/events WebSocket.

import "./styles.css";
import { ApiError, clearToken, fetchSnapshot, loadToken, probeToken, storeToken } from "./api.js";
import { EventStream, type EventStreamStatus } from "./events.js";
import { applyEvent, pickSelection } from "./sessions.js";
import { Store } from "./store.js";
import { orderedSessions, render, type AppState } from "./ui.js";
import type { SessionData, WireEvent } from "./types.js";

const store = new Store<AppState>({
  phase: "login",
  connecting: false,
  loginError: null,
  sessions: [],
  selectedTitle: null,
  live: "connecting",
});

// The credential and the push stream are process-local singletons: one token
// drives both the REST resync fetch and the events WS, and one stream is live at a
// time (started on connect, stopped on disconnect).
let token: string | null = null;
let stream: EventStream | null = null;
// Debounces the re-Snapshot that archived/restored events and reconnects trigger,
// so a burst of events collapses into a single authoritative refetch.
let resyncTimer: number | null = null;

function mount(): void {
  const root = document.getElementById("app");
  if (!root) {
    throw new Error("af-web: #app root element missing from index.html");
  }
  const rerender = () => render(root, store.get(), { connect, disconnect, select });
  store.subscribe(rerender);
  rerender();

  // Keyboard navigation over the rail (arrows / j / k), wired once at the document
  // level so it works regardless of which row last had focus.
  document.addEventListener("keydown", onKeydown);

  // Resume within the tab: sessionStorage keeps the token across a reload, so a
  // returning tab re-probes it silently and skips the login form on success.
  const existing = loadToken();
  if (existing) {
    void connect(existing);
  }
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
    selectedTitle: pickSelection(sessions, store.get().selectedTitle),
    live: "connecting",
  });
  startStream(candidate);
}

/** Forgets the token, tears down the push stream, and returns to the login view. */
function disconnect(): void {
  stopStream();
  token = null;
  clearToken();
  store.set({
    phase: "login",
    connecting: false,
    loginError: null,
    sessions: [],
    selectedTitle: null,
    live: "connecting",
  });
}

/** Selects a session row (click or keyboard). */
function select(title: string): void {
  store.set({ selectedTitle: title });
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
 * created/updated upsert in place (the instant, poll-free path the create/status
 * play-test checks), killed removes the row, and archived/restored request a
 * debounced re-Snapshot because the event can't convey the new liveness. The
 * selection is re-validated against the new list so a killed selected row clears.
 */
function onEvent(ev: WireEvent): void {
  const { sessions, needsResync } = applyEvent(store.get().sessions, ev);
  store.set({ sessions, selectedTitle: pickSelection(sessions, store.get().selectedTitle) });
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
        store.set({ sessions, selectedTitle: pickSelection(sessions, store.get().selectedTitle) });
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
  // Don't hijack typing in a form field (the login input, future modals).
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
  const cur = ordered.findIndex((s) => s.title === state.selectedTitle);
  // From no selection, ArrowDown lands on the first row and ArrowUp on the last.
  let next: number;
  if (cur === -1) {
    next = delta > 0 ? 0 : ordered.length - 1;
  } else {
    next = Math.min(Math.max(cur + delta, 0), ordered.length - 1);
  }
  const target2 = ordered[next];
  if (target2) {
    select(target2.title);
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
