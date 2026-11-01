// The Backlog view's HTTP contract (backlog proposal, api.md): POST /v1/<Verb>
// in the shared envelope. Every project route names its project by repo_path,
// the root the web scopes by; the daemon resolves it to the repo id.

import { af } from "./api.js";
import {
  type Budget, type BacklogView, type Item, type Message, type MoveTarget, type RunRef, type Settings, type Status, parseBacklogView,
} from "./backlog_model.js";
import { type Usage, parseUsage } from "./backlog_usage.js";

export async function getBacklog(root: string, token: string): Promise<BacklogView> {
  return parseBacklogView(await af<unknown>("GetBacklog", { repo_path: root }, token));
}

export interface BudgetReply {
  budget: Budget;
  usage: Usage;
}

function parseBudgetReply(resp: { budget?: Budget } | null): BudgetReply {
  const b = resp?.budget;
  return {
    budget: {
      share: Number(b?.share) || 0,
      stop_at: Number(b?.stop_at) || 0,
      max_per_run: Number(b?.max_per_run) || 0,
      usd_limit: Number(b?.usd_limit) || 0,
      usd_spent: Number(b?.usd_spent) || 0,
    },
    usage: parseUsage(resp),
  };
}

export async function getBudget(token: string): Promise<BudgetReply> {
  return parseBudgetReply(await af<{ budget?: Budget }>("GetBudget", {}, token));
}

export type BudgetUpdate = Partial<Pick<Budget, "share" | "stop_at" | "max_per_run" | "usd_limit">>;

export async function setBudget(update: BudgetUpdate, token: string): Promise<BudgetReply> {
  return parseBudgetReply(await af<{ budget?: Budget }>("SetBudget", {
    share: update.share, stop_at: update.stop_at, max_per_run: update.max_per_run, usd_limit: update.usd_limit,
  }, token));
}

export type SettingsUpdate = Partial<Settings>;

export async function setSettings(root: string, update: SettingsUpdate, token: string): Promise<Settings> {
  const resp = await af<{ settings: Settings }>("SetBacklogSettings", {
    repo_path: root, runner: update.runner, max_running: update.max_running, on_done: update.on_done,
  }, token);
  return resp.settings;
}

/** Starts the item now, past the runner switch and the budget. A new session's
 *  run comes back still starting. */
export async function startItem(root: string, id: number, token: string): Promise<RunRef> {
  const resp = await af<{ run: RunRef }>("StartBacklogItem", { repo_path: root, id }, token);
  return resp.run;
}

export interface RunReply {
  started: number[];
  reason: string;
}

/** Whether the daemon serves RunBacklog (it does since the runner loop landed, #6300). Kept so a
 *  test can drive the bar without the button. */
export const backlogRoutes = { runBacklog: true };

/** One runner pass now for one item, past the runner switch and the budget. */
export async function runNext(root: string, token: string): Promise<RunReply> {
  const resp = await af<{ started?: number[]; reason?: string }>("RunBacklog", { repo_path: root, count: 1 }, token);
  return { started: Array.isArray(resp?.started) ? resp.started : [], reason: resp?.reason ?? "" };
}

export async function answerQuestion(root: string, id: number, questionId: number, text: string, token: string): Promise<Message> {
  const resp = await af<{ message: Message }>("PostBacklogMessage", {
    repo_path: root, id, kind: "answer", text, question_id: questionId,
  }, token);
  return resp.message;
}

export interface NewItem {
  title: string;
  parent?: number;
}

interface AddResponse {
  ids?: { [key: string]: number };
}

export async function addItem(root: string, input: NewItem, token: string): Promise<number> {
  const resp = await af<AddResponse>("AddBacklogItems", {
    repo_path: root,
    items: [{ key: "new", title: input.title, parent: input.parent || undefined }],
  }, token);
  return resp?.ids?.new ?? 0;
}

export interface ItemUpdate {
  title?: string;
  body?: string;
  waits_on?: number[];
  sequential?: boolean;
}

export async function updateItem(root: string, id: number, update: ItemUpdate, token: string): Promise<Item> {
  const resp = await af<{ item: Item }>("UpdateBacklogItem", {
    repo_path: root,
    id,
    title: update.title,
    body: update.body,
    waits_on: update.waits_on,
    sequential: update.sequential,
  }, token);
  return resp.item;
}

export async function moveItem(root: string, id: number, to: MoveTarget, token: string): Promise<void> {
  await af<unknown>("MoveBacklogItem", {
    repo_path: root,
    id,
    before: "before" in to ? to.before : undefined,
    after: "after" in to ? to.after : undefined,
    into: "into" in to ? to.into : undefined,
    top: "top" in to ? true : undefined,
    bottom: "bottom" in to ? true : undefined,
  }, token);
}

export async function setItemStatus(root: string, id: number, status: Status, token: string): Promise<void> {
  await af<unknown>("SetBacklogItemStatus", { repo_path: root, id, status }, token);
}

export async function deleteItem(root: string, id: number, token: string): Promise<void> {
  await af<unknown>("DeleteBacklogItem", { repo_path: root, id }, token);
}

export async function addNote(root: string, id: number, text: string, token: string): Promise<Message> {
  const resp = await af<{ message: Message }>("PostBacklogMessage", { repo_path: root, id, kind: "note", text }, token);
  return resp.message;
}
