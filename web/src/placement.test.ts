// W6.4: web placement text mirrors Go session.PlacementData.Short/Detail.
import { test } from "node:test";
import assert from "node:assert/strict";

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
