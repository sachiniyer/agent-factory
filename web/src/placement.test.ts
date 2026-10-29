// W6.4: web placement text mirrors Go session.PlacementData.Short/Detail.
import { test } from "node:test";
import assert from "node:assert/strict";

import { applyTerminalTitle } from "./components.js";
import { placementDetail, placementShort } from "./status.js";
import type { SessionData } from "./types.js";

const s = (placement?: SessionData["placement"]): SessionData => ({ title: "t", branch: "b", placement });

test("a local or unprojected session adds nothing to the rail", () => {
  assert.equal(placementShort(s()), "");
  assert.equal(placementShort(s({ kind: "local", location: "/w" })), "");
  assert.equal(placementDetail(s()), "");
  assert.equal(placementDetail(s({ kind: "local", location: "/w" })), "local · /w");
});

test("an off-box session names its host, else its runtime", () => {
  assert.equal(placementShort(s({ kind: "remote", host: "box1", backend: "ssh" })), "box1");
  assert.equal(placementShort(s({ kind: "remote", backend: "docker" })), "docker");
  assert.equal(
    placementDetail(s({ kind: "remote", host: "box1", backend: "ssh", location: "/srv/s1/workspace" })),
    "box1 · ssh · /srv/s1/workspace",
  );
  assert.equal(placementDetail(s({ kind: "remote", backend: "docker" })), "docker");
});

// #6038: the head's title element is written in one place, so a patch (rename,
// status change, tab switch) cannot clobber the placement tooltip, and #2458's
// visible head text stays the title alone.
test("every head write keeps the placement tooltip and the title-only text", () => {
  const el = { textContent: "", title: "", attrs: {} as Record<string, string>, setAttribute(k: string, v: string) { this.attrs[k] = v; } };
  const session = s({ kind: "remote", host: "box1", backend: "ssh", location: "/srv/s1/workspace" });
  const tooltip = (title: string) => `${title} · box1 · ssh · /srv/s1/workspace`;

  applyTerminalTitle(el, "probe-a", placementDetail(session)); // initial render
  assert.equal(el.textContent, "probe-a");
  assert.equal(el.title, tooltip("probe-a"));

  applyTerminalTitle(el, "probe-b", placementDetail(session)); // rename patch
  assert.equal(el.textContent, "probe-b", "visible head text is the title alone (#2458)");
  assert.equal(el.title, tooltip("probe-b"));
  assert.equal(el.attrs["aria-label"], "probe-b");

  applyTerminalTitle(el, "probe-b", placementDetail(s({ kind: "remote", host: "box1", backend: "ssh", location: "/srv/s2/workspace" }))); // status/tab patch with new data
  assert.equal(el.title, "probe-b · box1 · ssh · /srv/s2/workspace");

  applyTerminalTitle(el, "plain", ""); // a daemon that projects no placement
  assert.equal(el.title, "plain");
  assert.equal(el.textContent, "plain");
});
