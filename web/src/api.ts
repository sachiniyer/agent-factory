// The REST layer of the web client (#1592 Phase 5). It is the browser analogue of
// the Go apiclient: one af<T>() helper that POSTs /v1/<Method>, attaches the
// bearer token, and unwraps the shared {data,error} envelope (apiproto/envelope.go)
// so every call site gets uniform error handling. The daemon API is the contract;
// this file must not fork it.
//
// Token handling is deliberately minimal and lives here so there is ONE place that
// knows the credential: it is kept in sessionStorage (survives reload within the
// tab, gone on tab-close — design §1.2, a deliberate "don't persist a full-access
// credential to disk" posture) and attached as `Authorization: Bearer` on every
// request. The WS `?access_token=` fallback (browsers cannot set WS headers) is
// used by the /v1/events subscriber (events.ts) and, in PR4, the PTY stream.

import type { SessionData, SnapshotResponse } from "./types.js";

const TOKEN_KEY = "af.token";

/** Reads the stored bearer token for this tab, or null if none is set. */
export function loadToken(): string | null {
  return sessionStorage.getItem(TOKEN_KEY);
}

/** Persists the bearer token for this tab (sessionStorage, not localStorage). */
export function storeToken(token: string): void {
  sessionStorage.setItem(TOKEN_KEY, token);
}

/** Forgets the stored token (logout / failed probe). */
export function clearToken(): void {
  sessionStorage.removeItem(TOKEN_KEY);
}

/** The shared REST envelope every /v1/ route returns (apiproto/envelope.go). */
interface Envelope<T> {
  data: T | null;
  error: string | null;
}

/**
 * A failed API call. `status` is the HTTP status (0 for a network/transport
 * failure) so callers can distinguish 401-unauthorized from a genuine error.
 */
export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

/**
 * POSTs /v1/<method> with the given JSON body and bearer token, then unwraps the
 * {data,error} envelope. Throws ApiError on a non-2xx status, on an envelope with
 * a non-null error, or on a transport failure — mirroring apiclient.call so the
 * whole client shares one error path.
 */
export async function af<T>(method: string, body: unknown, token: string): Promise<T> {
  let resp: Response;
  try {
    resp = await fetch(`/v1/${method}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
      },
      body: JSON.stringify(body ?? {}),
    });
  } catch (e) {
    // A TypeError from fetch is a transport failure (daemon down, TLS rejected,
    // wrong host). Surface it as status 0 with an actionable message.
    throw new ApiError(0, `cannot reach the daemon: ${(e as Error).message}`);
  }

  // Parse the envelope regardless of status so a structured error message wins
  // over the bare status line when the daemon sent one.
  let env: Envelope<T> | null = null;
  try {
    env = (await resp.json()) as Envelope<T>;
  } catch {
    // Non-JSON body (unlikely from /v1/): fall through to the status-based error.
  }

  if (!resp.ok) {
    throw new ApiError(resp.status, env?.error ?? `${resp.status} ${resp.statusText}`);
  }
  if (env && env.error !== null) {
    throw new ApiError(resp.status, env.error);
  }
  return env?.data as T;
}

/**
 * Fetches the authoritative session projection for all repos (design §2.1). This
 * is the read side of the single-writer model (#960): the daemon owns the state
 * and the web mirrors this Snapshot exactly like the TUI does, seeding the rail on
 * login and re-seeding it after an events-stream reconnect. Returns the instances
 * (never null — an empty daemon yields []); throws ApiError on transport/auth
 * failure so callers share one error path.
 */
export async function fetchSnapshot(token: string): Promise<SessionData[]> {
  const resp = await af<SnapshotResponse>("Snapshot", { repo_id: "" }, token);
  return resp.instances ?? [];
}

/**
 * Validates a token by probing an authed endpoint with it. Snapshot is the auth
 * check (design §1.2): a 200 means the token is accepted; a 401 means it is not.
 * Returns the snapshot's instances on success so the caller can seed the rail in
 * the same round-trip as the login probe; throws ApiError otherwise.
 */
export function probeToken(token: string): Promise<SessionData[]> {
  return fetchSnapshot(token);
}
