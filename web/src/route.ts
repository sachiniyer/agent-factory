// The web client's one route: stable session identity, scoped to this daemon.
export interface SessionRoute { session: string }

export function parseRoute(hash: string): SessionRoute | null {
  const match = /^#\/session\/([^/]+)$/.exec(hash);
  if (!match) return null;
  try {
    const session = decodeURIComponent(match[1]);
    return session.trim() && !/[\x00-\x1f\x7f]/.test(session) ? { session } : null;
  } catch {
    return null;
  }
}

export function serializeRoute(route: SessionRoute | null): string {
  return route?.session ? `#/session/${encodeURIComponent(route.session)}` : "";
}

/** A link to a project by its repo root. The switcher's rows point here so a
 *  browser "Open in new tab" lands on that project. It is consumed on resolve:
 *  the address bar then follows the session selection as usual. */
export interface ProjectRoute { project: string }

export function parseProjectRoute(hash: string): ProjectRoute | null {
  const match = /^#\/project\/([^/]+)$/.exec(hash);
  if (!match) return null;
  try {
    const project = decodeURIComponent(match[1]);
    return project.trim() && !/[\x00-\x1f\x7f]/.test(project) ? { project } : null;
  } catch {
    return null;
  }
}

export function serializeProjectRoute(route: ProjectRoute): string {
  return route.project ? `#/project/${encodeURIComponent(route.project)}` : "";
}

/** True for any fragment the client resolves: a session or a project link. */
function isRoute(hash: string): boolean {
  return parseRoute(hash) !== null || parseProjectRoute(hash) !== null;
}

const ROUTE_KEY = "af-session-route";
type RouteStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

export function stashRoute(hash: string, storage: RouteStorage): void {
  try {
    if (isRoute(hash)) storage.setItem(ROUTE_KEY, hash);
    else storage.removeItem(ROUTE_KEY);
  } catch { /* Storage may be disabled. The current fragment still works. */ }
}

export function restoreRoute(hash: string, storage: RouteStorage): string {
  try {
    const saved = storage.getItem(ROUTE_KEY);
    storage.removeItem(ROUTE_KEY);
    // An explicit incoming fragment always wins over a previous login attempt.
    return hash || (saved && isRoute(saved) ? saved : "");
  } catch {
    return hash;
  }
}

export function replaceRoute(route: SessionRoute | null): void {
  const hash = serializeRoute(route);
  if (location.hash !== hash) history.replaceState(null, "", location.pathname + location.search + hash);
}

export function sessionURL(id: string): string {
  return new URL("/" + serializeRoute({ session: id }), location.origin).href;
}

/** Restore the login handoff before the shell performs any selection updates. */
export function restoreLoginRoute(): void {
  try {
    const hash = restoreRoute(location.hash, sessionStorage);
    if (hash !== location.hash) history.replaceState(null, "", location.pathname + location.search + hash);
  } catch { /* Accessing sessionStorage itself can be denied. */ }
}

export function stashLoginRoute(): void {
  try { stashRoute(location.hash, sessionStorage); } catch { /* Storage may be disabled. */ }
}

export function clearLoginRoute(): void {
  try { sessionStorage.removeItem(ROUTE_KEY); } catch { /* Storage may be disabled. */ }
}
