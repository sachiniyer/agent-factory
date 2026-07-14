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
// the same reason (events published while the socket was down are dropped by
// design — the hub is drop-slow, not replayed). The resync is debounced in
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
}

const BACKOFF_BASE_MS = 500;
const BACKOFF_MAX_MS = 10_000;

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
      // Constructor can throw on a malformed URL/state; treat as a drop.
      this.scheduleReconnect();
      return;
    }
    this.ws = ws;

    ws.onopen = () => {
      this.retry = 0;
      this.everOpened = true;
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

    ws.onclose = () => this.scheduleReconnect();
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
  }
}
