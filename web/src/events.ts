// The live-update transport for the sidebar (#1592 Phase 5 PR3): a subscriber to
// the daemon's /v1/events WebSocket (daemon/ws_events.go). It replaces polling
// entirely — the rail updates when the daemon publishes a session.* event, which
// it does for every create, kill, archive, restore, and liveness/limit transition
// (daemon/control_server.go, daemon/limit.go). This is the browser analogue of the
// TUI's push model: the web is a pure projection of daemon state, no client-side
// source of truth (design §2.1, §3.1).
//
// Events ride the same auth seam as the REST API; a browser WebSocket cannot set
// an Authorization header, so the token travels as the ?access_token= query param
// the daemon already accepts (agentproto/auth.go:23). The socket is server→client
// only; the client sends nothing. Browsers answer the daemon's WS keepalive ping
// at the protocol layer, so there is no client keepalive to write (design §4.4).
//
// On EVERY open — including the FIRST — the stream asks the caller to re-Snapshot.
// The first-open resync closes a login-window race (#1592 Phase 5 PR5): connect()
// takes the seed Snapshot BEFORE this socket opens, so any create/kill/archive
// that lands between that Snapshot and the socket's open would otherwise be lost
// (the socket wasn't yet subscribed to receive it). Re-Snapshotting once the
// socket is open — after which every subsequent event IS delivered — makes the
// rail whole regardless of what happened in that gap. Reconnect opens resync for
// the same reason (events published while the socket was down are not replayed
// by the hub). The resync is debounced in
// index.ts, so the extra first-open refetch collapses with any burst.

import type { WireEvent } from "./types.js";

/** The connection state surfaced to the UI for a subtle liveness indicator. */
export type EventStreamStatus = "connecting" | "open" | "reconnecting";

export interface EventStreamCallbacks {
  /** A parsed events-plane message (session.* / task.*). */
  onEvent(ev: WireEvent): void;
  /** Fired after EVERY open (first connect AND every reconnect): the caller
   *  should re-Snapshot because events may have been missed before the socket was
   *  subscribed — on the first open, the login-window race between the seed
   *  Snapshot and this open; on reconnects, the events dropped while down. */
  onResync(): void;
  /** Fired on every connection-state change, for the header indicator. */
  onStatus(status: EventStreamStatus): void;
  /** Fired when a streak of reconnect attempts has closed before `onopen`
   *  ever fired, and again every AUTH_FAILURE_THRESHOLD further failures while
   *  the streak lasts (so an inconclusive probe is retried) — the only visible signature that the daemon rejected the WS
   *  upgrade (the realistic cause is HTTP 401 after the operator rotated the
   *  access token; the browser's WS API exposes no HTTP status to JS, surfacing
   *  a rejected handshake as `onclose(1006)` with no `onopen`). The caller
   *  should issue an authenticated REST probe (its own resync): a 401 there
   *  trips `shouldForgetToken → disconnect()` and returns the SPA to login,
   *  instead of looping the WS reconnect on the revoked credential forever.
   *  A genuine network blip that recovers within one backoff cycle never
   *  reaches the threshold; only an unbroken streak of close-before-opens
   *  escalates, and the stream keeps trying in the meantime, so a transport
   *  failure that says nothing about the token leaves the loop intact. */
  onAuthFailure(): void;
}

const BACKOFF_BASE_MS = 500;
const BACKOFF_MAX_MS = 10_000;
// A WebSocket upgrade the daemon rejects at the auth gate (HTTP 401) fires
// `onclose(1006)` with no `onopen` — the browser exposes no HTTP status to JS,
// so EventStream cannot tell 401 from a transient drop at the WS layer. The
// reliable signal for "the credential was revoked" is therefore "the upgrade
// keeps failing to even open": a small threshold of consecutive closes-without-
// onopen escalates through `onAuthFailure`. The threshold is a streak
// (not a count of total failures) because the first successful `onopen` resets
// it — so a single network blip that recovers, or a one-off WS handshake race,
// does not trip the escalation, while an idle client whose token has been
// rotated after a transport drop does, reliably, instead of looping forever
// (#1674 regression). Three keeps detection inside a few backoff cycles
// (≈3.5s) while staying clear of the multi-second network-outage window the
// test/console harnesses use to exercise transport reconnects.
//
// Escalation re-arms: it fires at the threshold and again every
// AUTH_FAILURE_THRESHOLD further close-before-opens while the streak lasts —
// not on every failed reconnect (that would spam the caller's probe), and not
// once per streak. The probe is an authenticated resync whose inconclusive
// outcomes (a status-0 transport failure, a 5xx, a request coalesced into or
// superseded by another resync) all retain the token and report nothing back,
// so a one-shot escalation left a revoked credential looping forever once REST
// recovered. Re-arming by count needs no probe outcome from the caller, so no
// inconclusive path can leave it disarmed; once backoff saturates at
// BACKOFF_MAX_MS it is one probe per ~30s. A successful open resets both the
// streak and the escalation point, so a later revocation escalates afresh.
//
// Comments inside the class below stay short on purpose: the web bundle is
// built unminified and ships them, against the perf gzip budget.
const AUTH_FAILURE_THRESHOLD = 3;

/** Returns the WS scheme matching the page origin: ws: under the daemon's plain
 *  HTTP listener (the normal case), wss: when a reverse proxy serves the page over
 *  https:. */
function wsScheme(): string {
  return window.location.protocol === "https:" ? "wss:" : "ws:";
}

/**
 * A self-healing subscription to /v1/events. Call start() once after login and
 * stop() on logout; it owns its reconnect loop and never throws to the caller —
 * transport failures become reconnect attempts, not exceptions.
 */
export class EventStream {
  private ws: WebSocket | null = null;
  private stopped = false;
  private everOpened = false;
  private retry = 0;
  private reconnectTimer: number | null = null;
  // Streak of attempts closed before onopen, and the streak length at which
  // onAuthFailure next fires (see AUTH_FAILURE_THRESHOLD).
  private consecutiveCloseBeforeOpen = 0;
  private nextAuthEscalationAt = AUTH_FAILURE_THRESHOLD;

  constructor(
    private readonly token: string,
    private readonly cb: EventStreamCallbacks,
  ) {}

  /** Opens the socket and begins delivering events. Idempotent-ish: call once. */
  start(): void {
    this.stopped = false;
    this.open();
  }

  /** Permanently closes the stream and cancels any pending reconnect. */
  stop(): void {
    this.stopped = true;
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      // Drop our handlers first so the close doesn't schedule a reconnect.
      this.ws.onopen = null;
      this.ws.onmessage = null;
      this.ws.onclose = null;
      this.ws.onerror = null;
      this.ws.close();
      this.ws = null;
    }
  }

  private open(): void {
    this.cb.onStatus(this.everOpened ? "reconnecting" : "connecting");
    const url = `${wsScheme()}//${window.location.host}/v1/events?access_token=${encodeURIComponent(this.token)}`;
    let ws: WebSocket;
    try {
      ws = new WebSocket(url);
    } catch {
      // Constructor can throw on a malformed URL/state; a close-before-open.
      this.handleClose(false);
      return;
    }
    this.ws = ws;

    // False until this attempt opens: a rejected upgrade never does.
    let opened = false;

    ws.onopen = () => {
      opened = true;
      this.retry = 0;
      this.everOpened = true;
      this.consecutiveCloseBeforeOpen = 0;
      this.nextAuthEscalationAt = AUTH_FAILURE_THRESHOLD;
      this.cb.onStatus("open");
      // Re-Snapshot on EVERY open. The FIRST open closes the login-window race:
      // the seed Snapshot was taken before this socket existed, so a mutation in
      // that gap would be lost without this refetch. A RE-connect closes the
      // dropped-while-down gap. Both funnel through the same debounced resync.
      this.cb.onResync();
    };

    ws.onmessage = (e) => {
      // The events plane sends JSON text frames (agentproto.WriteControl); binary
      // frames belong to the PTY stream (PR4) and never arrive here.
      if (typeof e.data !== "string") {
        return;
      }
      let ev: WireEvent;
      try {
        ev = JSON.parse(e.data) as WireEvent;
      } catch {
        return; // Ignore a malformed frame rather than tear down the stream.
      }
      if (ev && typeof ev.type === "string") {
        this.cb.onEvent(ev);
      }
    };

    ws.onclose = () => this.handleClose(opened);
    ws.onerror = () => {
      // onerror is followed by onclose; close here so a socket stuck in a half-open
      // state still funnels through the single reconnect path.
      try {
        ws.close();
      } catch {
        // already closing
      }
    };
  }

  /** Single funnel for every socket end; only a close-before-open extends
   *  the streak. */
  private handleClose(opened: boolean): void {
    if (!opened) {
      this.consecutiveCloseBeforeOpen += 1;
    }
    this.scheduleReconnect();
  }

  private scheduleReconnect(): void {
    if (this.stopped || this.reconnectTimer !== null) {
      return;
    }
    this.ws = null;
    this.cb.onStatus("reconnecting");
    const delay = Math.min(BACKOFF_BASE_MS * 2 ** this.retry, BACKOFF_MAX_MS);
    this.retry += 1;
    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = null;
      if (!this.stopped) {
        this.open();
      }
    }, delay);
    // Fire after arming so the stream owns its reconnect before the caller's
    // probe runs; the loop keeps trying meanwhile (see AUTH_FAILURE_THRESHOLD).
    if (this.consecutiveCloseBeforeOpen >= this.nextAuthEscalationAt) {
      this.nextAuthEscalationAt =
        this.consecutiveCloseBeforeOpen + AUTH_FAILURE_THRESHOLD;
      this.cb.onAuthFailure();
    }
  }
}
