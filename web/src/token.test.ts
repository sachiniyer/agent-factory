// Tests for bearer-token PERSISTENCE (feat: log in once). The property: a token
// that logged in successfully survives a new tab / a browser restart, so the web UI
// stops asking for `af token show` on every visit — and stops being trusted the
// moment the daemon says it is bad.
//
// These pin the storage layer and the forget/keep decision. The end-to-end proof
// (a second page in the same browser lands straight in the authed shell) lives in
// the Playwright selftest, which is the only place a real localStorage and a real
// boot sequence exist together.

import { test, afterEach } from "node:test";
import assert from "node:assert/strict";

import { ApiError, clearToken, loadToken, shouldForgetToken, storeToken } from "./api.js";

type StorageMap = Map<string, string>;

interface Stubbed {
  local: StorageMap;
  session: StorageMap;
  restore: () => void;
}

/** Installs in-memory local/sessionStorage (node has no DOM), optionally seeded, and
 *  returns the backing maps plus a restore fn. `throwing` simulates a blocked store
 *  (private mode / a hardened profile), which every helper must survive. */
function stubStorage(seed: { local?: Record<string, string>; session?: Record<string, string> } = {}, throwing = false): Stubbed {
  const local: StorageMap = new Map(Object.entries(seed.local ?? {}));
  const session: StorageMap = new Map(Object.entries(seed.session ?? {}));
  const priors = (["localStorage", "sessionStorage"] as const).map((name) => ({
    name,
    prior: Object.getOwnPropertyDescriptor(globalThis, name),
  }));
  const install = (name: "localStorage" | "sessionStorage", map: StorageMap): void => {
    Object.defineProperty(globalThis, name, {
      configurable: true,
      value: {
        getItem: (k: string): string | null => {
          if (throwing) {
            throw new Error("storage blocked");
          }
          return map.get(k) ?? null;
        },
        setItem: (k: string, v: string): void => {
          if (throwing) {
            throw new Error("storage blocked");
          }
          map.set(k, v);
        },
        removeItem: (k: string): void => {
          if (throwing) {
            throw new Error("storage blocked");
          }
          map.delete(k);
        },
      },
    });
  };
  install("localStorage", local);
  install("sessionStorage", session);
  return {
    local,
    session,
    restore: () => {
      for (const { name, prior } of priors) {
        if (prior) {
          Object.defineProperty(globalThis, name, prior);
        } else {
          delete (globalThis as Record<string, unknown>)[name];
        }
      }
    },
  };
}

let stub: Stubbed | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

test("a token round-trips through localStorage, so a NEW TAB resumes it", () => {
  stub = stubStorage();
  storeToken("tok-abc");
  // localStorage, not sessionStorage: sessionStorage is scoped to the one tab that
  // wrote it, which is exactly the re-paste-every-visit behavior this replaces.
  assert.equal(stub.local.get("af.token"), "tok-abc");
  assert.equal(stub.session.size, 0, "the token must not be written to per-tab storage");
  assert.equal(loadToken(), "tok-abc");
});

test("no stored token reads as null, not an empty credential", () => {
  stub = stubStorage();
  assert.equal(loadToken(), null);
});

test("an empty stored value reads as null — it must never pose as a valid token", () => {
  // "" is the tokenless sentinel (#1696) inside the app; read back out of storage it
  // would silently claim "this client needs no credential" and skip the login.
  stub = stubStorage({ local: { "af.token": "" } });
  assert.equal(loadToken(), null);
});

test("the tokenless sentinel is never stored", () => {
  stub = stubStorage();
  storeToken("");
  assert.equal(stub.local.size, 0, "a no-credential connection must leave nothing behind");
  assert.equal(loadToken(), null);
});

test("clearToken forgets the token, including a legacy per-tab copy", () => {
  stub = stubStorage({ local: { "af.token": "tok-abc" }, session: { "af.token": "tok-legacy" } });
  clearToken();
  assert.equal(loadToken(), null);
  assert.equal(stub.local.size, 0);
  assert.equal(stub.session.size, 0, "a token from a pre-persistence build must not outlive a logout");
});

test("blocked storage degrades to no persistence, never a throw", () => {
  // Private mode / a hardened profile: every helper is best-effort. A throw here
  // would happen at BOOT (bootstrap reads the token first thing) and take the whole
  // app down rather than showing a login form.
  stub = stubStorage({}, true);
  assert.doesNotThrow(() => storeToken("tok-abc"));
  assert.doesNotThrow(() => clearToken());
  assert.equal(loadToken(), null);
});

test("a rejected credential (401/403) is forgotten", () => {
  assert.equal(shouldForgetToken(new ApiError(401, "unauthorized")), true);
  assert.equal(shouldForgetToken(new ApiError(403, "forbidden")), true);
});

test("a transport failure KEEPS the token — the daemon being down says nothing about it", () => {
  // The regression this guards: clearing on any failure means every daemon restart
  // (or asleep laptop, or wrong host typed once) deletes a good credential and puts
  // the paste form back — re-creating the complaint persistence exists to fix.
  assert.equal(shouldForgetToken(new ApiError(0, "cannot reach the daemon")), false);
  assert.equal(shouldForgetToken(new ApiError(500, "internal error")), false);
  assert.equal(shouldForgetToken(new ApiError(502, "bad gateway")), false);
  assert.equal(shouldForgetToken(new TypeError("network error")), false);
  assert.equal(shouldForgetToken("boom"), false);
  assert.equal(shouldForgetToken(undefined), false);
});
