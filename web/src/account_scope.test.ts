// Tests for the create form's credential-account choice (#3844). The daemon has
// accepted `account` on create since #3051 and the web never sent it, so a session
// could only be scoped to an identity from the CLI — while the web's own Config
// view was already the place people registered and logged into those accounts.
//
// These pin the properties that make the web half correct: it renders whatever the
// daemon lists (and nothing it knows itself), the list follows the form's PROGRAM
// because an account belongs to one agent, the two unusable shapes stay distinct
// (one blocks, one only informs), "no choice" stays genuinely absent, and the
// account the daemon reports BACK is checked against the one that was picked.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  AMBIENT_ACCOUNT,
  accountAgentFor,
  accountAgentSupported,
  accountChoices,
  accountNotice,
  accountSelectable,
  accountSkewMessage,
} from "./account_scope.js";
import type { ProgramCatalog } from "./programs.js";
import type { AccountsResponse, SessionData } from "./types.js";

/** A registry in the daemon's own shape: two agents, one account never logged
 *  into, and the SAME NAME under two agents — which is the collision the whole
 *  agent-scoping rule exists for. */
function registry(over: Partial<AccountsResponse> = {}): AccountsResponse {
  return {
    entries: [
      { agent: "claude", name: "personal", dir: "/h/accounts/claude/personal", registration_only: false, logged_in: true },
      { agent: "claude", name: "work", dir: "/h/accounts/claude/work", registration_only: false, logged_in: true },
      { agent: "codex", name: "work", dir: "/h/accounts/codex/work", registration_only: false, logged_in: true },
    ],
    agents: ["claude", "codex", "gemini"],
    ...over,
  };
}

function session(over: Partial<SessionData> = {}): SessionData {
  return { title: "scoped", branch: "af/scoped", ...over };
}

test("the picker offers the agent's accounts, in the daemon's order, behind the ambient row", () => {
  const choices = accountChoices(registry(), "claude");

  assert.deepEqual(
    choices.map((c) => c.value),
    [AMBIENT_ACCOUNT, "personal", "work"],
    "the ambient identity leads, then the daemon's entries verbatim",
  );
});

// THE constraint-1 test. An account belongs to ONE agent: claude's "work" and
// codex's "work" are different identities in different registries, so offering the
// wrong one is offering a guaranteed failure — and the failure it invites is the
// identity kind, a session running somewhere the user did not choose.
test("the list follows the agent: a codex account is never offered to a claude session", () => {
  const forClaude = accountChoices(registry(), "claude");
  const forCodex = accountChoices(registry(), "codex");

  assert.deepEqual(forClaude.map((c) => c.value), [AMBIENT_ACCOUNT, "personal", "work"]);
  assert.deepEqual(forCodex.map((c) => c.value), [AMBIENT_ACCOUNT, "work"]);
  assert.ok(
    forClaude.every((c) => c.agent === "claude"),
    "every offered row must belong to the agent asked for",
  );
  assert.ok(
    forCodex.every((c) => c.agent === "codex"),
    "and the same name under another agent is a different row entirely",
  );
});

// The anti-drift test, and the point of the design: an account registered on the
// daemon host a second ago reaches the web with no web change — no list to extend,
// no label map to teach. This simulates exactly that.
test("an account name this file has never heard of is offered and sent verbatim", () => {
  const choices = accountChoices(
    registry({
      entries: [
        { agent: "claude", name: "moonbase-oncall", dir: "/h/a/c/moonbase-oncall", registration_only: false, logged_in: true },
      ],
    }),
    "claude",
  );

  assert.deepEqual(choices.map((c) => c.value), [AMBIENT_ACCOUNT, "moonbase-oncall"]);
  assert.equal(choices[1].label, "moonbase-oncall", "the label is the daemon's name, not a lookup");
});

// Constraint 2. A registration-only account is LISTED with its reason — hiding it
// would leave a user re-reading the Config view for an account that is right there
// — and it BLOCKS the submit, because the create would refuse it.
//
// The fixture marks a CLAUDE account registration-only, which this build would not
// derive on its own: the daemon is the authority on that state, since it is
// computed by the build that owns the registry and may be newer than the client.
test("a registration-only account is listed, marked, and blocks the submit", () => {
  const choices = accountChoices(
    registry({
      entries: [
        { agent: "claude", name: "unproven", dir: "/h/a/c/unproven", registration_only: true, logged_in: true },
      ],
      agents: ["claude"],
    }),
    "claude",
  );

  assert.equal(choices[1].label, "unproven — registration only", "the row says so before any click");
  assert.equal(accountSelectable(choices, "unproven"), false, "a create that would be refused must not be offered");
  assert.match(accountNotice(choices, "unproven"), /cannot be scoped to a claude account yet/);
});

// Constraint 3, and the half that a "listed but greyed out" design would get
// wrong. `logged_in` is a stat of the agent's OWN credential file, so false means
// "no credential in this directory yet" — not "broken". It is the most likely
// account to be picked right after the Config view created it.
test("a not-logged-in account is listed, labelled, and still selectable", () => {
  const choices = accountChoices(
    registry({
      entries: [
        { agent: "claude", name: "just-registered", dir: "/h/a/c/just-registered", registration_only: false, logged_in: false },
      ],
      agents: ["claude"],
    }),
    "claude",
  );

  assert.equal(choices[1].label, "just-registered — not logged in");
  assert.equal(accountSelectable(choices, "just-registered"), true, "nothing about it would fail a create");
  assert.match(accountNotice(choices, "just-registered"), /no claude credential yet/);
});

test("both states at once join with the repo's separator, and the blocking reason wins", () => {
  const choices = accountChoices(
    registry({
      entries: [
        { agent: "claude", name: "neither", dir: "/h/a/c/neither", registration_only: true, logged_in: false },
      ],
      agents: ["claude"],
    }),
    "claude",
  );

  assert.equal(choices[1].label, "neither — registration only · not logged in");
  assert.match(
    accountNotice(choices, "neither"),
    /cannot be scoped/,
    "a user whose submit is disabled must be told why before anything else",
  );
});

test("the ambient row is always offered, always selectable, and says nothing", () => {
  for (const choices of [accountChoices(null, ""), accountChoices(registry(), "claude"), accountChoices(registry(), "gemini")]) {
    assert.equal(choices[0].value, AMBIENT_ACCOUNT, "the pre-#3844 default is never taken away");
    assert.equal(accountSelectable(choices, AMBIENT_ACCOUNT), true);
    assert.equal(accountNotice(choices, AMBIENT_ACCOUNT), "");
  }
});

// The sentinel. AMBIENT_ACCOUNT must stay the empty string: it is what createSession
// omits on, so an ambient pick sends no account at all. A non-empty sentinel would
// eventually be transmitted as a literal account name — and an account name that
// does not exist is a refused create at best.
test("the ambient sentinel is the empty string, which is what the request omits on", () => {
  assert.equal(AMBIENT_ACCOUNT, "");
});

test("the agent comes from the program, and a repo-default program resolves through the catalog", () => {
  const catalog: ProgramCatalog = { programs: [{ name: "claude" }, { name: "codex" }], default: "codex" };

  assert.equal(accountAgentFor("claude", catalog), "claude", "an explicit program names its own agent");
  assert.equal(accountAgentFor("", catalog), "codex", "the repo default resolves through the daemon's answer");
  assert.equal(accountAgentFor("", null), "", "and with no catalog there is nothing to resolve");
  assert.equal(accountAgentFor("", { programs: [], default: "" }), "", "nor when the daemon reported no default");
});

// An agent outside the daemon's roster has no account registry at all, which is a
// fact about the agent rather than a failure. The roster is the daemon's own, and
// always the FULL one, so this answer stays right the day a fourth agent is
// verified — with no change here.
test("an agent the daemon's roster does not cover has no accounts to offer", () => {
  assert.equal(accountAgentSupported(registry(), "aider"), false);
  assert.equal(accountAgentSupported(registry(), "gemini"), true, "rostered with no entries is still rostered");
  assert.equal(accountAgentSupported(registry(), ""), false, "an unresolvable program resolves to no registry");
  assert.equal(accountAgentSupported(null, "claude"), false, "a registry that never loaded offers nothing");
});

// Constraint 5, the version skew this feature has to guard. A daemon predating
// account support drops the field silently, so the session runs on the ambient
// identity while the UI reports the account the user picked.
test("an account the daemon did not apply is reported, naming both identities", () => {
  const msg = accountSkewMessage("work", session({ title: "skewed" }));

  assert.match(msg, /"work"/, "the account that was asked for");
  assert.match(msg, /ambient identity/, "and the identity the session is actually running on");
  assert.match(msg, /"skewed"/, "and the session, which now exists and must be removed");
  assert.match(msg, /predates account support/, "and what to do about it");
});

test("an account applied as a DIFFERENT one is reported too, and not as version skew", () => {
  const msg = accountSkewMessage("work", session({ account: "personal" }));

  assert.match(msg, /"personal"/, "what it is actually running as");
  assert.match(msg, /"work"/, "and what was picked");
  assert.doesNotMatch(msg, /predates account support/, "the daemon knew the field — an upgrade is the wrong fix");
});

// The control, and it is not a formality: a check that fired on every scoped create
// would be worse than no check, because it trains the user to ignore the real one.
test("an applied account raises nothing, and neither does an ambient create", () => {
  assert.equal(accountSkewMessage("work", session({ account: "work" })), "");
  assert.equal(accountSkewMessage(AMBIENT_ACCOUNT, session()), "", "no account was asked for, so there is nothing to check");
  assert.equal(
    accountSkewMessage(AMBIENT_ACCOUNT, session({ account: "work" })),
    "",
    "a daemon that volunteered an account nobody asked for is not this check's business",
  );
});
