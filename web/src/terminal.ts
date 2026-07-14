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
import { type ITheme, Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { decode, encode, inputFrame, Op, resizeFrame } from "./frame.js";
import { currentXtermTheme } from "./theme.js";

/** The attach terminal's connection state, surfaced for a small status line. */
export type TerminalStatus = "connecting" | "open" | "reconnecting" | "exited";

export interface TerminalCallbacks {
  /** Fired on every connection-state change, for the pane's status indicator. */
  onStatus(status: TerminalStatus): void;
  /** Fired when the terminal gains/loses the keyboard (xterm's helper textarea
   *  focus/blur), so index.ts can keep its nav-vs-terminal mode (#1693) in sync
   *  with real DOM focus — e.g. a mouse click straight into the terminal. */
  onFocusChange(focused: boolean): void;
}

const BACKOFF_BASE_MS = 500;
const BACKOFF_MAX_MS = 10_000;
// Debounce the fit→OpResize send so dragging a window edge sends one resize on
// settle, not one per animation frame. The server echoes the winning size back.
const RESIZE_DEBOUNCE_MS = 120;

/** ws: matching the page origin — the daemon serves the SPA over plain HTTP, so
 *  this is normally ws:. If a reverse proxy fronts the daemon and serves the page
 *  over https:, it becomes wss: so the stream rides the same proxied transport. */
function wsScheme(): string {
  return window.location.protocol === "https:" ? "wss:" : "ws:";
}

/**
 * One live attach terminal bound to a container element. Construct it with the
 * bearer token, a session id, and a tab index, and it owns everything from there:
 * the xterm instance, the WS to `/v1/sessions/{id}/stream?tab=<idx>`, the
 * fit/resize wiring, and a self-healing reconnect loop. Call dispose() to tear it
 * all down (on selection/tab change or logout). One instance per attached
 * (session, tab) — selecting a different session OR switching tabs disposes this
 * and builds a fresh one, so scrollback and the replay cursor (which are per-tab
 * on the broker) never bleed across tabs (#1592 Phase 5 PR7).
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
    private readonly tab: number,
    private readonly cb: TerminalCallbacks,
  ) {
    this.term = new Terminal({
      allowProposedApi: true,
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
      fontSize: 13,
      // Born in the active theme (theme.ts derives the xterm palette from the same
      // tokens as the CSS chrome); setTheme() re-applies live on a toggle.
      theme: currentXtermTheme(),
      // The stream is the source of truth; local echo/scrollback beyond the ring is
      // fine but the server never sees our convert-eol, so leave it raw.
      scrollback: 5000,
    });
    this.fit = new FitAddon();
    this.term.loadAddon(this.fit);
    this.term.open(container);
    this.fit.fit();

    // Report focus/blur of xterm's helper textarea so index.ts can track which pane
    // owns the keyboard (#1693) even when the user clicks straight into the terminal
    // rather than pressing Enter. Guarded on `stopped` so the blur that dispose()
    // triggers (tearing the textarea down) doesn't fire a spurious mode change.
    const textarea = this.term.textarea;
    if (textarea) {
      textarea.addEventListener("focus", () => {
        if (!this.stopped) {
          this.cb.onFocusChange(true);
        }
      });
      textarea.addEventListener("blur", () => {
        if (!this.stopped) {
          this.cb.onFocusChange(false);
        }
      });
    }

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

  /** Gives the terminal the keyboard (focuses xterm's helper textarea) so typed
   *  keys reach the agent — the attach half of the #1693 nav/attach model. */
  focus(): void {
    this.term.focus();
  }

  /** Takes the keyboard away from the terminal (blurs it) so document-level rail
   *  navigation gets the keys again — the Escape/back-to-nav half of #1693. */
  blur(): void {
    this.term.blur();
  }

  /** Re-applies an xterm palette live (a theme toggle): xterm repaints the canvas
   *  from the new ITheme, so an open terminal switches light/dark without a
   *  reconnect or losing scrollback. */
  setTheme(theme: ITheme): void {
    this.term.options.theme = theme;
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
    // Select the bound tab (parseTab in daemon/ws_pty.go: empty/absent = 0 = the
    // agent tab). Sent only for a non-agent tab so the agent-tab URL is unchanged.
    if (this.tab > 0) {
      params.set("tab", String(this.tab));
    }
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
