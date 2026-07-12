// The attach terminal (#1592 Phase 5 PR4) — THE flagship: a live xterm.js pane
// over the daemon's binary WS PTY stream (session/ptybroker.go, daemon/ws_pty.go).
// It makes the web app a SECOND, non-Go client of the same multi-writer PTY plane
// the TUI/apiclient speak: it decodes the opcode-framed binary protocol with the
// TS codec (frame.ts, byte-identical to agentproto/frame.go) and renders it into
// xterm, sends keystrokes back as OpInput, and drives OpResize from a FitAddon.
//
// The three hard parts the plan calls out (design §4) are all handled here:
//
//   - Multi-writer resize (§4.1). The stream accepts INPUT/RESIZE from ANY
//     subscriber; the ONE cross-client conflict — size — is resolved server-side
//     by last-resize-wins with an authoritative MsgResize echo. So this client
//     drives resize from its own FitAddon but ALSO obeys the server's echo: on a
//     MsgResize it resizes xterm to the echoed size, never assuming its local fit
//     stuck (another tab may have resized after it).
//
//   - Reconnect + replay (§4.2/§4.3). The stream is a bounded ring with
//     ?since=<seq> replay. A browser WebSocket cannot read the X-Af-Stream-Seq
//     handshake header, so the broker also emits the start seq in-band as the
//     first frame (OpHello, PR1). This client seeds its absolute cursor from that
//     hello, advances it by each OpPTYOut's byte count (OpRepaint is per-subscriber
//     and NOT counted — design §4.2), and on any drop reconnects with
//     ?since=<cursor> to replay exactly the gap: no full re-render, no lost or
//     doubled bytes. Re-seeding the cursor from every hello also absorbs a ring
//     eviction (the broker clamps a too-old ?since up to its retained base).
//
//   - Keepalive (§4.4). The broker pings each subscriber; browsers answer WS pings
//     at the protocol layer, so there is no client keepalive to write — only the
//     reconnect loop for a socket that actually drops.
//
// It stays self-contained for the daemon's `default-src 'self'` CSP: xterm's JS is
// bundled by esbuild and its CSS is imported here so esbuild folds it into the
// same-origin dist/af-web.css — no CDN, no off-origin fetch. (xterm's DOM renderer
// injects dynamic <style> elements for glyph dimensions and theme colors, which a
// bare `default-src 'self'` blocks; the served policy adds `style-src 'self'
// 'unsafe-inline'` to permit them while keeping every FETCH same-origin — see
// daemon/webserve.go.)

import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { decode, encode, inputFrame, Op, resizeFrame } from "./frame.js";

/** The attach terminal's connection state, surfaced for a small status line. */
export type TerminalStatus = "connecting" | "open" | "reconnecting" | "exited";

export interface TerminalCallbacks {
  /** Fired on every connection-state change, for the pane's status indicator. */
  onStatus(status: TerminalStatus): void;
}

const BACKOFF_BASE_MS = 500;
const BACKOFF_MAX_MS = 10_000;
// Debounce the fit→OpResize send so dragging a window edge sends one resize on
// settle, not one per animation frame. The server echoes the winning size back.
const RESIZE_DEBOUNCE_MS = 120;

/** xterm theme mirroring the app's GitHub-dark palette (styles.css) and the TUI's
 *  terminal aesthetic, so the browser terminal reads like the rest of the shell. */
const THEME = {
  background: "#0d1117",
  foreground: "#e6edf3",
  cursor: "#e6edf3",
  cursorAccent: "#0d1117",
  selectionBackground: "rgba(47, 129, 247, 0.30)",
  black: "#484f58",
  red: "#ff7b72",
  green: "#3fb950",
  yellow: "#d29922",
  blue: "#58a6ff",
  magenta: "#bc8cff",
  cyan: "#39c5cf",
  white: "#b1bac4",
  brightBlack: "#6e7681",
  brightRed: "#ffa198",
  brightGreen: "#56d364",
  brightYellow: "#e3b341",
  brightBlue: "#79c0ff",
  brightMagenta: "#d2a8ff",
  brightCyan: "#56d4dd",
  brightWhite: "#f0f6fc",
} as const;

/** ws: under http:, wss: under the TLS TCP listener the SPA is normally served
 *  from — matching the page origin so the stream rides the same authed transport. */
function wsScheme(): string {
  return window.location.protocol === "https:" ? "wss:" : "ws:";
}

/**
 * One live attach terminal bound to a container element. Construct it with the
 * bearer token and a session id, and it owns everything from there: the xterm
 * instance, the WS to `/v1/sessions/{id}/stream`, the fit/resize wiring, and a
 * self-healing reconnect loop. Call dispose() to tear it all down (on selection
 * change or logout). One instance per attached session — selecting a different
 * session disposes this and builds a fresh one, so scrollback never bleeds across
 * sessions.
 */
export class AttachTerminal {
  private readonly term: Terminal;
  private readonly fit: FitAddon;
  private readonly enc = new TextEncoder();
  private readonly ro: ResizeObserver;

  private ws: WebSocket | null = null;
  private stopped = false;
  private everOpened = false;
  private retry = 0;
  private reconnectTimer: number | null = null;
  private resizeTimer: number | null = null;

  // The absolute replay cursor: seeded from OpHello, advanced by OpPTYOut byte
  // counts (never by OpRepaint). `seeded` gates whether a reconnect can pass a
  // real ?since; the first connect omits it (live tail + a fresh-screen repaint).
  private cursor = 0n;
  private seeded = false;
  // The last size we told the server, so we don't re-send an unchanged fit and so
  // an authoritative echo that matches is a no-op.
  private lastRows = 0;
  private lastCols = 0;
  private exited = false;

  constructor(
    container: HTMLElement,
    private readonly sessionId: string,
    private readonly token: string,
    private readonly cb: TerminalCallbacks,
  ) {
    this.term = new Terminal({
      allowProposedApi: true,
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
      fontSize: 13,
      theme: THEME,
      // The stream is the source of truth; local echo/scrollback beyond the ring is
      // fine but the server never sees our convert-eol, so leave it raw.
      scrollback: 5000,
    });
    this.fit = new FitAddon();
    this.term.loadAddon(this.fit);
    this.term.open(container);
    this.fit.fit();

    // Keystrokes → OpInput. xterm hands us the terminal's outgoing byte string
    // (regular chars and key escape sequences alike); UTF-8 encode it so a typed
    // multibyte char reaches the PTY as the same bytes a real terminal would send.
    this.term.onData((data) => this.send(encode(inputFrame(this.enc.encode(data)))));

    // Re-fit + re-announce size whenever the container changes (window resize,
    // rail collapse, devtools). Debounced; the server echo reconciles multi-writer.
    this.ro = new ResizeObserver(() => this.scheduleFit());
    this.ro.observe(container);

    this.connect();
  }

  /** Permanently closes the terminal: stops the reconnect loop, drops the socket,
   *  disconnects the observer, and disposes xterm (freeing its DOM/renderer). */
  dispose(): void {
    this.stopped = true;
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.resizeTimer !== null) {
      window.clearTimeout(this.resizeTimer);
      this.resizeTimer = null;
    }
    this.ro.disconnect();
    this.closeSocket();
    this.term.dispose();
  }

  // --- socket lifecycle ------------------------------------------------------

  private connect(): void {
    if (this.stopped) {
      return;
    }
    this.cb.onStatus(this.everOpened ? "reconnecting" : "connecting");
    // Reconnects replay the gap from our cursor; the first connect omits ?since so
    // the broker starts at the live tail and sends a one-shot screen repaint.
    const base = `${wsScheme()}//${window.location.host}/v1/sessions/${encodeURIComponent(this.sessionId)}/stream`;
    const params = new URLSearchParams();
    params.set("access_token", this.token);
    if (this.seeded) {
      params.set("since", this.cursor.toString());
    }
    let ws: WebSocket;
    try {
      ws = new WebSocket(`${base}?${params.toString()}`);
    } catch {
      this.scheduleReconnect();
      return;
    }
    ws.binaryType = "arraybuffer";
    this.ws = ws;

    ws.onopen = () => {
      this.retry = 0;
      this.everOpened = true;
      this.exited = false;
      this.cb.onStatus("open");
      // Push our current size on (re)connect so a server that came up after us, or
      // a resize we did while disconnected, is reflected. The echo reconciles it.
      this.sendResize(this.term.rows, this.term.cols, true);
    };

    ws.onmessage = (e) => this.onMessage(e.data);
    ws.onclose = () => this.scheduleReconnect();
    ws.onerror = () => {
      // onerror precedes onclose; close so a half-open socket funnels through the
      // single reconnect path rather than hanging.
      try {
        ws.close();
      } catch {
        // already closing
      }
    };
  }

  private onMessage(data: unknown): void {
    if (typeof data === "string") {
      this.onControl(data);
      return;
    }
    if (!(data instanceof ArrayBuffer)) {
      return;
    }
    let frame;
    try {
      frame = decode(new Uint8Array(data));
    } catch {
      return; // a malformed frame is dropped, not fatal to the stream
    }
    switch (frame.op) {
      case Op.Hello:
        // Seed (or re-seed) the absolute cursor. On reconnect the broker returns
        // the actual replay start (clamped up if our ?since fell out of the ring),
        // so trusting the hello keeps ?since arithmetic correct across evictions.
        this.cursor = frame.seq;
        this.seeded = true;
        break;
      case Op.PTYOut:
        this.term.write(frame.data);
        this.cursor += BigInt(frame.data.length);
        break;
      case Op.Repaint:
        // Rendered like output but NOT counted toward the cursor — a repaint is a
        // per-subscriber screen snapshot, outside the ring's monotonic seq.
        this.term.write(frame.data);
        break;
      default:
        // OpInput/OpResize are client→server; never expected inbound. Ignore.
        break;
    }
  }

  private onControl(text: string): void {
    let msg: { type?: string; rows?: number; cols?: number; code?: number };
    try {
      msg = JSON.parse(text);
    } catch {
      return;
    }
    if (msg.type === "resize" && typeof msg.rows === "number" && typeof msg.cols === "number") {
      // Authoritative last-resize-wins echo: obey it verbatim, even if it differs
      // from our own fit (another client may have resized). This is a local resize
      // ONLY — it must not loop back out as an OpResize.
      this.applyEchoedSize(msg.rows, msg.cols);
    } else if (msg.type === "exit") {
      this.exited = true;
      const code = typeof msg.code === "number" ? msg.code : 0;
      this.term.write(`\r\n\x1b[38;5;244m[agent exited (code ${code})]\x1b[0m\r\n`);
      this.cb.onStatus("exited");
      this.stopped = true; // a dead PTY won't come back on this session; stop retrying
      this.closeSocket();
    }
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
      this.connect();
    }, delay);
  }

  private closeSocket(): void {
    const ws = this.ws;
    if (!ws) {
      return;
    }
    // Drop handlers first so closing doesn't schedule a reconnect.
    ws.onopen = null;
    ws.onmessage = null;
    ws.onclose = null;
    ws.onerror = null;
    try {
      ws.close();
    } catch {
      // already closing
    }
    this.ws = null;
  }

  // --- resize ----------------------------------------------------------------

  private scheduleFit(): void {
    if (this.resizeTimer !== null) {
      window.clearTimeout(this.resizeTimer);
    }
    this.resizeTimer = window.setTimeout(() => {
      this.resizeTimer = null;
      if (this.stopped) {
        return;
      }
      try {
        this.fit.fit(); // resizes xterm locally to the container; onResize fires
      } catch {
        return; // container detached mid-resize
      }
      this.sendResize(this.term.rows, this.term.cols, false);
    }, RESIZE_DEBOUNCE_MS);
  }

  /** Sends an OpResize for the given size unless it's unchanged. `force` re-sends
   *  even an unchanged size (used on connect to (re)assert our size to the server). */
  private sendResize(rows: number, cols: number, force: boolean): void {
    if (rows <= 0 || cols <= 0) {
      return;
    }
    if (!force && rows === this.lastRows && cols === this.lastCols) {
      return;
    }
    this.lastRows = rows;
    this.lastCols = cols;
    this.send(encode(resizeFrame(rows, cols)));
  }

  /** Applies the server's authoritative size to xterm without echoing it back out
   *  as an OpResize (which would ping-pong). Records it as our last-known size so a
   *  later identical local fit doesn't re-send. */
  private applyEchoedSize(rows: number, cols: number): void {
    this.lastRows = rows;
    this.lastCols = cols;
    if (rows !== this.term.rows || cols !== this.term.cols) {
      try {
        this.term.resize(cols, rows);
      } catch {
        // ignore an out-of-range size from a racing peer
      }
    }
  }

  private send(bytes: Uint8Array): void {
    const ws = this.ws;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(bytes);
    }
  }
}
