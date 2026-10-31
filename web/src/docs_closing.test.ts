import assert from "node:assert/strict";
import { test } from "node:test";
import { ClosingTabs, type ClosingTab } from "./docs_closing.js";

class Tab implements ClosingTab {
  abandoned: boolean[] = [];
  private waiters: (() => void)[] = [];
  quiet = false;
  whenQuiet(): Promise<void> {
    return this.quiet ? Promise.resolve() : new Promise((r) => this.waiters.push(r));
  }
  settle(): void {
    this.quiet = true;
    this.waiters.splice(0).forEach((r) => r());
  }
  abandon(keep: boolean): void {
    this.abandoned.push(keep);
  }
}

test("a tab that loads takes over the closed tabs and clears them", () => {
  const c = new ClosingTabs<Tab>();
  const a = new Tab();
  const loader = new Tab();
  c.track("k", a);
  assert.deepEqual(c.priors("k", loader), [a]);
  c.takeOver("k", loader, true);
  assert.deepEqual(a.abandoned, [true]);
  assert.equal(c.has("k"), false);
});

test("a loader that was closed meanwhile takes nothing over and stays tracked until quiet", async () => {
  const c = new ClosingTabs<Tab>();
  const a = new Tab();
  const loader = new Tab();
  c.track("k", a);
  c.track("k", loader); // the loader's tab was closed while its first read was out
  c.takeOver("k", loader, false); // its read answers
  assert.deepEqual(a.abandoned, [], "the tab that is still writing is not abandoned");
  assert.deepEqual([...c.get("k")!], [a, loader], "and the next tab still waits for both");
  a.settle();
  loader.settle();
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(c.has("k"), false, "tracking ends when they are quiet");
});
