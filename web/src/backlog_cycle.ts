// The waits-on picker's cycle rule: a port of backlog.CheckWaitsOn
// (backlog/cycle.go), so the picker can disable a choice with the same reason
// the daemon would refuse it with. The daemon still checks every save.

import { type Backlog, formatID, isAncestor, isParent, itemByID } from "./backlog_model.js";

// Each item has a start node and a done node; an edge points at what a node needs.
type Node = string;

const start = (id: number): Node => `${id}s`;
const done = (id: number): Node => `${id}d`;
const nodeID = (n: Node): number => Number(n.slice(0, -1));
const isDone = (n: Node): boolean => n.endsWith("d");

function edges(b: Backlog): Map<Node, Node[]> {
  const g = new Map<Node, Node[]>();
  const add = (from: Node, to: Node): void => {
    g.set(from, [...(g.get(from) ?? []), to]);
  };
  const exists = new Set(b.items.map((it) => it.id));
  const prevChild = new Map<number, number>();
  for (const it of b.items) {
    for (const w of it.waits_on ?? []) {
      if (exists.has(w)) add(start(it.id), done(w));
    }
    const p = it.parent ?? 0;
    if (p !== 0 && exists.has(p)) {
      add(start(it.id), start(p));
      const prev = prevChild.get(p);
      if (prev !== undefined && itemByID(b, p)?.sequential) add(start(it.id), done(prev));
      prevChild.set(p, it.id);
      add(done(p), done(it.id));
    }
  }
  for (const it of b.items) {
    if (!isParent(b, it.id)) add(done(it.id), start(it.id));
  }
  return g;
}

function findPath(g: Map<Node, Node[]>, from: Node, to: Node): Node[] | null {
  const prev = new Map<Node, Node>([[from, from]]);
  const queue: Node[] = [from];
  while (queue.length > 0) {
    let n = queue.shift() as Node;
    if (n === to) {
      const path: Node[] = [];
      for (; n !== from; n = prev.get(n) as Node) path.push(n);
      path.push(from);
      return path.reverse();
    }
    for (const m of g.get(n) ?? []) {
      if (!prev.has(m)) {
        prev.set(m, n);
        queue.push(m);
      }
    }
  }
  return null;
}

function collapse(path: Node[]): number[] {
  const ids: number[] = [];
  for (const n of path) {
    const id = nodeID(n);
    if (ids.length === 0 || ids[ids.length - 1] !== id) ids.push(id);
  }
  return ids;
}

function isWaitEdge(b: Backlog, from: Node, to: Node): boolean {
  if (isDone(from) || !isDone(to) || nodeID(from) === nodeID(to)) return false;
  return (itemByID(b, nodeID(from))?.waits_on ?? []).includes(nodeID(to));
}

function allWaitEdges(b: Backlog, path: Node[]): boolean {
  for (let k = 0; k + 1 < path.length; k++) {
    const from = path[k];
    const to = path[k + 1];
    if (nodeID(from) === nodeID(to) && isDone(from) && !isDone(to)) continue;
    if (!isWaitEdge(b, from, to)) return false;
  }
  return true;
}

const formatPath = (ids: number[]): string => ids.map(formatID).join(" → ");

/** Why id may not wait on target, or null when it may. The text matches the
 *  daemon's refusal. */
export function waitsOnBlock(b: Backlog, id: number, target: number): string | null {
  if (!itemByID(b, id) || !itemByID(b, target)) return `${formatID(target)} not found`;
  const a = formatID(id);
  const t = formatID(target);
  if (id === target) return `${a} cannot wait on itself`;
  if (isAncestor(b, target, id)) return `${a} cannot wait on ${t}: ${t} contains it`;
  if (isAncestor(b, id, target)) return `${a} cannot wait on ${t}: ${t} is inside it`;
  const g = edges(b);
  const from = done(target);
  const to = start(id);
  g.set(to, [...(g.get(to) ?? []), from]);
  const path = findPath(g, from, to);
  if (path === null) return null;
  const ids = [id, ...collapse(path)];
  if (!allWaitEdges(b, path)) return `${a} cannot wait on ${t}: that would make a wait cycle (${formatPath(ids)})`;
  return `${a} cannot wait on ${t}: ${t} already waits on ${a} (${formatPath(ids)})`;
}
