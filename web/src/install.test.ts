// Unit coverage for the install affordance's visibility rule (feat: PWA).
//
// shouldShowInstall is the whole of the "don't lie to the user, don't nag" contract,
// so it is pure and pinned here. The DOM half — the button actually appearing,
// prompting, and dismissing — needs a real Chromium that fires beforeinstallprompt
// and lives in the Playwright selftest instead.

import { test } from "node:test";
import assert from "node:assert/strict";

import { shouldShowInstall, type InstallVisibility } from "./install.js";

function vis(over: Partial<InstallVisibility> = {}): InstallVisibility {
  return { stashed: true, dismissed: false, installed: false, ...over };
}

test("shows only once the browser has actually offered an install", () => {
  assert.equal(shouldShowInstall(vis()), true);
});

test("stays hidden until a beforeinstallprompt fires", () => {
  // This is the case that covers an INSECURE CONTEXT — the plain-HTTP Tailscale
  // address, where Chrome never fires the event. No stashed event, no button, which
  // is correct: there is no way to install from there, so an install button could
  // only lie. Same code path hides it for an unsupported browser and a non-installable
  // page, which is why none of those need their own check.
  assert.equal(shouldShowInstall(vis({ stashed: false })), false);
});

test("a dismissal wins over an offer — the affordance must never nag", () => {
  assert.equal(shouldShowInstall(vis({ dismissed: true })), false);
});

test("stays hidden once installed, even if an offer is somehow still stashed", () => {
  assert.equal(shouldShowInstall(vis({ installed: true })), false);
});

test("nothing shows when every reason to hide applies at once", () => {
  assert.equal(shouldShowInstall({ stashed: false, dismissed: true, installed: true }), false);
});
