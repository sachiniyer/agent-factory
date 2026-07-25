// Stream endpoints (#2467): WHICH daemon PTY an AttachTerminal renders, decoupled
// from HOW it renders it. Every stream speaks the same broker protocol
// (servePTYStream: OpHello/OpPTYOut/resize/exit, ?since replay) — only the URL and
// the tab-selection params differ. Keeping the address here (a pure module, no xterm)
// lets the terminal own the protocol, lets split.ts / config_assistant.ts pick the
// address, and makes the URL shapes unit-testable without importing xterm's CSS.

/** ws: matching the page origin — the daemon serves the SPA over plain HTTP, so this
 *  is normally ws:. A reverse proxy serving the page over https: makes it wss: so the
 *  stream rides the same proxied transport. */
export function wsScheme(): string {
  return window.location.protocol === "https:" ? "wss:" : "ws:";
}

/**
 * A stream endpoint builds the WS URL for a (re)connect and declares whether the
 * stream is an agent composer. The terminal owns the reconnect/replay/resize
 * machinery; the endpoint owns only the address, which is what lets one terminal
 * client render a session PTY and the bare-session config assistant with no fork.
 */
export interface StreamEndpoint {
  /** Builds the WS URL. `since` is null on the first connect (live tail + a
   *  fresh-screen repaint) and the absolute replay cursor on reconnects. */
  url(token: string, since: bigint | null): string;
  /** Whether this is an agent composer — session tab 0, or the config assistant —
   *  which gets the Shift+Enter→LF newline (clipboard.ts). A shell/process tab does
   *  not. */
  composerNewline: boolean;
}

/**
 * sessionStreamEndpoint addresses a session's per-tab PTY. It reproduces EXACTLY the
 * URL AttachTerminal built inline before the endpoint seam: ?access_token=, then the
 * stable ?tab_id= (#1738) or the legacy ordinal ?tab= for a tab with no id, then
 * ?since= on a reconnect — same params in the same order, so the wire request is
 * byte-identical and the session stream is unchanged.
 */
export function sessionStreamEndpoint(sessionId: string, tabId: string, tab: number): StreamEndpoint {
  return {
    composerNewline: tab === 0,
    url(token, since) {
      const base = `${wsScheme()}//${window.location.host}/v1/sessions/${encodeURIComponent(sessionId)}/stream`;
      const params = new URLSearchParams();
      params.set("access_token", token);
      if (tabId !== "") {
        params.set("tab_id", tabId);
      } else if (tab > 0) {
        params.set("tab", String(tab));
      }
      if (since !== null) {
        params.set("since", since.toString());
      }
      return `${base}?${params.toString()}`;
    },
  };
}

/**
 * configAssistantStreamEndpoint addresses the daemon-owned config assistant (#2467).
 * The path names no session — the daemon resolves the single shared assistant — so it
 * carries only ?access_token= (and ?since= on a reconnect), never a tab selector. It
 * is an agent composer. The route is reached only after a POST /v1/config-assistant
 * has spawned-or-reused the assistant (api.ts), so the stream finds it live.
 */
export function configAssistantStreamEndpoint(): StreamEndpoint {
  return {
    composerNewline: true,
    url(token, since) {
      const base = `${wsScheme()}//${window.location.host}/v1/config-assistant/stream`;
      const params = new URLSearchParams();
      params.set("access_token", token);
      if (since !== null) {
        params.set("since", since.toString());
      }
      return `${base}?${params.toString()}`;
    },
  };
}
