// Tests for the rail's status filter (feat: hide archived by default). They pin the
// three rules the feature rests on: the DEFAULT hides archived and nothing else, the
// partition keys on the DISPLAYED status (so an in-flight op filters as Working), and
// a persisted filter round-trips while defaulting any state it doesn't mention.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  FILTER_KINDS,
  type StatusFilter,
  defaultFilter,
  filterLabel,
  filterSessions,
  hiddenCount,
  isDefaultFilter,
  kindCounts,
  loadFilter,
  persistFilter,
  withKind,
} from "./filter.js";
import { OPERATOR_KIND_LABELS, type OperatorKind } from "./status.js";
import { InFlightOp, Liveness, Status, type SessionData } from "./types.js";

function sess(over: Partial<SessionData> = {}): SessionData {
  return { title: "s", branch: "b", ...over };
}

/** A session per operator kind, so a filter can exercise every action group at once. */
const ROWS: Record<OperatorKind, SessionData> = {
  "needs-you": sess({ title: "needs-you", liveness: Liveness.Ready, idle_reason: "settled-after-pane-change" }),
  working: sess({ title: "working", liveness: Liveness.Running }),
  "waiting-limit": sess({ title: "waiting-limit", liveness: Liveness.LimitReached }),
  broken: sess({ title: "broken", liveness: Liveness.Lost }),
  archived: sess({ title: "archived", liveness: Liveness.Archived }),
};

const ALL: SessionData[] = FILTER_KINDS.map((k) => ROWS[k]);

/** Installs a fresh in-memory localStorage (node has no DOM) and returns a restore
 *  fn, so persistence is testable without a browser. */
function stubStorage(seed: Record<string, string> = {}): () => void {
  const map = new Map(Object.entries(seed));
  const prior = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    value: {
      getItem: (k: string) => map.get(k) ?? null,
      setItem: (k: string, v: string) => void map.set(k, v),
      removeItem: (k: string) => void map.delete(k),
    },
  });
  return () => {
    if (prior) {
      Object.defineProperty(globalThis, "localStorage", prior);
    } else {
      delete (globalThis as { localStorage?: unknown }).localStorage;
    }
  };
}

test("the default shows every state EXCEPT archived", () => {
  const f = defaultFilter();
  assert.equal(f.archived, false);
  for (const k of FILTER_KINDS) {
    if (k !== "archived") {
      assert.equal(f[k], true, `${k} must be shown by default`);
    }
  }
  // The sane default IS the default: it must not read as a filter to go undo.
  assert.equal(isDefaultFilter(f), true);
});

test("by default the rail shows the live states and hides only the archived row", () => {
  const shown = filterSessions(ALL, defaultFilter());
  assert.deepEqual(
    shown.map((s) => s.title),
    ["needs-you", "working", "waiting-limit", "broken"],
  );
  assert.equal(hiddenCount(ALL, defaultFilter()), 1);
});

test("checking Archived reveals the archived row without disturbing the rest", () => {
  const f = withKind(defaultFilter(), "archived", true);
  assert.equal(
    filterSessions(ALL, f).length,
    ALL.length,
    "every state checked ⇒ every row shows",
  );
  assert.equal(hiddenCount(ALL, f), 0);
  assert.equal(isDefaultFilter(f), false, "revealing the archive is a narrowing to indicate");
});

test("unchecking one state hides exactly that group", () => {
  const f = withKind(defaultFilter(), "working", false);
  assert.deepEqual(
    filterSessions(ALL, f).map((s) => s.title),
    ["needs-you", "waiting-limit", "broken"],
  );
  // withKind is pure — the caller's filter is untouched (the store swaps references).
  assert.equal(defaultFilter().working, true);
});

test("narrowing to a single state shows only it (the only-working case)", () => {
  const only: StatusFilter = {
    "needs-you": false,
    working: true,
    "waiting-limit": false,
    broken: false,
    archived: false,
  };
  assert.deepEqual(
    filterSessions(ALL, only).map((s) => s.title),
    ["working"],
  );
});

test("an all-off filter empties the rail rather than falling back to showing everything", () => {
  // The empty-state copy depends on this being an honest zero: renderRail reports
  // "no sessions match" instead of quietly ignoring the user's filter.
  const none: StatusFilter = {
    "needs-you": false,
    working: false,
    "waiting-limit": false,
    broken: false,
    archived: false,
  };
  assert.deepEqual(filterSessions(ALL, none), []);
  assert.equal(hiddenCount(ALL, none), ALL.length);
});

test("the filter partitions by the DISPLAYED status: an in-flight op filters as Working", () => {
  // A session being archived is Ready underneath but RENDERS as working (#1766, no
  // dot). Unchecking Working must hide it — filtering on the raw liveness instead
  // would leave a visible dotless row behind and hide it under "Ready" instead.
  const archiving = sess({ title: "archiving", liveness: Liveness.Ready, in_flight_op: InFlightOp.Archiving });
  const list = [archiving];
  assert.deepEqual(filterSessions(list, withKind(defaultFilter(), "working", false)), []);
  assert.deepEqual(
    filterSessions(list, withKind(defaultFilter(), "needs-you", false)).map((s) => s.title),
    ["archiving"],
    "it is NOT governed by the Ready checkbox",
  );
});

test("a session ARCHIVING is not yet archived — the default keeps it visible", () => {
  // It still reads as working (the [deleting] row the user is watching), so hiding it
  // by default would make the row vanish the instant the archive is issued.
  const archiving = sess({ title: "archiving", liveness: Liveness.Ready, in_flight_op: InFlightOp.Archiving });
  assert.deepEqual(
    filterSessions([archiving], defaultFilter()).map((s) => s.title),
    ["archiving"],
  );
});

test("the legacy status fallback is filtered too (a pre-#1195 archived record)", () => {
  const legacy = sess({ title: "legacy", status: Status.Archived });
  assert.deepEqual(filterSessions([legacy], defaultFilter()), []);
  assert.deepEqual(
    filterSessions([legacy], withKind(defaultFilter(), "archived", true)).map((s) => s.title),
    ["legacy"],
  );
});

test("kindCounts reports every kind, zeros included, for the menu's glance", () => {
  const counts = kindCounts([ROWS.archived, ROWS.archived, ROWS["needs-you"]]);
  assert.equal(counts.archived, 2);
  assert.equal(counts["needs-you"], 1);
  assert.equal(counts.broken, 0, "a kind with no sessions still reports 0, so the menu is stable");
  for (const k of FILTER_KINDS) {
    assert.equal(typeof counts[k], "number");
  }
});

test("filter labels are the row's own operator words — the two surfaces cannot drift", () => {
  for (const k of FILTER_KINDS) {
    assert.equal(filterLabel(k), OPERATOR_KIND_LABELS[k]);
  }
  assert.equal(filterLabel("waiting-limit"), "Waiting on a limit", "sentence case, per the copy convention");
});

test("a filter round-trips through localStorage", () => {
  const restore = stubStorage();
  try {
    const f = withKind(withKind(defaultFilter(), "archived", true), "broken", false);
    persistFilter(f);
    assert.deepEqual(loadFilter(), f);
  } finally {
    restore();
  }
});

test("a stored filter missing a state defaults it to SHOWN, never silently hidden", () => {
  // The forward-compat rule: a browser holding a filter written before a state
  // existed must inherit that state's default, not lose the group. Only `archived`
  // was stored here — every other state (including a hypothetical future one) shows.
  const restore = stubStorage({ "af-status-filter": JSON.stringify({ archived: true }) });
  try {
    const f = loadFilter();
    assert.equal(f.archived, true, "the stored choice is honored");
    for (const k of FILTER_KINDS) {
      if (k !== "archived") {
        assert.equal(f[k], true, `${k} was not stored ⇒ defaults to shown`);
      }
    }
  } finally {
    restore();
  }
});

test("corrupt / hostile stored values fall back to the default instead of blanking the rail", () => {
  for (const raw of ["not json", "null", '"a string"', "[]", "42"]) {
    const restore = stubStorage({ "af-status-filter": raw });
    try {
      assert.deepEqual(loadFilter(), defaultFilter(), `${raw} ⇒ default`);
    } finally {
      restore();
    }
  }
});

test("the pre-grouping filter migrates without silently hiding a combined group", () => {
  const restore = stubStorage({
    "af-status-filter": JSON.stringify({ working: false, ready: false, lost: false, dead: true, limit: false, archived: true }),
  });
  try {
    const f = loadFilter();
    assert.equal(f.working, false);
    assert.equal(f["needs-you"], false);
    assert.equal(f["waiting-limit"], false);
    assert.equal(f.broken, true, "either old degraded bucket shown keeps the combined Broken group shown");
    assert.equal(f.archived, true);
  } finally {
    restore();
  }
});

test("the pre-grouping Ready choice keeps newly Broken ready sessions visible", () => {
  const restore = stubStorage({
    "af-status-filter": JSON.stringify({ working: true, ready: true, lost: false, dead: false, limit: true, archived: false }),
  });
  try {
    const f = loadFilter();
    assert.equal(
      f.broken,
      true,
      "Ready previously included prompt-not-delivered, so its visible choice must carry into Broken",
    );
  } finally {
    restore();
  }
});

test("non-boolean flags are ignored, so a truthy 'false' can't hide a group", () => {
  const restore = stubStorage({
    "af-status-filter": JSON.stringify({ working: "false", "needs-you": 0, broken: null, archived: true }),
  });
  try {
    const f = loadFilter();
    assert.equal(f.working, true, "the string 'false' is not a boolean ⇒ default wins");
    assert.equal(f["needs-you"], true);
    assert.equal(f.broken, true);
    assert.equal(f.archived, true);
  } finally {
    restore();
  }
});

test("filter persistence never throws when storage is unavailable (private mode)", () => {
  const prior = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    get() {
      throw new Error("storage blocked");
    },
  });
  try {
    assert.deepEqual(loadFilter(), defaultFilter());
    assert.doesNotThrow(() => persistFilter(defaultFilter()));
  } finally {
    if (prior) {
      Object.defineProperty(globalThis, "localStorage", prior);
    } else {
      delete (globalThis as { localStorage?: unknown }).localStorage;
    }
  }
});
