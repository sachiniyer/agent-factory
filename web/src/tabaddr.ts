// How a pane ADDRESSES the tab it shows — the one question that decides whether a
// tab moving to a new ordinal invalidates what a live pane is pointed at (#1779).
//
// Kept in its own css-free module (like layout.ts, and for the same reason): the
// logic lives beside split.ts's rendering but must be importable — and unit-testable
// — without dragging in xterm and its CSS, which the node test runner cannot load.

import { TabKind } from "./types.js";

/** What an iframe pane shows: a web tab's target URL, or a vscode tab, which
 *  deliberately has NO target — its editor is a daemon-managed per-session
 *  code-server on an ephemeral port, so the proxy path is the only address that
 *  exists for it. See SplitView.iframeSpecAt. */
export type IframeSpec =
  | { kind: typeof TabKind.Web; target: string }
  | { kind: typeof TabKind.VSCode; target: "" };

/** Whether an iframe pane is served through the daemon proxy (rather than framing
 *  its target directly) — i.e. whether its src is a /v1/webtab/ path or the target
 *  URL itself. (It no longer feeds paneAddressUsesOrdinal: since #1810 the proxy is
 *  id-keyed, so no iframe pane addresses by ordinal and the question is moot.)
 *
 *  A vscode tab is ALWAYS proxied — its code-server is loopback-only, and it has no
 *  target to classify, so the empty-target test that decides a web tab would answer
 *  the wrong question for it. */
export function iframeIsProxied(spec: IframeSpec): boolean {
  if (spec.kind === TabKind.VSCode) {
    return true;
  }
  return spec.target !== "" && isLoopbackWebUrl(spec.target);
}

/** The stable identity of what an iframe pane is showing, used to decide whether a
 *  reconcile must rebuild the frame. It must NOT change across reconciles of an
 *  unchanged tab: a rebuild reloads the iframe, dropping a dev server's in-page
 *  state or a VS Code pane's unsaved buffers. A vscode tab has no target, so its
 *  identity is a constant — which is exactly right. The leading space keeps it from
 *  ever colliding with a real URL. */
export function iframeIdentity(spec: IframeSpec): string {
  return spec.kind === TabKind.VSCode ? " vscode" : spec.target;
}

/** Whether a web-tab target points at a loopback host (localhost/127.x/::1) — the
 *  only targets the daemon reverse-proxies. Mirrors session.IsLoopbackWebTarget
 *  (session/weburl.go). A URL that does not parse is treated as non-loopback. */
export function isLoopbackWebUrl(raw: string): boolean {
  try {
    let host = new URL(raw).hostname.toLowerCase();
    host = host.replace(/^\[|\]$/g, ""); // strip IPv6 brackets
    // A single trailing dot is the DNS root label. Strip exactly one to mirror
    // session.IsLoopbackWebTarget; a doubled dot remains malformed/fail-closed.
    host = host.replace(/\.$/, "");
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

/** The query of a web-tab target, WITHOUT its leading "?" ("" when it has none, or
 *  does not parse). The mirror is of the whole address, not just its path: a target
 *  is stored exactly as the user gave it (NormalizeWebTabURL keeps the query), and
 *  for plenty of dev servers the query IS the address — Storybook's ?path=/story/…,
 *  a viewer's ?doc=123. Dropping it opens the app's default view instead of the one
 *  the tab names.
 *
 *  Returned raw, not re-encoded through URLSearchParams, so the target's own
 *  escaping and parameter order reach the dev server exactly as written. */
function targetQueryOf(target: string): string {
  try {
    return new URL(target).search.replace(/^\?/, "");
  } catch {
    return "";
  }
}

/** The query param the daemon's OWN credential rides on a proxied web tab —
 *  deliberately NOT `access_token`. The proxy forwards the framed target's whole
 *  query to the dev server, so a daemon token under `access_token` would collide
 *  with a target that carries its own `access_token`: the daemon would read the
 *  app's value as its credential (401), or strip the app's value on the way
 *  upstream. A private name keeps them apart. Mirrors
 *  daemon/webtab_proxy.go `webtabTokenQueryParam`. */
const webtabTokenParam = "af_webtab_token";

/** The same-origin daemon proxy path for a loopback web tab, so the iframe hits
 *  the daemon (which shares the machine with the dev server) rather than the
 *  viewer's own machine. The bearer token rides the daemon-private
 *  ?af_webtab_token= for network peers (an iframe src can't set the Authorization
 *  header); a loopback/tokenless client sends none.
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
 *  - The target's own QUERY is mirrored too, for the same reason as its path: the
 *    tab's address is the whole URL. The daemon strips only its own
 *    ?af_webtab_token= before forwarding, so the app's own parameters — including
 *    an `access_token` of its own — ride through untouched. The target's query goes
 *    FIRST, so a dev server reading `?doc=` positionally sees what it would have
 *    without us, and the daemon's credential stays last and separable.
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
  const query = [targetQueryOf(target), token ? `${webtabTokenParam}=${encodeURIComponent(token)}` : ""]
    .filter((part) => part !== "")
    .join("&");
  return query ? `${base}?${query}` : base;
}

/**
 * The reload-only cache-busting query parameter (#1900). It carries no meaning beyond
 * "this is attempt N" — only its CHANGING is load-bearing.
 *
 * The name is chosen to be collision-proof against the app being previewed, because it
 * does not stop at the daemon: the proxy forwards the mirror path's query to the
 * upstream dev server (it strips only ?access_token=, webtab_proxy.go), so the dev
 * server sees this param too. That is harmless — a normal dev server ignores unknown
 * query params, and it is in fact what makes the bust effective end to end, since the
 * upstream is asked for a distinct URL as well. But it is also why the name is
 * namespaced (`af`) and underscore-prefixed rather than something like `t` or
 * `cachebust`, which a real app could plausibly read as its own.
 */
const RELOAD_PARAM = "_afreload";

/**
 * `src` with a cache-busting `_afreload=<n>` param, so the ↻ control really refetches
 * (#1900).
 *
 * Re-assigning the same URL to an iframe is not a guarantee of fresh content: the
 * browser's HTTP cache — or any intermediary between the daemon and the dev server —
 * may answer from a stale entry, which is precisely the page the user pressed ↻ to
 * escape. A URL that differs per attempt cannot be served from a prior entry.
 *
 * Two constraints the signature encodes:
 *
 *   - Callers must pass the PRISTINE src, never the frame's current one. That is what
 *     makes repeated reloads REPLACE the param rather than accumulate `_afreload=1&
 *     _afreload=2&…` into an ever-growing URL. It is structural: with a clean base
 *     there is nothing to accumulate onto, so no strip/rewrite step can be forgotten.
 *   - PROXIED targets only. An external target is never cache-busted, and the reason
 *     is not caution: a presigned or CDN-token URL signs over its query string, so an
 *     extra param invalidates the signature and turns a working preview into a 403.
 *     split.ts enforces the proxied-only gate; this function is the mechanism.
 */
export function cacheBustedWebSrc(src: string, n: number): string {
  const sep = src.includes("?") ? "&" : "?";
  return `${src}${sep}${RELOAD_PARAM}=${n}`;
}

/**
 * The next cache-busting value for a ↻ press — unique against every value this browser
 * has ALREADY used, not merely against the ones a given pane has.
 *
 * The counter is the whole fix, so what it is scoped to IS the correctness argument. A
 * cache-buster's only job is to name a URL the HTTP cache has never seen; a cache entry
 * outlives the pane that created it, outlives the session selection, and outlives the
 * PAGE. So a counter scoped to any of those collides with its own past the moment its
 * scope is recreated, and re-issues a URL the cache can still answer — serving the exact
 * stale page ↻ exists to escape, with the control appearing to work.
 *
 * That is why this is module-scope AND seeded from the clock, which are two separate
 * fixes for two scopes:
 *
 *   - MODULE scope (rather than a `let` inside the pane mount, #1900's original) fixes
 *     the collision across a pane recreate. A remount — switching tabs away and back,
 *     changing the target, an archive flip — reran the mount and reset its counter to 0,
 *     so the second ↻ of a session re-requested `_afreload=1`.
 *   - The Date.now() SEED (rather than 0) fixes the same collision one scope up, across
 *     a PAGE reload, which resets every module. F5 revalidates the document, not the
 *     iframe subresources the cache holds, so a fresh module counter starting at 1 walks
 *     straight back over the URLs the previous page issued.
 *
 * Monotonic rather than random: it is the same one-line guarantee (a value is never
 * re-issued) without a birthday collision to reason about, and a strictly-increasing
 * sequence is what a test can assert. Wall-clock going BACKWARDS (an NTP step) is the
 * one way to re-issue a value, and it would have to land on exactly a previously-used
 * integer to matter — strictly weaker than the every-remount collision this replaces.
 *
 * Numeric (not a random string) so the param stays `_afreload=<digits>`: proxies, the
 * daemon and the specs all read it as such.
 */
let reloadNonce = Date.now();

export function nextReloadNonce(): number {
  return ++reloadNonce;
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
 *  - an external web tab's src is the target URL itself and encodes no ordinal;
 *  - a VSCODE pane is always proxied, so it rides the same id-keyed guarantee: its
 *    src is /v1/webtab/{session}/{tabId}/, and a move cannot repoint it at another
 *    session's editor.
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

/** Whether a per-tab preview origin is even worth ASKING about from this page
 *  (#1856 step 3b) — a cheap precondition, never the whole answer.
 *
 *  A per-tab origin is an `http://af….localhost:<port>` name, and *.localhost is
 *  resolved by the BROWSER to its own loopback (RFC 6761) — never by DNS, never to
 *  the daemon's machine. Two things follow, and only the first is decidable here:
 *
 *  - an `https://` page cannot frame a plain-`http://` origin at all (mixed
 *    content), and a page served from a non-loopback host is definitively remote;
 *  - but a loopback `location` does NOT prove the daemon is local. Under
 *    `ssh -L 8443:127.0.0.1:8443 remote` the browser URL is `http://localhost:8443`
 *    while the daemon — and its preview port — are on the far end, and that port is
 *    usually not forwarded. Switching a frame there on the strength of this check
 *    alone would abandon a WORKING tunnelled mirror for an address that resolves to
 *    the viewer's own machine.
 *
 *  So this gates the round trip, and `previewOriginReachable` (split.ts) decides:
 *  it asks the browser to actually load something from the preview port. That also
 *  answers the browser-support question — Safari does not resolve *.localhost — with
 *  no user-agent sniffing.
 *
 *  Mirrors isLoopbackWebUrl's host rules on the PAGE's own address rather than a
 *  target's. */
export function canUsePreviewOrigin(loc: { protocol: string; hostname: string }): boolean {
  if (loc.protocol !== "http:") {
    return false; // an https:// page cannot frame a plain-http per-tab origin
  }
  const host = loc.hostname.toLowerCase().replace(/^\[|\]$/g, "").replace(/\.$/, "");
  return host === "localhost" || host === "::1" || host === "127.0.0.1" || host.startsWith("127.");
}

/** The iframe src for a web tab on its OWN preview origin (#1856 step 3b).
 *
 *  `origin` is what GET /v1/preview-auth vended for this tab — `http://af<label>
 *  .localhost:<port>`, where the label is an unguessable per-tab HMAC that IS the
 *  credential the preview listener authenticates. Nothing else is appended to
 *  authenticate: unlike webProxyPath there is no token query param, because the
 *  address itself carries the capability and a cross-site frame cannot be relied on
 *  to send a cookie at all.
 *
 *  The tab owns this origin's ROOT, so the target's path is mirrored directly onto
 *  it: target http://localhost:3000/app/viewer.html?doc=1 becomes
 *  http://af….localhost:8444/app/viewer.html?doc=1. That is the whole point of the
 *  per-tab origin — an ABSOLUTE-path asset the app emits (/assets/app.js) now
 *  resolves to the dev server's own /assets/app.js instead of escaping a mirror
 *  prefix and 404ing (#1811).
 *
 *  The target's own query rides through verbatim, raw, for the same reason
 *  webProxyPath keeps it: for plenty of dev servers the query IS the address. */
export function previewOriginSrc(origin: string, target: string): string {
  const base = `${origin.replace(/\/$/, "")}/${targetPathOf(target)}`;
  const query = targetQueryOf(target);
  return query ? `${base}?${query}` : base;
}

/** The `sandbox` attribute an iframe pane must carry, given WHAT it frames.
 *
 *  Three cases, and the difference between them is entirely "can this content reach
 *  the SPA and its bearer token":
 *
 *  - a WEB tab on the same-origin `/v1/webtab/` mirror gets NO allow-same-origin.
 *    It frames an arbitrary agent-supplied dev server on the SPA's own origin, so
 *    the opaque origin is the only thing standing between that content and the
 *    parent document. This is the default, and every remote viewer stays on it.
 *  - a WEB tab on its OWN preview origin (#1856) gets allow-same-origin, because
 *    the frame is CROSS-origin to the SPA: the browser's own origin check already
 *    denies it the parent, so the grant costs nothing and buys the dev server a real
 *    origin — localStorage, its own cookies, service workers, the things a preview
 *    that "just works" needs. Note the classic sandbox-escape (allow-scripts +
 *    allow-same-origin letting a frame remove its own sandbox attribute) requires
 *    the frame to be SAME-origin with the parent; it does not apply here, and that
 *    is precisely why this grant waited for the origin split.
 *  - a VSCODE tab always gets it, plus downloads. Its content is a process the
 *    daemon itself spawned, and VS Code cannot run under an opaque origin at all.
 *
 *  Kept here, beside the addressing that decides which origin a frame lands on, so
 *  the grant and its precondition are read together and unit-testable. */
export function webSandbox(isVSCode: boolean, onPreviewOrigin: boolean): string {
  const base = "allow-scripts allow-forms allow-popups allow-modals";
  if (isVSCode) {
    return `${base} allow-same-origin allow-downloads`;
  }
  return onPreviewOrigin ? `${base} allow-same-origin` : base;
}
