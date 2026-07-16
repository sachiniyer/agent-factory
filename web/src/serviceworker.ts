// Service-worker registration (feat: PWA). The worker itself is src/sw.js — plain
// JS, copied to the dist root by build.mjs rather than bundled, because a worker must
// be served from the scope it controls. Read the header there before touching either.

/**
 * Registers /sw.js, best-effort.
 *
 * Every failure mode here is a silent no-op ON PURPOSE. The worker buys exactly one
 * thing — Chrome's installability bar — and the app is fully functional without it,
 * so nothing it can do is worth a console error in a user's face, let alone a broken
 * boot.
 *
 * `"serviceWorker" in navigator` is false on an INSECURE CONTEXT, which is the case
 * that actually happens: reaching the daemon over plain HTTP on a Tailscale address
 * has no navigator.serviceWorker at all. So this returns early there, no install
 * affordance appears (install.ts), and the app runs exactly as before. That is the
 * designed outcome, not a degradation — see docs/web.md.
 *
 * Registration is deferred to `load` so it never competes with the bundle and the
 * first /v1 calls for bandwidth on a cold open; the worker controls nothing until its
 * next navigation anyway, so there is nothing to gain by racing it in earlier.
 */
export function registerServiceWorker(): void {
  if (!("serviceWorker" in navigator)) {
    return;
  }
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {
      // Blocked by policy, private mode, or an insecure origin that still exposed the
      // API. The app does not depend on the worker; leave the user alone.
    });
  });
}
