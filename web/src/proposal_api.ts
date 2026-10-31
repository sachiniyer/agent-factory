// The Docs tab's whole HTTP and event contract with the daemon, in one module, so
// a change to the routes or shapes is an edit here and nowhere else. The contract
// is docs/dev/proposals/proposal-mode/api.md: POST /v1/<Verb> RPCs in the shared
// envelope, each addressed by the session that owns the Docs tab.

import { af, ApiError, errorText } from "./api.js";

export interface ProposalDocRef {
  /** The doc name: "api". */
  name: string;
  /** The file name: "api.md". Replies and events name docs by this. */
  file: string;
}

export interface ProposalInfo {
  slug: string;
  head: string;
  docs: ProposalDocRef[];
  /** Template names that this proposal has no doc for yet, in template order. */
  templates: string[];
  /** The `> [!question]` callouts in all docs of the proposal. */
  open_questions: number;
  /** The push status of the tab's session; null when it is not attached to this proposal. */
  push: PushStatus | null;
}

export type PushMode = "auto" | "manual";
export type PushState = "idle" | "countdown" | "waiting_agent" | "held" | "sent";

/** A session's push status: the `push` of GetProposal, a push route reply, or a
 *  `proposal.push` event. */
export interface PushStatus {
  sessionId: string;
  slug: string;
  mode: PushMode;
  state: PushState;
  /** Milliseconds to the push when the daemon built this status; null when none is due.
   *  Count from the moment the status arrives: the daemon clock is not the browser's. */
  dueInMs: number | null;
  unsent: number;
}

export interface DocText {
  text: string;
  commit: string;
}

export type SaveResult =
  | { kind: "saved"; text: string; commit: string; merged: boolean }
  /** `head` is the commit to send as the base of the save that resolves it. */
  | { kind: "conflict"; ours: string; theirs: string; base: string; head: string };

/** A `proposal.changed` event from /v1/events. */
export interface ProposalChange {
  repoId: string;
  slug: string;
  /** The doc file that changed ("api.md"); "" when the event names none. */
  doc: string;
  /** "" for an edit on disk that is not committed yet. */
  commit: string;
  author: string;
}

export const DOC_NAME_PATTERN = /^[a-z0-9][a-z0-9-]{0,62}$/;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

export function parseProposalInfo(d: unknown): ProposalInfo {
  if (!isRecord(d)) {
    throw new ApiError(500, "the daemon sent a proposal without fields");
  }
  const docs = Array.isArray(d.docs) ? d.docs : [];
  return {
    slug: str(d.slug),
    head: str(d.head),
    docs: docs.flatMap((entry): ProposalDocRef[] => {
      if (typeof entry === "string") {
        const name = entry.replace(/\.md$/, "");
        return [{ name, file: `${name}.md` }];
      }
      if (isRecord(entry) && typeof entry.name === "string") {
        return [{ name: entry.name, file: str(entry.file) || `${entry.name}.md` }];
      }
      return [];
    }),
    templates: Array.isArray(d.templates) ? d.templates.filter((t): t is string => typeof t === "string") : [],
    open_questions: typeof d.open_questions === "number" ? d.open_questions : 0,
    push: parsePushStatus(d.push),
  };
}

const PUSH_STATES: readonly string[] = ["idle", "countdown", "waiting_agent", "held", "sent"];

/** Reads a push status. Returns null for a value that is not one. */
export function parsePushStatus(d: unknown): PushStatus | null {
  if (!isRecord(d) || typeof d.state !== "string" || !PUSH_STATES.includes(d.state)) {
    return null;
  }
  const due = typeof d.due_in_ms === "number" && Number.isFinite(d.due_in_ms) ? Math.max(0, d.due_in_ms) : null;
  return {
    sessionId: str(d.session_id),
    slug: str(d.slug),
    mode: d.mode === "manual" ? "manual" : "auto",
    state: d.state as PushState,
    dueInMs: due,
    unsent: typeof d.unsent === "number" && d.unsent > 0 ? d.unsent : 0,
  };
}

function pushReply(d: unknown): PushStatus {
  const status = parsePushStatus(d);
  if (!status) {
    throw new ApiError(500, "the daemon sent a push status without a state");
  }
  return status;
}

/** Reads a `proposal.push` event. Returns null for any other event. */
export function parseProposalPush(ev: unknown): PushStatus | null {
  if (!isRecord(ev) || ev.type !== "proposal.push") {
    return null;
  }
  const status = parsePushStatus(ev.data);
  return status && status.sessionId !== "" && status.slug !== "" ? status : null;
}

export async function holdPush(sessionId: string, token: string): Promise<PushStatus> {
  return pushReply(await af<unknown>("HoldProposalPush", { session_id: sessionId }, token));
}

/** Send now: sends at once when the agent is Ready, else on its next Ready tick. */
export async function sendChanges(sessionId: string, token: string): Promise<PushStatus> {
  return pushReply(await af<unknown>("SendProposalChanges", { session_id: sessionId }, token));
}

export function parseDocText(d: unknown): DocText {
  if (!isRecord(d) || typeof d.text !== "string") {
    throw new ApiError(500, "the daemon sent a doc without text");
  }
  return { text: d.text, commit: str(d.commit) };
}

/** Reads a SaveProposalDoc reply: the HTTP status and the parsed envelope. */
export function parseSaveResponse(status: number, body: unknown): SaveResult {
  const env = isRecord(body) ? body : {};
  const data = env.data;
  if (status === 409 && isRecord(data) && typeof data.ours === "string" && typeof data.theirs === "string") {
    return { kind: "conflict", ours: data.ours, theirs: data.theirs, base: str(data.base), head: str(data.head) };
  }
  const err = env.error;
  if (status < 200 || status >= 300 || err != null) {
    const message = isRecord(err) && typeof err.message === "string" ? err.message : `save failed with status ${status}`;
    throw new ApiError(status, message, isRecord(err) ? str(err.code) : "");
  }
  if (!isRecord(data) || typeof data.text !== "string") {
    throw new ApiError(500, "the daemon sent a save result without text");
  }
  return { kind: "saved", text: data.text, commit: str(data.commit), merged: data.merged === true };
}

export async function getProposal(sessionId: string, slug: string, token: string): Promise<ProposalInfo> {
  return parseProposalInfo(await af<unknown>("GetProposal", { session_id: sessionId, slug }, token));
}

export async function getDoc(sessionId: string, slug: string, doc: string, token: string): Promise<DocText> {
  return parseDocText(await af<unknown>("ReadProposalDoc", { session_id: sessionId, slug, doc }, token));
}

/** Browsers cap the body of a keepalive request at 64 KiB. */
export const KEEPALIVE_BODY_LIMIT = 60_000;

/** Whether a request body of `body` can ride a keepalive fetch. The limit applies
 *  to the serialized JSON, which escaping makes longer than the text it holds. */
export function fitsKeepalive(body: string): boolean {
  return new TextEncoder().encode(body).length < KEEPALIVE_BODY_LIMIT;
}

/** Saves a doc. A 409 is a result, not an error: it carries both sides of the
 *  conflict, which af() would drop, so this call reads the envelope itself.
 *  `keepalive` asks for a request that outlives the page; it is dropped when the
 *  serialized body is too large for one, so the save is still sent. */
export async function putDoc(
  sessionId: string,
  slug: string,
  doc: string,
  text: string,
  base: string,
  token: string,
  keepalive = false,
  /** Names this editor and counts its saves upward: the daemon refuses (409 proposal_stale_save) a
   *  save at or below one it has already seen from the same writer, so a request that outlived
   *  its client cannot land after that client's later saves. */
  fence?: { writer: string; seq: number },
): Promise<SaveResult> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token !== "") {
    headers.Authorization = `Bearer ${token}`;
  }
  let resp: Response;
  const payload = JSON.stringify({ session_id: sessionId, slug, doc, text, base, ...(fence ? { writer: fence.writer, seq: fence.seq } : {}) });
  try {
    resp = await fetch("/v1/SaveProposalDoc", {
      method: "POST",
      headers,
      body: payload,
      keepalive: keepalive && fitsKeepalive(payload),
    });
  } catch (e) {
    throw new ApiError(0, `cannot reach the daemon: ${errorText(e)}`);
  }
  let body: unknown = null;
  try {
    body = await resp.json();
  } catch {
    // A body that is not JSON leaves only the status to report.
  }
  return parseSaveResponse(resp.status, body);
}

export async function addDoc(sessionId: string, slug: string, name: string, token: string): Promise<void> {
  await af<unknown>("AddProposalDoc", { session_id: sessionId, slug, name, author: "human" }, token);
}

export async function removeDoc(sessionId: string, slug: string, doc: string, token: string): Promise<void> {
  await af<unknown>("RemoveProposalDoc", { session_id: sessionId, slug, doc, author: "human" }, token);
}

/** Reads a `proposal.changed` event. The fields sit under `data`. Returns null
 *  for any other event. */
export function parseProposalChange(ev: unknown): ProposalChange | null {
  if (!isRecord(ev) || ev.type !== "proposal.changed" || !isRecord(ev.data)) {
    return null;
  }
  const d = ev.data;
  const slug = str(d.slug);
  if (slug === "") {
    return null;
  }
  return { repoId: str(d.repo_id), slug, doc: str(d.doc), commit: str(d.commit), author: str(d.author) };
}

/** Whether an event names this doc. Events use the file name; a bare name is
 *  accepted too. */
export function changeNamesDoc(change: ProposalChange, doc: ProposalDocRef): boolean {
  return change.doc === doc.file || change.doc === doc.name;
}
