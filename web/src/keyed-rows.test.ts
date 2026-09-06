import { test } from "node:test";
import assert from "node:assert/strict";
import { KeyedRows, orderChildren } from "./keyed-rows.js";

test("1000 decoded snapshot rows preserve identity and render only changed sessions", () => {
  const cache = new KeyedRows<{ id: string; title: string }, { id: string; title: string }>();
  const values = Array.from({ length: 1000 }, (_, i) => ({ id: String(i), title: `Session ${i}` }));
  let renders = 0;
  const disposed: string[] = [];
  const reconcile = (rows: typeof values) => cache.reconcile(rows, s => s.id, JSON.stringify,
    (s, node) => { renders++; return Object.assign(node ?? {}, s); }, key => disposed.push(key));
  const first = reconcile(values);
  renders = 0;
  const same = reconcile(JSON.parse(JSON.stringify(values)));
  assert.equal(renders, 0);
  same.forEach((node, i) => assert.equal(node, first[i]));
  const changed = values.map(s => s.id === "500" ? { ...s, title: "Renamed" } : s);
  const updated = reconcile(changed);
  assert.equal(renders, 1);
  assert.equal(updated[500], first[500]);
  assert.equal(updated[500].title, "Renamed");
  renders = 0;
  const reordered = reconcile([...changed].reverse().slice(1));
  assert.equal(renders, 0);
  assert.equal(reordered[0], first[998]);
  assert.deepEqual(disposed, ["999"]);
});

test("ordering leaves unchanged nodes attached; inserts, moves and removes only differences", () => {
  class Node {
    constructor(readonly id: string) {}
    get nextElementSibling(): Node | null { return parent.children[parent.children.indexOf(this) + 1] ?? null; }
    remove() { parent.children.splice(parent.children.indexOf(this), 1); writes++; }
  }
  let writes = 0;
  const parent = {
    children: [] as Node[],
    get firstElementChild() { return this.children[0] ?? null; },
    insertBefore(child: Node, before: Node | null) {
      const old = this.children.indexOf(child);
      if (old !== -1) this.children.splice(old, 1);
      this.children.splice(before ? this.children.indexOf(before) : this.children.length, 0, child);
      writes++;
    },
  };
  const nodes = [new Node("a"), new Node("b"), new Node("c")];
  const order = (rows: Node[]) => orderChildren(parent as unknown as HTMLElement, rows as unknown as HTMLElement[]);
  order(nodes);
  writes = 0;
  order(nodes);
  assert.equal(writes, 0);
  order([nodes[2], nodes[0]]);
  assert.deepEqual(parent.children.map(n => n.id), ["c", "a"]);
  assert.equal(writes, 2);
  order(nodes);
  writes = 0;
  order(nodes.slice(1));
  assert.equal(writes, 1, "deleting the head must not move surviving rows");
  assert.deepEqual(parent.children, nodes.slice(1));
});
