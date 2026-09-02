// Coverage for the latest-request fence every refetch in index.ts commits through
// (#3659, generalizing #2330 and #3654).
//
// Two halves, and the second is the one no amount of driving the helper can reach:
//
//   - the RULE — an older response commits nothing once a newer request has gone
//     out, whichever order they settle in, on success and on failure alike. Driven
//     below per projection, with two overlapping fake responses resolved out of
//     order, because in-order resolution hides the bug entirely.
//   - the WIRING — index.ts routes each projection THROUGH the fence rather than
//     settling a response inline. index.ts cannot be imported here (it builds DOM
//     nodes and mounts at import time), so that half is asserted against its source.
//     Without it every rule test below would still pass in a tree where nothing used
//     the fence at all, which is exactly the tree this issue was filed against.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import type { RegisteredProject } from "./api.js";
import { createFencedRefetcher, createLatestRequestGate } from "./refetch.js";
import type { ConfigResponse } from "./types.js";

const srcRoot = dirname(fileURLToPath(import.meta.url));

/** Lets the promise chains inside the refetcher run to completion. */
function settle(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}

/** A projection whose responses are handed back on demand, so a test can land them
 *  in an order the network chose rather than the order they were issued in. */
function pendingFetch<T>(): {
  fetch: (token: string) => Promise<T>;
  tokens: string[];
  resolve: (index: number, value: T) => void;
  reject: (index: number, error: unknown) => void;
} {
  const settlers: Array<{ resolve: (value: T) => void; reject: (error: unknown) => void }> = [];
  const tokens: string[] = [];
  return {
    fetch: (token: string) => {
      tokens.push(token);
      return new Promise<T>((resolve, reject) => {
        settlers.push({ resolve, reject });
      });
    },
    tokens,
    resolve: (index, value) => settlers[index].resolve(value),
    reject: (index, error) => settlers[index].reject(error),
  };
}

function project(root: string): RegisteredProject {
  return { id: root, checkout_id: root, root, relative_root: root, path_exists: true };
}

function config(value: string): ConfigResponse {
  return {
    path: "/home/u/.af/config.toml",
    entries: [
      {
        key: "default_program",
        type: "string",
        default: "claude",
        purpose: "The agent a new session starts",
        tier: 1,
        tier_name: "core",
        settable: true,
        value,
        requires_restart: false,
      },
    ],
  };
}

// --- the rule, per projection ----------------------------------------------

test("#3659 an older ListProjects response commits nothing once a newer one has landed", async () => {
  const committed: string[][] = [];
  const projects = pendingFetch<RegisteredProject[]>();
  const refetcher = createFencedRefetcher({
    readToken: () => "tok",
    fetch: projects.fetch,
    commit: (value) => {
      committed.push(value.map((p) => p.root));
    },
  });

  // Registering a project fires both triggers by design: the modal refetches
  // directly, and the projects.changed it causes schedules the debounced resync.
  refetcher.refresh(); // the direct one — issued first, still reading the OLD registry
  refetcher.refresh(); // the resync — issued second, and the one that sees the new project

  projects.resolve(1, [project("/repo/one"), project("/repo/two")]);
  await settle();
  projects.resolve(0, [project("/repo/one")]);
  await settle();

  assert.deepEqual(
    committed,
    [["/repo/one", "/repo/two"]],
    "the pre-registration list must not drop the just-registered project back out of the switcher",
  );
});

test("#3659 an older GetConfig response commits nothing once a newer one has landed", async () => {
  const committed: string[] = [];
  const entries = pendingFetch<ConfigResponse>();
  const refetcher = createFencedRefetcher({
    readToken: () => "tok",
    fetch: entries.fetch,
    commit: (value) => {
      committed.push(value.entries[0].value);
    },
  });

  // Entering the config view fetches; setting a key refetches. Set one soon after
  // arriving and the two overlap.
  refetcher.refresh(); // the view-entry read, carrying the PRE-write value
  refetcher.refresh(); // the post-write read, carrying what the writer stored

  entries.resolve(1, config("codex"));
  await settle();
  entries.resolve(0, config("claude"));
  await settle();

  assert.deepEqual(committed, ["codex"], "the view-entry read must not redisplay the value the write replaced");
});

test("#3659 an older response commits nothing even when it FAILS", async () => {
  const committed: string[] = [];
  const surfaced: unknown[] = [];
  const entries = pendingFetch<ConfigResponse>();
  const refetcher = createFencedRefetcher({
    readToken: () => "tok",
    fetch: entries.fetch,
    commit: (value) => {
      committed.push(value.entries[0].value);
    },
    onError: (error) => {
      surfaced.push(error);
    },
  });

  refetcher.refresh();
  refetcher.refresh();

  entries.resolve(1, config("codex"));
  await settle();
  entries.reject(0, new Error("connection reset"));
  await settle();

  assert.deepEqual(committed, ["codex"]);
  assert.deepEqual(surfaced, [], "a stale request's failure must not paint an error over the fresher answer");
});

test("#3659 the newest response still reports its own failure", async () => {
  const surfaced: string[] = [];
  const entries = pendingFetch<ConfigResponse>();
  const refetcher = createFencedRefetcher({
    readToken: () => "tok",
    fetch: entries.fetch,
    commit: () => {
      assert.fail("nothing to commit");
    },
    onError: (error) => {
      surfaced.push(String(error));
    },
  });

  refetcher.refresh();
  entries.reject(0, new Error("the read failed"));
  await settle();

  assert.deepEqual(surfaced, ["Error: the read failed"], "a failure must still reach the tab-error line");
});

// --- the token half of the fence -------------------------------------------

test("#3659 a response issued under a rotated credential commits nothing", async () => {
  const committed: string[] = [];
  const entries = pendingFetch<ConfigResponse>();
  let token: string | null = "first";
  const refetcher = createFencedRefetcher({
    readToken: () => token,
    fetch: entries.fetch,
    commit: (value) => {
      committed.push(value.entries[0].value);
    },
  });

  refetcher.refresh();
  // The newest generation is still this request's; only the token moved. It now
  // describes a daemon session that is no longer the one on screen.
  token = "second";
  entries.resolve(0, config("claude"));
  await settle();

  assert.deepEqual(committed, [], "the generation alone cannot catch a rotation");
});

test("#3659 a disconnect drops an in-flight response even under the same credential", async () => {
  const committed: string[] = [];
  const tasks = pendingFetch<string[]>();
  const refetcher = createFencedRefetcher({
    readToken: () => "",
    fetch: tasks.fetch,
    commit: (value) => {
      committed.push(...value);
    },
  });

  refetcher.refresh();
  // Reconnecting on the tokenless loopback (#1696) moves neither the generation nor
  // the token, so stopStream's invalidate() is the only thing that can close it.
  refetcher.invalidate();
  tasks.resolve(0, ["a task removed while the stream was away"]);
  await settle();

  assert.deepEqual(committed, []);
});

test("#3659 no credential issues no request, and the tokenless sentinel is a credential", () => {
  const disconnected = pendingFetch<string[]>();
  createFencedRefetcher({
    readToken: () => null,
    fetch: disconnected.fetch,
    commit: () => assert.fail("a disconnected client must not fetch"),
  }).refresh();
  assert.deepEqual(disconnected.tokens, []);

  const tokenless = pendingFetch<string[]>();
  createFencedRefetcher({
    readToken: () => "",
    fetch: tokenless.fetch,
    commit: () => {},
  }).refresh();
  assert.deepEqual(tokenless.tokens, [""], '"" is the authorized tokenless credential (#1696), not "no token"');
});

test("request generations reject an older completion for the same token", () => {
  const gate = createLatestRequestGate();
  const older = gate.begin();
  const newer = gate.begin();

  assert.equal(older.isCurrent(), false);
  assert.equal(newer.isCurrent(), true);
});

// --- the wiring -------------------------------------------------------------

/** The source of one top-level `function name(): void { … }`, closing brace included.
 *  Top-level declarations close at column 0, which is what makes this reliable
 *  without parsing TypeScript. */
function topLevelFunction(source: string, name: string): string {
  const header = `function ${name}(): void {`;
  const start = source.indexOf(header);
  assert.notEqual(start, -1, `index.ts must declare ${name}`);
  const end = source.indexOf("\n}\n", start);
  assert.notEqual(end, -1, `${name} must close at column 0`);
  return source.slice(start, end);
}

test("#3659 every refetcher in index.ts commits through the fence, not inline", () => {
  const source = readFileSync(join(srcRoot, "index.ts"), "utf8");
  const refetchers: string[] = [];

  for (const name of ["refreshConfig", "refreshRegisteredProjects", "refreshTasks"]) {
    const body = topLevelFunction(source, name);
    const delegation = /\b(\w+)\.refresh\(\)/.exec(body);
    assert.ok(delegation, `${name} must issue its request through a fenced refetcher`);
    const refetcher = delegation[1];
    assert.match(
      source,
      new RegExp(`const ${refetcher} = createFencedRefetcher\\(`),
      `${refetcher} must be built by createFencedRefetcher`,
    );
    // The response is settled INSIDE the fence or not at all: a `.then` here would be
    // a second, unfenced path to the store — which is the whole of #3659.
    assert.doesNotMatch(body, /\.then\(|store\.set\(/, `${name} must not settle a response outside the fence`);
    refetchers.push(refetcher);
  }

  // And a response outstanding across a stream teardown is disowned: reconnecting
  // with the same credential moves neither the generation nor the token.
  const teardown = topLevelFunction(source, "stopStream");
  for (const refetcher of refetchers) {
    assert.match(teardown, new RegExp(`${refetcher}\\.invalidate\\(\\)`), `stopStream must invalidate ${refetcher}`);
  }
});
