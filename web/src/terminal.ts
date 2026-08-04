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
import { type IMarker, type ITheme, Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { handleClipboardKeydown } from "./clipboard.js";
import { decode, encode, inputFrame, Op, resizeFrame } from "./frame.js";
import {
  hasVisibleTerminalGeometry,
  shouldRefitVisibleTerminal,
  shouldRestoreViewport,
  terminalUserScrollPlan,
  type TerminalUserScrollSource,
  viewportAnchorLine,
  viewportMarkerOffset,
} from "./terminal-geometry.js";
import {
  historyWheelPlan,
  invertTerminalMouseOverride,
  terminalMouseOverride,
  terminalMouseOverrideHeld,
  type TerminalMouseOverride,
  touchHistoryScrollPlan,
} from "./terminal-mouse.js";
import type { StreamEndpoint } from "./stream_endpoint.js";
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

interface PendingViewportAnchor {
  /** A marker follows the actual line while inactive output shifts the buffer. */
  marker: IMarker | null;
  /** Absolute fallback for a buffer where xterm cannot register a marker. */
  line: number;
  /** A reader already at the bottom should follow new output to the new bottom. */
  atBottom: boolean;
}


/**
 * One live attach terminal bound to a container element. Construct it with the
 * bearer token and a StreamEndpoint (which daemon PTY to render), and it owns
 * everything from there: the xterm instance, the WS to the endpoint's URL, the
 * fit/resize wiring, and a self-healing reconnect loop. Call dispose() to tear it
 * all down (on selection/tab change, logout, or closing the config-assistant pane).
 * One instance per attached endpoint — selecting a different session OR switching
 * tabs disposes this and builds a fresh one, so scrollback and the replay cursor
 * (which are per-tab on the broker) never bleed across tabs (#1592 Phase 5 PR7).
 */
export class AttachTerminal {
  private readonly term: Terminal;
  private readonly fit: FitAddon;
  private readonly enc = new TextEncoder();
  private readonly ro: ResizeObserver;
  private readonly io: IntersectionObserver;

  private ws: WebSocket | null = null;
  private stopped = false;
  private everOpened = false;
  private retry = 0;
  private initialConnectStarted = false;
  private reconnectTimer: number | null = null;
  private resizeTimer: number | null = null;
  private visibleFitFrame: number | null = null;
  private viewportRestoreFrame: number | null = null;
  private readonly mouseOverride: TerminalMouseOverride;
  private readonly mouseCaptureHint: HTMLDivElement;
  private readonly pointerHint: string;
  private readonly wheelHint: string;
  private mouseCaptureHintTimer: number | null = null;
  private mouseOverrideKeyHeld = false;
  private handedOffDrag = false;
  private historyWheelRemainder = 0;
  // The screen position the current one-finger drag last scrolled from, plus its
  // sub-row carry. Null whenever no gesture is af's to scroll: none is down, or a
  // second finger arrived and the browser owns the pinch.
  private touchScrollY: number | null = null;
  private touchScrollRemainder = 0;
  // Whether the gesture in flight came from a finger, read off the pointer event that
  // precedes the browser's compatibility mouse events (onPointerDown).
  private lastPointerWasTouch = false;
  // Incremented only by an actual user scroll input while a peer-owned anchor is
  // pending. Output and resize reflows can move viewportY too, so position deltas
  // alone do not prove that a user intended to override the saved reading line.
  private userScrollRevision = 0;

  // The absolute replay cursor: seeded from OpHello, advanced by OpPTYOut byte
  // counts (never by OpRepaint). `seeded` gates whether a reconnect can pass a
  // real ?since; the first connect omits it (live tail + a fresh-screen repaint).
  private cursor = 0n;
  private seeded = false;
  // The last size we told the server, so we don't re-send an unchanged fit and so
  // an authoritative echo that matches is a no-op.
  private lastRows = 0;
  private lastCols = 0;
  // A peer resize can temporarily collapse this client's scrollback (for example,
  // 60 lines fit in a peer's 111-row grid). Anchor the actual visible buffer line,
  // not a one-time distance from the bottom: output can keep arriving while this
  // client is inactive. Null means no peer-owned grid is pending reconciliation.
  private pendingViewport: PendingViewportAnchor | null = null;
  private exited = false;

  // A peer is allowed to resize the one shared PTY and every client obeys that
  // authoritative echo. When this window becomes active again, its visible host
  // becomes the local writer: refit once instead of leaving a peer-sized emulator in
  // an unchanged container until the user physically resizes the window (#2347).
  private readonly onWindowFocus = (): void => this.scheduleVisibleFit();
  private readonly onWindowBlur = (): void => {
    // A modifier released in another tab/window never sends this terminal keyup.
    this.mouseOverrideKeyHeld = false;
    // Nor does a button released there send this window a mouseup, which would
    // otherwise strand the drag's document listeners until the next release.
    this.endHandedOffDrag();
  };
  private readonly onVisibilityChange = (): void => {
    if (document.visibilityState === "visible") {
      this.scheduleVisibleFit();
    } else {
      this.mouseOverrideKeyHeld = false;
    }
  };
  // Entering a pane is the earliest reliable activation signal for side-by-side
  // clients: reconcile before the wheel gesture arrives so xterm can consume that
  // first gesture rather than making the user scroll twice.
  private readonly onPointerEnter = (): void => this.fitVisibleHost();
  // A scroll can target an already-focused window after another client resized the
  // PTY without the pointer ever leaving this pane. Capture repairs that less-common
  // path; pointer entry above handles the ordinary first gesture. The pending-peer
  // gate makes every ordinary input a no-op before even measuring layout.
  private readonly onWheel = (event: WheelEvent): void => {
    this.handleUserScroll("wheel");
    if (
      !terminalMouseOverrideHeld(event, this.mouseOverride) &&
      !this.mouseOverrideKeyHeld &&
      this.applicationOwnsWheel()
    ) {
      this.showMouseCaptureHint(this.wheelHint);
    }
  };
  // Application mouse mode switches xterm's OWN touch scrolling off — both of its
  // touch listeners return early while mouse events are active — and nothing takes
  // over: the finger is on the screen, and the .xterm-viewport holding the scrollback
  // is that screen's SIBLING, so the browser has no ancestor to pan. A phone
  // therefore loses scrollback entirely the moment an agent enables mouse tracking,
  // and unlike the wheel (#2681) it has no modifier to escape with. So af scrolls
  // history itself here (#2682): the DRAG is terminal-owned, the TAP still reaches
  // the application — which is why only the move is ever cancelled, never the
  // touchstart that a tap's compatibility mouse events depend on.
  private readonly onTouchStart = (event: TouchEvent): void => {
    // Ownership is settled once, at the start of the gesture. A touch whose target is
    // the viewport itself is a scrollbar drag the browser already handles — the same
    // discriminator onPointerDown reads below, and taking it over would scroll
    // BACKWARDS, since a thumb follows the finger while the content opposes it.
    const onScrollbar = event.target === this.container.querySelector(".xterm-viewport");
    this.touchScrollY = event.touches.length === 1 && !onScrollbar ? event.touches[0].clientY : null;
    this.touchScrollRemainder = 0;
  };
  private readonly onTouchMove = (event: TouchEvent): void => {
    this.handleUserScroll("touch");
    if (this.touchScrollY === null) {
      return;
    }
    if (event.touches.length !== 1) {
      // A second finger arrived: hand the REST of this gesture to the browser rather
      // than scrolling by a pinch's motion or by the distance it covered untracked.
      this.touchScrollY = null;
      return;
    }
    const last = this.touchScrollY;
    const y = event.touches[0].clientY;
    this.touchScrollY = y;
    if (!this.applicationOwnsMouse()) {
      // xterm's own touch scrolling is live here; tracking the position anyway keeps
      // a mode change mid-drag from scrolling by everything travelled before it.
      return;
    }
    const plan = touchHistoryScrollPlan(last, y, this.term.rows, this.rowHeight(), this.touchScrollRemainder);
    this.touchScrollRemainder = plan.remainder;
    if (plan.lines !== 0) {
      this.term.scrollLines(plan.lines);
    }
    // Claim the pan. Left to the browser it chains out to the document, which toggles
    // the URL bar, resizes .af-app and refits the terminal mid-gesture (#2493) — and
    // an uncancelled drag can still synthesize the compatibility mouse events the
    // application would read as a drag it never got the button press for.
    event.preventDefault();
  };
  private readonly onPointerDown = (event: PointerEvent): void => {
    // A pointer event always precedes its compatibility mouse counterpart — for a tap
    // the browser fires pointerdown at touch-down and mousedown only after the finger
    // lifts — so this is what tells onMouseDownCapture below whether the click it is
    // about to rewrite came from a finger.
    this.lastPointerWasTouch = event.pointerType === "touch";
    // The xterm screen is a sibling of its scrollable viewport. A pointer whose
    // target is the viewport itself is therefore a scrollbar/track gesture, while
    // an ordinary terminal click targets the screen and keeps the saved anchor.
    const viewport = this.container.querySelector(".xterm-viewport");
    if (event.target === viewport) {
      this.handleUserScroll("scrollbar");
      return;
    }
    // Both halves of the hint are desktop advice a finger cannot take: the drag it
    // describes scrolls history here rather than selecting (#2682), and there is no
    // modifier to hold for the app clicks it offers — a tap gives those directly.
    if (event.pointerType === "touch") {
      return;
    }
    if (!terminalMouseOverrideHeld(event, this.mouseOverride) && this.applicationOwnsMouse()) {
      this.showMouseCaptureHint(this.pointerHint);
    }
  };
  // The inversion itself (#2787). It runs in the CAPTURE phase on the pane host, so
  // it lands before both of xterm's mousedown listeners — they sit on xterm's own
  // element, a descendant — and therefore before either reads the modifier.
  //
  // mousedown ONLY. mouseup carries xterm's alt-click-moves-cursor gesture, which
  // reads the same altKey: a synthetic Option there would fire cursor-movement
  // sequences into the PTY on every plain click. It is also unnecessary — xterm only
  // registers its PTY mouseup/mousedrag forwarders inside the mousedown branch this
  // inversion already diverts, and the selection drag that replaces it is driven by
  // document listeners that read no modifier at all.
  //
  // Mouse pointers ONLY. The inversion trades a plain click for a selection and hands
  // the click back behind a modifier — a trade a touch device cannot take, because it
  // has no modifier to hold, so inverting a tap would leave a phone with NO way to
  // click a mouse-driven TUI at all. Touch does not need the trade either: its two
  // gestures already separate without one, the drag scrolling history (#2682) and the
  // tap staying the click.
  private readonly onMouseDownCapture = (event: MouseEvent): void => {
    if (!this.applicationOwnsMouse() || this.lastPointerWasTouch) {
      // Outside application mouse mode a plain drag already selects, and the
      // modifier keeps its xterm meaning (Shift extends an existing selection). A
      // touch has no modifier to invert or to hand a drag off with, per the note
      // above, so it leaves here too.
      return;
    }
    // Held → this drag is being handed to the application, and the REST of it has
    // to stay consistent with the press it is about to receive.
    const handedOff = terminalMouseOverrideHeld(event, this.mouseOverride);
    invertTerminalMouseOverride(event, this.mouseOverride);
    if (handedOff) {
      this.beginHandedOffDrag();
    }
  };
  // The rest of a handed-off drag. xterm forwards move/release from DOCUMENT-level
  // listeners and encodes each event's OWN modifiers into the report, so stripping
  // only the mousedown would hand a mouse-aware TUI an incoherent sequence: an
  // unmodified press followed by a Shift/Alt-flagged drag and release. The modifier
  // is af's escape hatch, not input the user aimed at the application, so it must
  // not arrive as a modified-click binding.
  //
  // STRIPS only, and only while a handed-off drag is in flight. With the modifier
  // NOT held the drag is a selection, and mouseup must keep its true altKey there:
  // xterm's alt-click-moves-cursor reads exactly that flag and would otherwise fire
  // cursor-movement sequences into the PTY on every plain click.
  private readonly onHandedOffDragModifier = (event: MouseEvent): void => {
    if (terminalMouseOverrideHeld(event, this.mouseOverride)) {
      invertTerminalMouseOverride(event, this.mouseOverride);
    }
    if (event.type === "mouseup") {
      this.endHandedOffDrag();
    }
  };

  private handleUserScroll(source: TerminalUserScrollSource): void {
    if (this.pendingViewport === null) {
      return;
    }
    const plan = terminalUserScrollPlan(source, this.visibleFitFrame !== null);
    if (plan.cancelScheduledVisibleFit) {
      this.cancelVisibleFitFrame();
    }
    this.fitVisibleHost();
    // fitVisibleHost schedules from the pre-input revision. xterm consumes the
    // user gesture after this capture listener, so the deferred callback sees a
    // newer explicit intent and leaves that first gesture authoritative.
    this.userScrollRevision += 1;
  }

  constructor(
    private readonly container: HTMLElement,
    private readonly token: string,
    private readonly endpoint: StreamEndpoint,
    private readonly cb: TerminalCallbacks,
  ) {
    this.mouseOverride = terminalMouseOverride(navigator.platform);
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
      // Names Option as macOS's force-selection modifier (Shift is xterm's built-in
      // choice elsewhere), giving onMouseDownCapture ONE bit to invert on every
      // platform. It also keeps Option out of xterm's block-select gesture, which
      // would otherwise fire on the inverted bit.
      macOptionClickForcesSelection: true,
    });
    this.fit = new FitAddon();
    this.term.loadAddon(this.fit);
    this.term.open(container);
    this.mouseCaptureHint = document.createElement("div");
    this.mouseCaptureHint.className = "af-mouse-capture-hint";
    this.mouseCaptureHint.setAttribute("role", "status");
    this.mouseCaptureHint.setAttribute("aria-live", "polite");
    this.mouseCaptureHint.setAttribute("aria-hidden", "true");
    // Two messages, because the pointer and the wheel now sit on opposite sides of
    // the modifier: a drag is terminal-owned by default (#2787) while the wheel stays
    // application-owned (#2681). One combined line would have to describe both
    // directions at once, and the hint exists precisely to say which gesture the user
    // just lost.
    this.pointerHint = `App mouse active · Drag selects · Hold ${this.mouseOverride} for app clicks`;
    this.wheelHint = `App mouse active · Hold ${this.mouseOverride} to scroll history`;
    this.term.element?.append(this.mouseCaptureHint);

    // xterm normally forwards every wheel to a mouse-aware application and cancels
    // browser scrollback. The modifier instead scrolls terminal history (#2681). The
    // wheel is deliberately NOT inverted alongside the drag: an agent TUI that turns
    // mouse mode on is usually doing it to own the wheel, and taking scroll away from
    // it by default would break the pane it draws.
    this.term.attachCustomWheelEventHandler((event) => this.handleHistoryWheel(event));

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
        this.mouseOverrideKeyHeld = false;
        if (!this.stopped) {
          this.cb.onFocusChange(false);
        }
      });
    }

    // Keystrokes → OpInput. xterm hands us the terminal's outgoing byte string
    // (regular chars and key escape sequences alike); UTF-8 encode it so a typed
    // multibyte char reaches the PTY as the same bytes a real terminal would send.
    this.term.onData((data) => this.sendInput(data));

    // Modified input + clipboard decisions (see clipboard.ts): intercept the key
    // BEFORE xterm turns it into input. Bare Shift+Enter emits LF only for the
    // invariant agent tab at index 0; shell/process tabs and plain Enter retain
    // xterm's CR. Ctrl+C copies a present selection (else interrupts),
    // Ctrl+Shift+C is explicit copy, Cmd+C copies a present selection (macOS, and
    // never an interrupt — #2787), and Ctrl+V defers to native paste. False
    // suppresses xterm's own handling.
    this.term.attachCustomKeyEventHandler((ev) => {
      const overrideKey = this.mouseOverride === "Option" ? "Alt" : "Shift";
      if (ev.key === overrideKey) {
        this.mouseOverrideKeyHeld = ev.type !== "keyup";
      }
      return handleClipboardKeydown(ev, {
        composerNewline: this.endpoint.composerNewline,
        hasSelection: () => this.term.hasSelection(),
        getSelection: () => this.term.getSelection(),
        clearSelection: () => this.term.clearSelection(),
        copy: (text) => this.copyToClipboard(text),
        sendInput: (text) => this.sendInput(text),
        // Public Terminal.input(..., true) is xterm's genuine-user-input path:
        // it scrolls to bottom and clears selection, then fires onData above.
        sendUserInput: (text) => this.term.input(text, true),
      });
    });

    // Re-fit + re-announce size whenever the container changes (window resize,
    // rail collapse, devtools). Debounced; the server echo reconciles multi-writer.
    this.ro = new ResizeObserver(() => this.scheduleFit());
    this.ro.observe(container);

    // ResizeObserver alone cannot express two important transitions:
    //   * xterm is opened before its first painted cell metrics exist, even when the
    //     host already has its final border-box; and
    //   * a peer's authoritative resize changes xterm's grid, not this host's box.
    // Intersection/activation schedule a one-frame-later fit at those boundaries.
    // A zero-size hidden pane is rejected by fitVisibleHost and retried when it
    // intersects; there is no polling timer.
    this.io = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.target === container && entry.isIntersecting)) {
        this.scheduleVisibleFit();
      }
    });
    this.io.observe(container);
    window.addEventListener("focus", this.onWindowFocus);
    window.addEventListener("blur", this.onWindowBlur);
    document.addEventListener("visibilitychange", this.onVisibilityChange);
    container.addEventListener("pointerenter", this.onPointerEnter);
    container.addEventListener("wheel", this.onWheel, { capture: true, passive: true });
    container.addEventListener("touchstart", this.onTouchStart, { capture: true, passive: true });
    // Capture, so the drag is resolved before xterm's own touch listeners (which sit
    // on a descendant) see it, and NON-passive, because claiming the pan means
    // calling preventDefault — a passive listener's is ignored outright.
    container.addEventListener("touchmove", this.onTouchMove, { capture: true, passive: false });
    container.addEventListener("pointerdown", this.onPointerDown, true);
    container.addEventListener("mousedown", this.onMouseDownCapture, true);
    this.scheduleVisibleFit();
  }

  /** Permanently closes the terminal: stops the reconnect loop, drops the socket,
   *  disconnects the observer, and disposes xterm (freeing its DOM/renderer). */
  dispose(): void {
    this.stopped = true;
    if (this.mouseCaptureHintTimer !== null) {
      window.clearTimeout(this.mouseCaptureHintTimer);
      this.mouseCaptureHintTimer = null;
    }
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.resizeTimer !== null) {
      window.clearTimeout(this.resizeTimer);
      this.resizeTimer = null;
    }
    this.cancelVisibleFitFrame();
    this.clearPendingViewport();
    this.ro.disconnect();
    this.io.disconnect();
    window.removeEventListener("focus", this.onWindowFocus);
    window.removeEventListener("blur", this.onWindowBlur);
    document.removeEventListener("visibilitychange", this.onVisibilityChange);
    this.container.removeEventListener("pointerenter", this.onPointerEnter);
    this.container.removeEventListener("wheel", this.onWheel, true);
    this.container.removeEventListener("touchstart", this.onTouchStart, true);
    this.container.removeEventListener("touchmove", this.onTouchMove, true);
    this.container.removeEventListener("pointerdown", this.onPointerDown, true);
    this.container.removeEventListener("mousedown", this.onMouseDownCapture, true);
    this.endHandedOffDrag();
    this.closeSocket();
    this.term.dispose();
  }

  private applicationOwnsMouse(): boolean {
    return this.term.modes.mouseTrackingMode !== "none";
  }

  private applicationOwnsWheel(): boolean {
    const mode = this.term.modes.mouseTrackingMode;
    // DECSET 9/X10 reports button-down only. xterm keeps wheel events for
    // scrollback there; VT200, drag, and any-event modes report wheel input.
    return mode !== "none" && mode !== "x10";
  }

  /** Tracks the modifier strip on the document for exactly the life of one
   *  handed-off drag — xterm registers its own forwarders the same way, and events
   *  can leave this pane mid-drag, so the pane host is not a wide enough net. */
  private beginHandedOffDrag(): void {
    if (this.handedOffDrag) {
      return;
    }
    this.handedOffDrag = true;
    document.addEventListener("mousemove", this.onHandedOffDragModifier, true);
    document.addEventListener("mouseup", this.onHandedOffDragModifier, true);
  }

  private endHandedOffDrag(): void {
    if (!this.handedOffDrag) {
      return;
    }
    this.handedOffDrag = false;
    document.removeEventListener("mousemove", this.onHandedOffDragModifier, true);
    document.removeEventListener("mouseup", this.onHandedOffDragModifier, true);
  }

  private showMouseCaptureHint(message: string): void {
    this.mouseCaptureHint.textContent = message;
    this.mouseCaptureHint.classList.add("af-visible");
    this.mouseCaptureHint.setAttribute("aria-hidden", "false");
    if (this.mouseCaptureHintTimer !== null) {
      window.clearTimeout(this.mouseCaptureHintTimer);
    }
    this.mouseCaptureHintTimer = window.setTimeout(() => {
      this.mouseCaptureHint.classList.remove("af-visible");
      this.mouseCaptureHint.setAttribute("aria-hidden", "true");
      this.mouseCaptureHintTimer = null;
    }, 4_000);
  }

  /** The painted height of one terminal row, which turns a pixel-denominated gesture
   *  into rows. Falls back to a font-size estimate before the first row exists. */
  private rowHeight(): number {
    const renderedRow = this.container.querySelector<HTMLElement>(".xterm-rows > div");
    return renderedRow?.getBoundingClientRect().height ?? (this.term.options.fontSize ?? 13) * 1.2;
  }

  private handleHistoryWheel(event: WheelEvent): boolean {
    // Chromium's wheel event can omit modifier flags even while the keyboard key
    // remains down (notably through automation and trackpad momentum). xterm sees
    // the matching keydown/up, so retain that state as the authoritative fallback.
    if (
      !this.applicationOwnsWheel() ||
      (!terminalMouseOverrideHeld(event, this.mouseOverride) && !this.mouseOverrideKeyHeld)
    ) {
      return true;
    }

    const plan = historyWheelPlan(event, this.term.rows, this.rowHeight(), this.historyWheelRemainder);
    this.historyWheelRemainder = plan.remainder;
    if (plan.lines !== 0) {
      this.term.scrollLines(plan.lines);
    }
    event.preventDefault();
    return false;
  }

  /** Gives the terminal the keyboard (focuses xterm's helper textarea) so typed
   *  keys reach the agent — the attach half of the #1693 nav/attach model. */
  focus(): void {
    this.fitVisibleHost();
    this.term.focus();
  }

  /** Reconciles this terminal after an app-shell layout/visibility transition.
   *  Coalesced onto the next painted frame so CSS has settled; the measurable-host
   *  gate in fitVisibleHost keeps hidden or zero-sized panes inert. */
  refit(): void {
    this.scheduleVisibleFit();
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
    // the broker starts at the live tail and sends a one-shot screen repaint. The
    // endpoint owns the URL shape (session tab vs the bare config assistant); the
    // reconnect/replay/resize machinery below is identical for either.
    const url = this.endpoint.url(this.token, this.seeded ? this.cursor : null);
    let ws: WebSocket;
    try {
      ws = new WebSocket(url);
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

  /** Starts the first connection only after a real visible-host fit. Reconnects
   * continue through connect() directly once this boundary has been crossed. */
  private connectAfterInitialFit(): void {
    if (this.initialConnectStarted || this.stopped) {
      return;
    }
    this.initialConnectStarted = true;
    this.connect();
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

  /** Fits on the next painted frame, after visibility/layout changes have settled.
   *  Repeated activation signals coalesce into one frame; unlike the resize debounce,
   *  this is not a time guess and never repeats on its own. */
  private scheduleVisibleFit(): void {
    if (this.stopped || this.visibleFitFrame !== null) {
      return;
    }
    this.visibleFitFrame = window.requestAnimationFrame(() => {
      this.visibleFitFrame = null;
      this.fitVisibleHost();
    });
  }

  /** Drops activation work queued before a direct user scroll. Leaving that frame
   * alive lets it run after the input fit and rebase the restore onto the new user
   * revision, which makes the first gesture appear inert. */
  private cancelVisibleFitFrame(): void {
    if (this.visibleFitFrame === null) {
      return;
    }
    window.cancelAnimationFrame(this.visibleFitFrame);
    this.visibleFitFrame = null;
  }

  /** Reconciles xterm's grid with this host only when the host is measurable and the
   *  FitAddon proposes a real change. A peer-owned MsgResize remains authoritative
   *  while this client is inactive; activation/wheel makes this client the newest
   *  writer once, without a resize-echo ping-pong between visible clients. */
  private fitVisibleHost(): void {
    if (this.stopped) {
      return;
    }
    let proposed;
    try {
      proposed = this.fit.proposeDimensions();
    } catch {
      return; // xterm has not painted metrics yet, or the container detached
    }
    const host = { width: this.container.clientWidth, height: this.container.clientHeight };
    if (!hasVisibleTerminalGeometry(host, proposed)) {
      return;
    }
    const needsFit = shouldRefitVisibleTerminal(host, { rows: this.term.rows, cols: this.term.cols }, proposed);
    if (needsFit) {
      try {
        this.fit.fit();
      } catch {
        return; // container detached between proposal and fit
      }
      this.sendResize(this.term.rows, this.term.cols, false);
    }
    // A second peer can already have returned xterm to this host's local grid. No
    // fit is needed in that case, but its reflow still displaced the saved line.
    this.scheduleViewportRestore(this.term.rows, this.term.cols);
    this.connectAfterInitialFit();
  }

  /** Restores the pre-peer reading position after xterm has rendered its new grid.
   *  resize() schedules the buffer reflow: reading baseY synchronously after fit can
   *  still see the peer-collapsed zero. The next painted frame is the first settled
   *  value. A newer peer grid cancels this frame and leaves the anchor pending for
   *  the next local activation. */
  private scheduleViewportRestore(rows: number, cols: number): void {
    if (this.pendingViewport === null) {
      return;
    }
    this.cancelViewportRestoreFrame();
    const scheduledUserScroll = this.userScrollRevision;
    this.viewportRestoreFrame = window.requestAnimationFrame(() => {
      this.viewportRestoreFrame = null;
      if (this.stopped || this.term.rows !== rows || this.term.cols !== cols) {
        return;
      }
      const anchor = this.pendingViewport;
      if (anchor === null) {
        return;
      }
      const buffer = this.term.buffer.active;
      if (
        !shouldRestoreViewport({
          scheduledUserScroll,
          currentUserScroll: this.userScrollRevision,
        })
      ) {
        this.clearPendingViewport();
        return;
      }
      const target = viewportAnchorLine(
        {
          atBottom: anchor.atBottom,
          markerLine: anchor.marker?.line ?? null,
          fallbackLine: anchor.line,
        },
        buffer.baseY,
      );
      this.clearPendingViewport();
      this.term.scrollToLine(Math.max(0, target));
    });
  }

  private cancelViewportRestoreFrame(): void {
    if (this.viewportRestoreFrame !== null) {
      window.cancelAnimationFrame(this.viewportRestoreFrame);
      this.viewportRestoreFrame = null;
    }
  }

  private clearPendingViewport(): void {
    this.cancelViewportRestoreFrame();
    this.pendingViewport?.marker?.dispose();
    this.pendingViewport = null;
  }

  private scheduleFit(): void {
    if (this.resizeTimer !== null) {
      window.clearTimeout(this.resizeTimer);
    }
    this.resizeTimer = window.setTimeout(() => {
      this.resizeTimer = null;
      if (this.stopped) {
        return;
      }
      this.fitVisibleHost();
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
      // Capture only the FIRST peer-owned resize in a run. Further peer echoes may
      // already have collapsed/reflowed the buffer, but the first boundary still
      // knows where this client was reading in its own grid. The active-host fit
      // consumes and clears this value before its own echo comes back.
      if (this.pendingViewport === null) {
        const buffer = this.term.buffer.active;
        const atBottom = buffer.viewportY >= buffer.baseY;
        let marker: IMarker | null = null;
        if (!atBottom) {
          try {
            marker = this.term.registerMarker(viewportMarkerOffset(buffer));
          } catch {
            // Alternate buffers cannot register markers; the absolute line below
            // is still safer than a bottom distance that grows stale with output.
          }
        }
        this.pendingViewport = { marker, line: buffer.viewportY, atBottom };
      }
      this.cancelViewportRestoreFrame();
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

  // --- input & clipboard -----------------------------------------------------

  /** Sends text to the PTY as OpInput — the single input path shared by typed
   *  keys (onData) and the Ctrl+C interrupt (clipboard.ts). UTF-8 encoded so a
   *  multibyte char reaches the PTY as the same bytes a real terminal would send. */
  private sendInput(text: string): void {
    this.send(encode(inputFrame(this.enc.encode(text))));
  }

  /** Copies text to the system clipboard, never silently. localhost is a secure
   *  context, so navigator.clipboard.writeText works; but if it is missing (a
   *  non-secure origin behind a proxy) or rejects, fall back to the legacy
   *  execCommand path, and only if THAT fails surface a visible hint — a copy that
   *  silently fails is worse than none (the user pastes stale content unaware). */
  private copyToClipboard(text: string): void {
    if (text === "") {
      return; // nothing selected — an explicit copy of nothing is a no-op
    }
    const clip = navigator.clipboard;
    if (clip && typeof clip.writeText === "function") {
      // The .catch fallback runs after the async rejection, i.e. outside the key
      // gesture, so execCommand may itself fail there; the hint is the backstop.
      clip.writeText(text).catch(() => {
        if (!this.execCommandCopy(text)) {
          this.flashCopyHint();
        }
      });
      return;
    }
    if (!this.execCommandCopy(text)) {
      this.flashCopyHint();
    }
  }

  /** Legacy clipboard write via a throwaway off-screen textarea. Returns whether the
   *  copy reported success. Requires a user gesture, which the key handler provides. */
  private execCommandCopy(text: string): boolean {
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      // Off-screen but still selectable; readonly stops a mobile keyboard popping up.
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.top = "0";
      ta.style.left = "0";
      ta.style.width = "1px";
      ta.style.height = "1px";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      ta.setSelectionRange(0, text.length);
      const ok = document.execCommand("copy");
      ta.remove();
      this.term.focus(); // the temp textarea stole focus; hand it back to the terminal
      return ok;
    } catch {
      return false;
    }
  }

  /** Last-resort visible cue when BOTH clipboard paths fail, so the copy is never
   *  silently dropped. An app-level "clipboard unavailable" condition (not
   *  pane-specific), so it is a viewport-fixed toast appended to document.body —
   *  matching the app's own af-toast pattern and, by living outside the pane tree,
   *  never clipped by a split pane's overflow:hidden or anchored to a transformed
   *  ancestor. Styled inline so it needs no stylesheet plumbing and no <style>
   *  element under the CSP. */
  private flashCopyHint(): void {
    try {
      const hint = document.createElement("div");
      hint.textContent = "Copy failed — clipboard unavailable";
      hint.setAttribute("role", "alert");
      hint.style.position = "fixed";
      hint.style.bottom = "12px";
      hint.style.right = "12px";
      hint.style.zIndex = "9999";
      hint.style.padding = "4px 10px";
      hint.style.borderRadius = "4px";
      hint.style.font = "12px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace";
      hint.style.background = "rgba(0, 0, 0, 0.82)";
      hint.style.color = "#fff";
      hint.style.pointerEvents = "none";
      document.body.appendChild(hint);
      window.setTimeout(() => hint.remove(), 2500);
    } catch {
      // If even the DOM cue fails there is nothing further to do.
    }
  }
}
