// How a pane ADDRESSES the tab it shows — the one question that decides whether a
// tab moving to a new ordinal invalidates what a live pane is pointed at (#1779).
//
// Kept in its own css-free module (like layout.ts, and for the same reason): the
// logic lives beside split.ts's rendering but must be importable — and unit-testable
// — without dragging in xterm and its CSS, which the node test runner cannot load.

/** Whether a web-tab target points at a loopback host (localhost/127.x/::1) — the
 *  only targets the daemon reverse-proxies. Mirrors session.IsLoopbackWebTarget
 *  (session/weburl.go). A URL that does not parse is treated as non-loopback. */
export function isLoopbackWebUrl(raw: string): boolean {
  try {
    let host = new URL(raw).hostname.toLowerCase();
    host = host.replace(/^\[|\]$/g, ""); // strip IPv6 brackets
    return host === "localhost" || host === "::1" || host === "127.0.0.1" || host.startsWith("127.");
  } catch {
    return false;
  }
}

/** The same-origin daemon proxy path for a loopback web tab, so the iframe hits
 *  the daemon (which shares the machine with the dev server) rather than the
 *  viewer's own machine. The bearer token rides ?access_token= for network peers
 *  (an iframe src can't set the Authorization header); a loopback/tokenless client
 *  sends none. The trailing slash matters — the route requires it, and it makes
 *  the dev app's RELATIVE asset URLs resolve under the proxy prefix.
 *
 *  NOTE the {tabIdx}: this route is ORDINAL-addressed, so unlike a PTY stream a web
 *  tab cannot be addressed by its stable id. That is why paneAddressUsesOrdinal
 *  answers true for a proxied tab however stable its id is. */
export function webProxyPath(sessionId: string, tabIdx: number, token: string | null): string {
  const base = `/v1/webtab/${encodeURIComponent(sessionId)}/${tabIdx}/`;
  return token ? `${base}?access_token=${encodeURIComponent(token)}` : base;
}

/** Whether a pane's live address for its tab EMBEDS that tab's ordinal — i.e. whether
 *  the tab merely shifting position invalidates what the pane already points at
 *  (#1779). It decides whether a moved tab must be torn down and rebuilt, or can
 *  simply be followed.
 *
 *  The question is NOT "does this tab have a stable id" — it is "does the address this
 *  pane already holds still name the right thing". The two come apart:
 *
 *  - A terminal streams `?tab_id=<id>` when the tab has a real id and `?tab=<ordinal>`
 *    otherwise — never both (terminal.ts picks one). So an id-addressed terminal's
 *    ordinal is inert and a move needs no rebuild, while a legacy one's ordinal IS the
 *    address.
 *  - A web tab ignores the tab id entirely. A LOOPBACK preview is fetched through the
 *    daemon proxy at /v1/webtab/{session}/{ordinal}/ — ordinal-keyed no matter how
 *    stable the tab's id is — so a moved proxied tab MUST rebuild, or its iframe
 *    silently proxies whatever tab took its old index. An EXTERNAL tab's src is the
 *    target URL itself and encodes no ordinal, so it can be followed.
 *
 *  Conflating the two either reloads every moved iframe (needless in-page state loss)
 *  or, worse, leaves a proxied frame pointed at a different tab — the misroute this
 *  all exists to end.
 *
 *  `webTarget` is null for a terminal pane; `realId` is "" for a tab with no daemon id. */
export function paneAddressUsesOrdinal(webTarget: string | null, realId: string): boolean {
  if (webTarget !== null) {
    return webTarget !== "" && isLoopbackWebUrl(webTarget); // proxied → /v1/webtab/…/{ordinal}/
  }
  return realId === ""; // a legacy terminal streams by ?tab=<ordinal>
}
