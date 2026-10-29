// W6.4: web placement text mirrors Go session.PlacementData.Short/Detail.
import { test } from "node:test";
import assert from "node:assert/strict";

import { applyTerminalPlacement, applyTerminalTitle } from "./components.js";
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

// #6038 / #2224 / #3981 / #2458: the head's title element owns the title alone
// (text, tooltip and aria-label are all the full title, so a truncated title stays
// readable), and where the session runs has its OWN element and tooltip.
type El = { textContent: string; title: string; hidden: boolean; attrs: Record<string, string>; setAttribute(k: string, v: string): void };
const el = (): El => ({ textContent: "", title: "", hidden: false, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; } });

test("the title element keeps the bare title through every patch", () => {
  const title = el();
  applyTerminalTitle(title, "probe-a");
  assert.equal(title.textContent, "probe-a");
  assert.equal(title.title, "probe-a");
  applyTerminalTitle(title, "title-row-one-375-light-with-a-useful-distinguishing-suffix"); // rename patch
  assert.equal(title.title, "title-row-one-375-light-with-a-useful-distinguishing-suffix");
  assert.equal(title.attrs["aria-label"], title.title);
});

test("placement is its own element: shown with its own tooltip for an off-box session, hidden for local", () => {
  const chip = el();
  const remote = s({ kind: "remote", host: "box1", backend: "ssh", location: "/srv/s1/workspace" });
  applyTerminalPlacement(chip, placementDetail(remote), true); // initial render
  assert.equal(chip.hidden, false);
  assert.equal(chip.textContent, "box1 · ssh · /srv/s1/workspace");
  assert.equal(chip.title, "Where this session runs: box1 · ssh · /srv/s1/workspace");

  applyTerminalPlacement(chip, placementDetail(s({ kind: "remote", host: "box1", backend: "ssh", location: "/srv/s2/workspace" })), true); // patch
  assert.equal(chip.textContent, "box1 · ssh · /srv/s2/workspace");

  const local = s({ kind: "local", location: "/w" });
  applyTerminalPlacement(chip, placementDetail(local), false); // a local session says nothing extra
  assert.equal(chip.hidden, true);
  assert.equal(chip.textContent, "");
  assert.equal(chip.title, "");

  applyTerminalPlacement(chip, placementDetail(s()), false); // a daemon that projects none
  assert.equal(chip.hidden, true);
});
