// Tests for the create/task forms' agent-program choice (#1970).
//
// The web used to hardcode the agent list in two pickers while the daemon owned
// the canonical one. The lists matched, so nothing was broken — the COUPLING was:
// add an agent server-side and the web silently keeps offering the old set, with
// every test green. These pin the two properties that make the served version
// correct: the web renders whatever the daemon lists (and nothing it knows itself),
// and "no choice" stays genuinely absent so the repo default still applies.

import { test } from "node:test";
import assert from "node:assert/strict";

import { PROGRAM_REPO_DEFAULT, type ProgramCatalog, programChoices } from "./programs.js";

/** A catalog in the daemon's own shape: canonical (append-only) order, plus the
 *  program an unspecified create resolves to. */
function catalog(over: Partial<ProgramCatalog> = {}): ProgramCatalog {
  return {
    default: "claude",
    programs: [
      { name: "claude" },
      { name: "codex" },
      { name: "aider" },
      { name: "gemini" },
      { name: "amp" },
      { name: "opencode" },
    ],
    ...over,
  };
}

test("the picker offers every program the daemon lists, in the daemon's order", () => {
  const choices = programChoices(catalog());

  assert.deepEqual(
    choices.map((c) => c.value),
    [PROGRAM_REPO_DEFAULT, "claude", "codex", "aider", "gemini", "amp", "opencode"],
    "repo default leads, then the daemon's list verbatim",
  );
});

// THE anti-drift test, and the point of the design (#1970) — the web twin of
// daemon/programs_test.go's TestListPrograms_NewProgramReachesClientsWithNoClientChange.
//
// An agent added server-side must reach the web with no web change: no enum to
// extend, no label map to teach, no `if (name === …)`. This simulates exactly that:
// a daemon that lists an agent this file has never heard of.
//
// If someone "helpfully" adds a local name→label map or an allow-list, this fails.
test("an agent the web has never heard of is offered without a web change", () => {
  const withNewAgent = catalog({
    programs: [{ name: "claude" }, { name: "cursor" }],
  });

  const choices = programChoices(withNewAgent);

  const cursor = choices.find((c) => c.value === "cursor");
  assert.ok(cursor, "a newly supported agent must appear with no change to the web");
  assert.equal(cursor.label, "cursor", "its label comes from the daemon's name, not a local map that would render it blank");
});

test("the repo default is labelled with the program it resolves to", () => {
  const choices = programChoices(catalog({ default: "codex" }));

  assert.equal(choices[0].value, PROGRAM_REPO_DEFAULT);
  assert.equal(choices[0].label, "Repo default (codex)", "a user can see the repo defaults to codex without picking codex");
});

// The honesty case. An empty `default` means the daemon had no default to report,
// so naming one here would invent a choice nobody made — the same failure the
// served enum exists to prevent, one field over.
test("a daemon that reports no default is not given one by the web", () => {
  const choices = programChoices(catalog({ default: "" }));

  assert.equal(choices[0].label, "Repo default", "no parenthetical, rather than a guessed agent name");
});

// The sentinel must stay the empty string: it is what the create/task requests
// already send to mean "unspecified", so picking the default sends no program and
// the repo's config decides. A non-empty sentinel would eventually be transmitted
// as a literal program name and silently override the repo default.
test("the repo-default choice submits as absent, not as a program name", () => {
  assert.equal(PROGRAM_REPO_DEFAULT, "");
  assert.equal(programChoices(catalog())[0].value, "");
});

// An unreachable catalog costs the user the choice, never the session: "repo
// default" alone is exactly the behavior of sending no program at all.
test("an unavailable catalog degrades to the repo default alone", () => {
  const choices = programChoices(null);

  assert.deepEqual(choices.map((c) => c.value), [PROGRAM_REPO_DEFAULT]);
  assert.equal(choices[0].label, "Repo default");
});

// The task editor seeds its picker from the stored task. A program the catalog does
// not list — one set via `af tasks add --program`, or one retired since — must
// survive, or opening the editor to change a prompt silently rewrites the task's
// agent to the repo default. That is data loss wearing the shape of a form.
test("a seeded program the catalog does not list is preserved as a choice", () => {
  const choices = programChoices(catalog(), "my-custom-agent");

  const kept = choices.find((c) => c.value === "my-custom-agent");
  assert.ok(kept, "an unlisted seeded program must remain selectable");
  assert.equal(kept.label, "my-custom-agent");
  assert.equal(choices[choices.length - 1].value, "my-custom-agent", "appended last, so it never displaces a real choice");
});

test("a seeded program the catalog already lists is not duplicated", () => {
  const choices = programChoices(catalog(), "codex");

  assert.equal(choices.filter((c) => c.value === "codex").length, 1);
});

// The seed is empty for a task that never set a program (it uses the repo default),
// and must not become a blank option next to the real one.
test("an empty or whitespace seed adds no choice", () => {
  assert.deepEqual(programChoices(catalog(), "").map((c) => c.value), programChoices(catalog()).map((c) => c.value));
  assert.deepEqual(programChoices(catalog(), "   ").map((c) => c.value), programChoices(catalog()).map((c) => c.value));
});

// Degrading and seeding compose: a catalog fetch that fails while editing a task
// must still show that task's program, not silently reset it.
test("a seeded program survives an unavailable catalog", () => {
  const choices = programChoices(null, "aider");

  assert.deepEqual(choices.map((c) => c.value), [PROGRAM_REPO_DEFAULT, "aider"]);
});
