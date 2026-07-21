// The Agent Factory service worker: the minimum that makes the app installable
// without ever touching a live connection.
//
// This file is PLAIN JS and is NOT an esbuild input — build.mjs copies it to
// dist/sw.js verbatim (bar the version stamp below), because a service worker must
// be served from the scope root to control "/" and cannot ride inside the bundle.
//
// It exists for one reason: Chrome will not offer to install a site whose service
// worker has no fetch handler. Everything past that bar is deliberately as close to
// nothing as it can be, because the app it is wrapping is a live terminal — a stray
// intercept here is not a slow page, it is somebody's PTY stream stalling.
//
// THREE RULES KEEP THAT SAFE. Each is easy to break with a well-meaning edit:
//
// 1. /v1 IS NEVER HANDLED, and that check comes FIRST, before anything else can
//    catch it. The API, the /v1/events and /v1/sessions/*/stream sockets, the
//    /v1/webtab/* previews (local web tabs AND the vscode editor both proxy through
//    there), all of it passes straight through. It is one guard at the top rather
//    than an exclusion sprinkled through every branch, so no later rule can reach a
//    /v1 request by accident.
//
// 2. WHAT *IS* HANDLED SPLITS IN TWO, and neither half is a "/v1 denylist".
//    - Sub-resources are an ALLOWLIST: only the shell files named in SHELL_PATHS.
//      A denylist ("cache everything except /v1") is correct only until someone adds
//      a route, which would then be intercepted BY DEFAULT; the allowlist makes new
//      routes pass through because they were never named.
//    - Top-level NAVIGATIONS mirror the daemon. serveSPA answers every extensionless
//      non-/v1 route with the index.html shell (client-side routing: /tasks,
//      /sessions/<id>, any deep-link), so the worker must fall back the same way, or
//      an offline refresh or bookmark of one of those routes shows the browser's
//      network-error page instead of the app's own daemon-unreachable screen. The
//      shell fallback is served for any non-/v1 navigation, but only the canonical
//      entry ( / or /index.html ) ever WRITES the shell cache — so no other
//      navigation's body can land in the shell slot.
//
// 3. "PASSTHROUGH" MEANS NOT CALLING respondWith AT ALL.
//    event.respondWith(fetch(event.request)) LOOKS like a passthrough and is not:
//    it re-issues the request from the worker, which re-wraps the body stream and
//    can quietly alter streaming, range, and credential behaviour. Simply returning
//    from the handler leaves the request on the browser's own default path, which is
//    byte-for-byte what happens with no service worker installed. That is the only
//    passthrough this file uses.
//
// A note on WebSockets, since it is the first thing to worry about here: a ws://
// handshake does not fire a service worker's fetch event at all — the spec routes
// only http(s) requests through it — so the PTY and event sockets are untouchable by
// this file even if the rules above were broken. The allowlist is still what the
// selftest asserts against, because "unreachable in today's spec" is a weaker
// guarantee than "never named in the first place".
//
// CACHING IS NETWORK-FIRST, and that direction is load-bearing rather than a taste
// call: af auto-updates, so a cache-first shell would keep handing users the bundle
// from before the update with no way to know. Network-first means the cache is only
// ever consulted when the network already failed, so a reachable daemon always wins
// and staleness is impossible. The cache exists so that a daemon that went away
// yields the app's own "can't reach the daemon" screen instead of the browser's
// dinosaur.
//
// The cache therefore warms on the SECOND load, not the first: a first visit fetches
// the shell before this worker exists, so those requests never reach the handler, and
// clients.claim() only takes over once the page is already up. There is deliberately
// no install-time precache to close that gap. cache.addAll is all-or-nothing, so one
// renamed icon would fail the INSTALL — and a worker that fails to install is a worker
// that does not exist, which costs the installability this file is here to buy. A
// one-load-late fallback is a much better trade than a fragile one.

// Stamped by build.mjs with a hash of the built shell. It is a CONTENT hash rather
// than the af version on purpose: CI bumps main.go's version without rebuilding
// web/dist, so a version stamp would desync the committed bundle from its own cache
// name. The hash changes when — and only when — the shell bytes change.
const VERSION = "1ae52e8c1ecd";
const CACHE = `af-shell-${VERSION}`;

/** The exact same-origin SUB-RESOURCE paths this worker will handle. Anything absent
 *  passes through untouched; see the rules above before adding to it. */
const SHELL_PATHS = new Set(["/", "/index.html", "/af-web.js", "/af-web.css", "/manifest.webmanifest"]);

/** The one key every navigation's offline fallback resolves to. A deep route's shell
 *  is byte-identical to it, so caching only this entry lets one copy serve them all. */
const SHELL_FALLBACK = "/index.html";

/** True for a sub-resource path the shell owns. The icons are a prefix match (there
 *  are six of them and they change as a set); everything else is exact. */
function isShellPath(pathname) {
  return SHELL_PATHS.has(pathname) || pathname.startsWith("/icons/");
}

/** True for the canonical shell entry — the only navigation whose response is written
 *  to the shell cache, so no other navigation can overwrite the fallback. */
function isShellEntry(pathname) {
  return pathname === "/" || pathname === "/index.html";
}

// Take over as soon as we're installed rather than idling until every tab closes.
// Pairing skipWaiting with clients.claim() is what makes an auto-update actually
// land: without it a user who updated af could keep the previous worker — and its
// cache name — for the rest of the browser session.
self.addEventListener("install", () => {
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      // Drop every previous shell cache. The version is a content hash, so anything
      // not matching the current name is by definition a superseded build.
      const names = await caches.keys();
      await Promise.all(names.filter((n) => n.startsWith("af-shell-") && n !== CACHE).map((n) => caches.delete(n)));
      await self.clients.claim();
    })(),
  );
});

/** Writes a response into THIS build's cache under `key` (a Request or a URL string).
 *  Swallows a full/blocked store so it can never fail the request it came from. The
 *  caller hands this to event.waitUntil so the write completes within the fetch
 *  event's lifetime — a bare, un-awaited put can be cut off if the worker is
 *  terminated right after the response is returned, leaving the cache unwarmed and the
 *  offline fallback dependent on timing. */
async function cacheShell(key, response) {
  try {
    const cache = await caches.open(CACHE);
    await cache.put(key, response);
  } catch {
    // A full or blocked cache is not an error worth failing a live request over.
  }
}

/** Reads THIS build's cache for a request, scoped to CACHE rather than the global
 *  caches.match: the global one searches every cache in creation order, so a
 *  superseded af-shell-* that activate has not finished deleting could answer with the
 *  previous build's shell — precisely the staleness this worker is arranged to prevent. */
async function cachedShell(key) {
  const cache = await caches.open(CACHE);
  return cache.match(key);
}

/** Network-first for a shell sub-resource: try the network, cache a real success, and
 *  fall back to the cached copy only once the network has actually failed. */
async function networkFirst(event, request) {
  try {
    const response = await fetch(request);
    // Only stash a real success. Caching an error or an opaque response would let a
    // transient 502 poison the offline fallback until the next deploy. Clone before
    // returning — a body can be read once, and the caller gets the original — and let
    // the write finish under the event's lifetime (see cacheShell).
    if (response.ok && response.type === "basic") {
      event.waitUntil(cacheShell(request, response.clone()));
    }
    return response;
  } catch (err) {
    const cached = await cachedShell(request);
    if (cached) {
      return cached;
    }
    // Nothing cached and no network: rethrow so the browser shows its own network
    // error, exactly as it would with no worker installed.
    throw err;
  }
}

/** Network-first for a top-level navigation. Mirrors the daemon's serveSPA: a
 *  reachable daemon's response wins, and offline the request falls back to the cached
 *  shell so a refresh or bookmark of a deep route (/tasks, /sessions/<id>) renders the
 *  app rather than a browser error. Only the canonical entry writes the cache, so no
 *  other navigation's body can overwrite the fallback. */
async function handleNavigation(event, pathname) {
  try {
    const response = await fetch(event.request);
    if (response.ok && response.type === "basic" && isShellEntry(pathname)) {
      event.waitUntil(cacheShell(SHELL_FALLBACK, response.clone()));
    }
    return response;
  } catch (err) {
    const cached = await cachedShell(SHELL_FALLBACK);
    if (cached) {
      return cached;
    }
    // No cached shell and no network — rethrow to the browser's own error, unchanged.
    throw err;
  }
}

self.addEventListener("fetch", (event) => {
  const request = event.request;
  // Non-GET never touches the shell (and a cached POST is meaningless).
  if (request.method !== "GET") {
    return;
  }
  const url = new URL(request.url);
  // Cross-origin is somebody else's problem — an external web tab, most likely.
  if (url.origin !== self.location.origin) {
    return;
  }
  // Rule 1: /v1 is never handled, checked first so nothing below can reach it. The
  // API, both WS endpoints, and the /v1/webtab/* proxy (web tabs + vscode) pass through.
  if (url.pathname.startsWith("/v1/")) {
    return;
  }
  // Rule 2a: a top-level navigation (including a client-routed deep-link) falls back
  // to the cached shell, mirroring serveSPA.
  if (request.mode === "navigate") {
    event.respondWith(handleNavigation(event, url.pathname));
    return;
  }
  // Rule 2b: shell sub-resources only. Everything else passes through (rule 3: no
  // respondWith, so the browser handles it as if we did not exist).
  if (!isShellPath(url.pathname)) {
    return;
  }
  event.respondWith(networkFirst(event, request));
});
