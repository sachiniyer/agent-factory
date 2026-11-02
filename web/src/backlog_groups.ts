// Backlog v2 groups and inbox (backlog-v2 proposal, technical.md "Groups" and
// "Inbox"): pure rules over GetBacklog data. A group is a top-level parent.

import {
  type Backlog, type BacklogView, type Item, type Message, type State,
  archivedBy, derivedOf, isAncestor, isParent, itemByID, latestRun, openQuestions,
} from "./backlog_model.js";
import { isArchived, rowKind } from "./status.js";
import { InFlightOp, type SessionData } from "./types.js";

export function isGroup(b: Backlog, it: Item): boolean {
  return !it.parent && isParent(b, it.id);
}

export interface InboxEntry {
  item: Item;
  question: Message;
}

/** Every open question of the project, oldest first, leaving out archived
 *  groups. An item that holds two open questions shows each. */
export function inbox(view: BacklogView): InboxEntry[] {
  const out: InboxEntry[] = [];
  for (const item of view.backlog.items) {
    if (archivedBy(view.backlog, item.id)) continue;
    for (const question of openQuestions(item)) out.push({ item, question });
  }
  const at = (e: InboxEntry): number => Date.parse(e.question.at) || 0;
  return out.sort((a, b) => at(a) - at(b));
}

export type AnswerEffect = "live" | "restore" | "new_run" | "checking" | "unknown";

/** What af learned about a run session the page does not hold: asked, answered
 *  (absent after that means gone), or could not tell. */
export type SessionLookup = "pending" | "done" | "failed";

/** What an answer to the item's question does, as the daemon decides it
 *  (backlog/service_messages.go Post): only a started run whose session is
 *  listed, not killed by the user and not being killed gets the answer;
 *  otherwise the answer starts a new run. A page holds only live sessions
 *  until it looks others up, so with lookup an absent session is not taken as
 *  gone until the daemon confirms it; without lookup, absence counts as gone. */
export function answerEffect(it: Item, sessions: readonly SessionData[],
  lookup?: (sessionId: string) => SessionLookup | undefined): AnswerEffect {
  const run = latestRun(it);
  if (run?.state !== "started" || !run.session_id) return "new_run";
  const s = sessions.find((x) => x.id === run.session_id);
  if (!s && lookup) {
    const l = lookup(run.session_id);
    if (l === "failed") return "unknown";
    if (l !== "done") return "checking";
  }
  if (!s || s.user_killed === true || (s.in_flight_op ?? InFlightOp.None) === InFlightOp.Killing) return "new_run";
  if (isArchived(s)) return "restore";
  const kind = rowKind(s);
  return kind === "lost" || kind === "dead" ? "restore" : "live";
}

export function answerEffectText(effect: AnswerEffect): string {
  switch (effect) {
    case "restore":
      return "Restores the session";
    case "new_run":
      return "Starts a new run with the previous agent's notes";
    case "checking":
      return "Checking the session…";
    case "unknown":
      return "Can't tell yet whether the session is there";
    default:
      return "Goes to the session";
  }
}

export interface Dependency {
  from: Item;
  to: Item | undefined;
  toID: number;
  /** Whether `to` is in the group too. */
  inside: boolean;
}

function inGroup(b: Backlog, group: number, id: number): boolean {
  return id === group || isAncestor(b, group, id);
}

/** Each waits-on link from an item in the group, in tree order. */
export function groupDependencies(b: Backlog, group: number): Dependency[] {
  const out: Dependency[] = [];
  for (const from of b.items) {
    if (!inGroup(b, group, from.id)) continue;
    for (const toID of from.waits_on ?? []) {
      out.push({ from, to: itemByID(b, toID), toID, inside: inGroup(b, group, toID) });
    }
  }
  return out;
}

const closed = (s: State): boolean => s === "done" || s === "dropped";

/** The group's done and dropped items, newest first. */
export function groupHistory(view: BacklogView, group: number): Item[] {
  const b = view.backlog;
  const at = (it: Item): number => Date.parse(it.updated_at) || 0;
  return b.items
    .filter((it) => isAncestor(b, group, it.id) && closed(derivedOf(view, it.id).state))
    .sort((x, y) => at(y) - at(x));
}
