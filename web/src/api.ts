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
  // An empty token is the "this client needs none" sentinel (loopback, or
  // require_token=false, #1696): send no Authorization header at all rather than a
  // bogus "Bearer " — the daemon exempts the peer, so the header is simply absent.
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token !== "") {
    headers.Authorization = `Bearer ${token}`;
  }
  let resp: Response;
  try {
    resp = await fetch(`/v1/${method}`, {
      method: "POST",
      headers,
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

/**
 * Asks the daemon whether THIS client must present a bearer token, via the
 * unauthenticated `/v1/auth-info` probe (#1696). The daemon answers from the
 * transport peer address and its policy: false when it exempts this client —
 * a loopback browser, or a network client under require_token=false — so the SPA
 * can skip the paste-token login and connect with no credential; true otherwise.
 *
 * Fails SAFE: a body missing the flag is treated as "token required", so a probe
 * the daemon didn't answer as expected never silently drops the login. Throws
 * ApiError on a transport failure so the caller can fall back to the token form.
 */
export async function probeAuthRequired(): Promise<boolean> {
  let resp: Response;
  try {
    resp = await fetch("/v1/auth-info", { method: "GET" });
  } catch (e) {
    throw new ApiError(0, `cannot reach the daemon: ${(e as Error).message}`);
  }
  let env: Envelope<{ auth_required?: boolean }> | null = null;
  try {
    env = (await resp.json()) as Envelope<{ auth_required?: boolean }>;
  } catch {
    // Non-JSON body: fall through to the status check / safe default below.
  }
  if (!resp.ok) {
    throw new ApiError(resp.status, env?.error ?? `${resp.status} ${resp.statusText}`);
  }
  return env?.data?.auth_required !== false;
}

// --- lifecycle mutations (#1592 Phase 5 PR5) ------------------------------
//
// The write side of the web client: the create/send-prompt/kill/archive verbs the
// modals drive, each a thin POST to the daemon RPC the TUI/CLI already speak. They
// send title-scoped requests with an EMPTY repo_id — the all-repo lookup the CLI
// uses (`af sessions kill <title>`) — because the web is an all-repos client (like
// the daemon Snapshot) and the InstanceData projection carries no repo id. The
// daemon is the single writer; the resulting create/kill/archive event flows back
// over /v1/events and updates the rail live, so these calls never touch local
// state themselves.

/** The new-session form's inputs (a subset of daemon.CreateSessionRequest). */
export interface CreateSessionInput {
  /** The desired title; sent as `title_base` so the daemon auto-suffixes a
   *  collision (the TUI's friendly behavior) and returns the resolved title. */
  title: string;
  /** The picked project's repo root (InstanceData.worktree.repo_path). */
  repoPath: string;
  /** The agent program (claude/codex/…); empty resolves the repo default. */
  program: string;
  /** An optional initial prompt fed to the new agent. */
  prompt: string;
  /** Whether to run the session in auto-yes mode. */
  autoYes: boolean;
}

/** Creates a session and returns the daemon's authoritative projection of it (the
 *  resolved title + stable id). The created row also arrives via /v1/events. */
export async function createSession(input: CreateSessionInput, token: string): Promise<SessionData> {
  const resp = await af<{ instance: SessionData }>(
    "CreateSession",
    {
      title_base: input.title,
      repo_path: input.repoPath,
      program: input.program,
      prompt: input.prompt,
      auto_yes: input.autoYes,
    },
    token,
  );
  return resp.instance;
}

// The lifecycle mutations send the session's stable `id` as the primary lookup
// key alongside the (display) title, with an empty repo_id. The daemon resolves
// the target by id FIRST, so a duplicate title across repos can never make the
// web kill/archive/prompt the wrong session — the write-path analogue of the
// id-keyed read/stream paths (#1592 Phase 5 PR5). The title is still sent so the
// daemon's lifecycle event and any title-only fallback stay correct.

/** Sends a prompt to an existing session (mirrors `af sessions send-prompt`). */
export async function sendPrompt(id: string, title: string, prompt: string, token: string): Promise<void> {
  await af("SendPrompt", { id, title, repo_id: "", prompt }, token);
}

/** Kills a session (mirrors `af sessions kill`). The session.killed event removes
 *  its row from the rail live. */
export async function killSession(id: string, title: string, token: string): Promise<void> {
  await af("KillSession", { id, title, repo_id: "" }, token);
}

/** Archives a session (mirrors `af sessions archive`) — non-destructive, keeps it
 *  restorable. The session.archived event triggers a rail resync. */
export async function archiveSession(id: string, title: string, token: string): Promise<void> {
  await af("ArchiveSession", { id, title, repo_id: "" }, token);
}

// --- tab mutations (#1592 Phase 5 PR7) -------------------------------------
//
// The web tab bar's write verbs, mirroring the TUI's `t`/`w` keys: `t` creates a
// $SHELL tab (Shell=true, exactly like Instance.AddShellTab — no command prompt),
// and `w` closes a non-agent tab. Both are thin POSTs to the daemon's CreateTab /
// CloseTab RPCs the TUI/CLI already speak (#930/#960). Both send the session's
// STABLE id as the primary lookup key alongside the (display) title with an EMPTY
// repo_id: the daemon resolves by id FIRST (resolveActionSession), so a duplicate
// title across repos can never make the web create/close a tab on the wrong
// session (#1592 Phase 5 PR7 / the #1678 id-scoping class).
//
// FAIL-CLOSED on a missing id (#1592 PR7 review): the daemon still title-resolves
// an EMPTY id (the CLI path, `af sessions tab-create <title>`), so the web MUST NOT
// send one on these destructive-ish ops — an empty id + all-repo title is the
// cross-repo landmine #1678 eliminated. Every live Snapshot row carries an id, so
// this only trips on a legacy/disk-only record; it refuses BEFORE the request
// rather than silently retargeting by title. The daemon emits NO event for a tab
// change, so callers resync the Snapshot to refresh the rail's tab list.

/** Refuses a tab op that has no stable session id to target, before any request is
 *  issued — so a missing id can never fall through to the daemon's all-repo title
 *  match on a cross-repo duplicate title (the #1678 class). */
function requireSessionID(id: string, action: string): void {
  if (id === "") {
    throw new ApiError(0, `cannot ${action}: this session has no stable id to target safely`);
  }
}

/** Creates a $SHELL tab on the session (mirrors the TUI `t` key). Returns the
 *  daemon's resolved, collision-suffixed tab name. Refuses a session with no id. */
export async function createTab(id: string, title: string, token: string): Promise<string> {
  requireSessionID(id, "create a tab");
  const resp = await af<{ name: string }>(
    "CreateTab",
    { id, title, repo_id: "", shell: true, command: "", name: "" },
    token,
  );
  return resp.name;
}

/** Closes a non-agent tab by name (mirrors the TUI `w` key). The agent tab
 *  (index 0) is refused daemon-side — kill the session to tear it down. Refuses a
 *  session with no stable id, before issuing the request. */
export async function closeTab(id: string, title: string, tabName: string, token: string): Promise<void> {
  requireSessionID(id, "close a tab");
  await af("CloseTab", { id, title, repo_id: "", tab_name: tabName, tab_index: 0 }, token);
}
