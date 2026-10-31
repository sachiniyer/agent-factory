// The Backlog view's HTTP contract (backlog proposal, api.md): POST /v1/<Verb>
// in the shared envelope. Every project route names its project by repo_path,
// the root the web scopes by; the daemon resolves it to the repo id.

import { af } from "./api.js";
import { type Budget, type BacklogView, type Item, type Message, type MoveTarget, type Status, parseBacklogView } from "./backlog_model.js";

export async function getBacklog(root: string, token: string): Promise<BacklogView> {
  return parseBacklogView(await af<unknown>("GetBacklog", { repo_path: root }, token));
}

export async function getBudget(token: string): Promise<Budget> {
  const resp = await af<{ budget?: Budget }>("GetBudget", {}, token);
  const b = resp?.budget;
  return {
    share: Number(b?.share) || 0,
    stop_at: Number(b?.stop_at) || 0,
    max_per_run: Number(b?.max_per_run) || 0,
    usd_limit: Number(b?.usd_limit) || 0,
    usd_spent: Number(b?.usd_spent) || 0,
  };
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
