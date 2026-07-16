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
// TWO RULES KEEP THAT SAFE. Both are easy to break with a well-meaning edit:
//
// 1. THE HANDLED SET IS AN ALLOWLIST, NEVER A /v1 DENYLIST.
//    The obvious way to write this is "intercept everything except /v1/*". Don't.
//    A denylist is only correct until someone adds a route — the next API prefix,
//    proxy hop, or streaming endpoint is intercepted BY DEFAULT and silently
//    inherits caching nobody asked for. The allowlist inverts that: the shell files
//    below are handled, and every other path on this origin — /v1 REST, the
//    /v1/events and /v1/sessions/*/stream sockets, /v1/webtab/* previews, the vscode
//    proxy, and anything added later — passes through because it was never named.
//    New routes are safe by construction, not by remembering to exclude them.
//
// 2. "PASSTHROUGH" MEANS NOT CALLING respondWith AT ALL.
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
const VERSION = "__AF_SHELL_VERSION__";
const CACHE = `af-shell-${VERSION}`;

/** The exact same-origin paths this worker will handle. Anything absent passes
 *  through untouched; see rule 1 above before adding to it. */
const SHELL_PATHS = new Set(["/", "/index.html", "/af-web.js", "/af-web.css", "/manifest.webmanifest"]);

/** True for a path the shell owns. The icons are a prefix match (there are six of
 *  them and they change as a set); everything else is exact. */
function isShellPath(pathname) {
  return SHELL_PATHS.has(pathname) || pathname.startsWith("/icons/");
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

/** Network-first: try the network, cache what came back, and fall back to the cached
 *  copy only once the network has actually failed. */
async function networkFirst(request) {
  try {
    const response = await fetch(request);
    // Only stash a real success. Caching an error or an opaque response would let a
    // transient 502 poison the offline fallback until the next deploy.
    if (response.ok && response.type === "basic") {
      const cache = await caches.open(CACHE);
      // Clone before returning: a Response body can only be read once, and the
      // caller gets the original.
      cache.put(request, response.clone()).catch(() => {
        // A full/blocked cache must never fail the navigation it came from.
      });
    }
    return response;
  } catch (err) {
    // Scoped to THIS build's cache, not the global caches.match: the global one
    // searches every cache in creation order, so a superseded af-shell-* that
    // activate has not finished deleting could answer with the previous build's
    // shell. Narrow, but it is precisely the staleness this worker is arranged to
    // make impossible.
    const cache = await caches.open(CACHE);
    const cached = await cache.match(request);
    if (cached) {
      return cached;
    }
    // Nothing cached and no network: rethrow so the browser shows its own network
    // error, exactly as it would with no worker installed.
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
  if (!isShellPath(url.pathname)) {
    return; // rule 2: no respondWith, so the browser handles it as if we did not exist
  }
  event.respondWith(networkFirst(request));
});
