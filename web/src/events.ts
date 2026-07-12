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
// On any drop (network blip, daemon restart) the stream reconnects with capped
// exponential backoff, and on every RE-connect it asks the caller to re-Snapshot —
// events published during the gap are lost by design (the hub is drop-slow, not
// replayed), so a fresh Snapshot is how the rail re-synchronises. This mirrors the
// plan's "reconnect + re-Snapshot" resume for a plain event stream.

import type { WireEvent } from "./types.js";

/** The connection state surfaced to the UI for a subtle liveness indicator. */
export type EventStreamStatus = "connecting" | "open" | "reconnecting";

export interface EventStreamCallbacks {
  /** A parsed events-plane message (session.* / task.*). */
  onEvent(ev: WireEvent): void;
  /** Fired after a RE-connect (not the first connect): the caller should
   *  re-Snapshot because events may have been missed during the gap. */
  onResync(): void;
  /** Fired on every connection-state change, for the header indicator. */
  onStatus(status: EventStreamStatus): void;
}

const BACKOFF_BASE_MS = 500;
const BACKOFF_MAX_MS = 10_000;

/** Returns the WS scheme matching the page origin: wss: under https: (the TLS TCP
 *  listener the SPA is served from), ws: otherwise. */
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
      const wasReconnect = this.everOpened;
      this.everOpened = true;
      this.cb.onStatus("open");
      // A reconnect may have missed events; re-Snapshot to resynchronise. The
      // first-ever open needs no resync — the caller already seeded from Snapshot.
      if (wasReconnect) {
        this.cb.onResync();
      }
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
