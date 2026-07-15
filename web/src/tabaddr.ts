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

/** The path component of a web-tab target, as the proxy URL must mirror it. Returns
 *  "" for a root target (or an unparseable one), whose proxy path is just the tab
 *  prefix. Percent-encoding in the target is preserved verbatim — `pathname` is
 *  already the escaped form, so it is spliced in without a re-encode. */
function targetPathOf(target: string): string {
  try {
    const p = new URL(target).pathname;
    return p === "/" ? "" : p.replace(/^\//, "");
  } catch {
    return "";
  }
}

/** The same-origin daemon proxy path for a loopback web tab, so the iframe hits
 *  the daemon (which shares the machine with the dev server) rather than the
 *  viewer's own machine. The bearer token rides ?access_token= for network peers
 *  (an iframe src can't set the Authorization header); a loopback/tokenless client
 *  sends none.
 *
 *  Two things this URL must get right:
 *
 *  - {tabId} is the tab's STABLE id (#1738), never its ordinal. Closing a LOWER tab
 *    shifts every higher ordinal down, so an ordinal-keyed src left an open frame
 *    silently proxying a DIFFERENT dev server (#1810). By id, a moved tab keeps
 *    resolving to itself and a closed one 404s.
 *  - The target's own path is MIRRORED into the URL, so the browser resolves the
 *    app's relative URLs at the same depth the dev server serves them at:
 *    target http://localhost:3000/app/viewer.html → /v1/webtab/<sid>/<id>/app/viewer.html.
 *    A sibling (x.css) and a PARENT-relative link (../shared.css) then both land
 *    inside the prefix, and a subdirectory target loads. The daemon forwards the
 *    remainder verbatim (daemon/webtab_proxy.go).
 *
 *  The trailing slash on a root target matters: the route requires it, and it keeps
 *  the app's relative URLs resolving under the prefix rather than beside it. */
export function webProxyPath(
  sessionId: string,
  tabId: string,
  target: string,
  token: string | null,
): string {
  const base = `/v1/webtab/${encodeURIComponent(sessionId)}/${encodeURIComponent(tabId)}/${targetPathOf(target)}`;
  return token ? `${base}?access_token=${encodeURIComponent(token)}` : base;
}

/** Whether a pane's live address for its tab EMBEDS that tab's ordinal — i.e. whether
 *  the tab merely shifting position invalidates what the pane already points at
 *  (#1779). It decides whether a moved tab must be torn down and rebuilt, or can
 *  simply be followed.
 *
 *  The question is NOT "does this tab have a stable id" — it is "does the address this
 *  pane already holds still name the right thing".
 *
 *  Only ONE address form still embeds an ordinal: a legacy terminal streaming
 *  `?tab=<ordinal>` because its tab has no id. Everything else is ordinal-free and a
 *  moved tab is simply followed —
 *
 *  - a terminal with a real id streams `?tab_id=<id>` (terminal.ts sends one or the
 *    other, never both), so its captured ordinal is inert;
 *  - a proxied web tab's src is `/v1/webtab/{session}/{tabId}/…` — id-keyed since
 *    #1810, so a shifted ordinal no longer changes what it fetches;
 *  - an external web tab's src is the target URL itself and encodes no ordinal.
 *
 *  Web panes used to answer true here purely BECAUSE the proxy route was
 *  ordinal-keyed: a moved tab had to be torn down or its frame would silently proxy
 *  whoever took its old index. Keying the route by id removed the reason, so a moved
 *  preview now keeps its live frame — and its dev server's in-page state — instead of
 *  reloading for nothing.
 *
 *  `webTarget` is null for a terminal pane; `realId` is "" for a tab with no daemon id. */
export function paneAddressUsesOrdinal(webTarget: string | null, realId: string): boolean {
  if (webTarget !== null) {
    return false; // proxied → id-keyed (#1810); external → the target URL itself
  }
  return realId === ""; // a legacy terminal streams by ?tab=<ordinal>
}
