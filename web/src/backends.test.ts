// Tests for the create form's backend choice (#1933). The daemon has always
// accepted `backend` on create; the web never sent it, so remote sessions could
// only be started from the TUI/CLI. These pin the two properties that make the web
// half correct: it renders whatever the daemon lists (and nothing it knows itself),
// and "no choice" stays genuinely absent so the repo default still applies.

import { test } from "node:test";
import assert from "node:assert/strict";

import { REPO_DEFAULT, type BackendCatalog, backendChoices, backendNotice, backendSelectable } from "./backends.js";

/** A catalog in the daemon's own shape: canonical order, availability per backend. */
function catalog(over: Partial<BackendCatalog> = {}): BackendCatalog {
  return {
    default: "local",
    default_status: "available",
    backends: [
      { name: "local", status: "available" },
      { name: "docker", status: "unavailable", reason: "backend=docker requires docker.image to be set in this repo's .agent-factory/config.json" },
      { name: "ssh", status: "unavailable", reason: "backend=ssh requires ssh.host to be set in this repo's .agent-factory/config.json" },
      { name: "hook", status: "unavailable", reason: "backend=hook requires remote_hooks to be configured" },
    ],
    ...over,
  };
}

test("the picker offers every backend the daemon lists, in the daemon's order", () => {
  const choices = backendChoices(catalog());

  assert.deepEqual(
    choices.map((c) => c.value),
    [REPO_DEFAULT, "local", "docker", "ssh", "hook"],
    "repo default leads, then the daemon's list verbatim",
  );
});

// THE anti-drift test, and the point of the design (#1933). A backend added
// server-side must reach the web with no web change — no enum to extend, no label
// map to teach, no `if (name === …)`. This simulates exactly that: a daemon that
// lists a backend this file has never heard of.
//
// If someone "helpfully" adds a local name→label map or an allow-list, this fails.
test("a backend the web has never heard of is offered without a web change", () => {
  const withNewBackend = catalog({
    backends: [
      { name: "local", status: "available" },
      { name: "fargate", status: "available" },
    ],
  });

  const choices = backendChoices(withNewBackend);

  const fargate = choices.find((c) => c.value === "fargate");
  assert.ok(fargate, "a newly supported backend must appear with no change to the web");
  assert.equal(fargate.label, "fargate", "its label comes from the daemon's name, not a local map that would render it blank");
  assert.equal(fargate.status, "available");
  assert.equal(fargate.reason, "");
});

test("an unconfigured backend is offered, explained, and blocked from submitting", () => {
  const choices = backendChoices(catalog());

  const docker = choices.find((c) => c.value === "docker");
  assert.ok(docker);
  assert.equal(docker.status, "unavailable", "creating with it would fail, so the form must not submit it");
  assert.match(docker.reason, /docker\.image/, "the reason names the missing key, so it is actionable");

  // Selectable on purpose: a disabled <option> would hide the reason, leaving the
  // user with a greyed-out "docker" and no idea that one config key unlocks it.
  assert.equal(backendSelectable(choices, "docker"), false, "the submit is what gets blocked, not the choice");
  assert.match(backendNotice(choices, "docker"), /docker\.image/, "and picking it explains exactly why");
});

test("a configured backend is selectable with nothing to explain", () => {
  const choices = backendChoices(
    catalog({ backends: [{ name: "docker", status: "available" }] }),
  );

  const docker = choices.find((c) => c.value === "docker");
  assert.ok(docker);
  assert.equal(docker.status, "available");
  assert.equal(docker.reason, "");
  assert.equal(backendSelectable(choices, "docker"), true);
});

test("the repo default is labelled with the backend it resolves to", () => {
  const choices = backendChoices(catalog({ default: "docker", backends: [{ name: "docker", status: "available" }] }));

  assert.equal(choices[0].value, REPO_DEFAULT, "the default choice always sends nothing");
  assert.equal(choices[0].label, "Repo default (docker)", "a repo that defaults to docker says so, so the user need not pick docker to get it");
});

test("a repo whose declared default is unconfigured is explained, not silently broken", () => {
  // backend = "docker" with no docker.image. Leaving the choice alone still resolves
  // to docker and still fails, so the default is not a safe harbour here — naming the
  // problem while the user is looking at the form is the whole point of #1933.
  const choices = backendChoices(
    catalog({
      default: "docker",
      default_status: "unavailable",
      default_reason: "backend=docker requires docker.image to be set in this repo's .agent-factory/config.json",
    }),
  );

  assert.match(choices[0].reason, /docker\.image/, "the broken config is surfaced at choose time");
  assert.equal(
    backendSelectable(choices, REPO_DEFAULT),
    false,
    "this create resolves to the broken docker and WOULD fail, so blocking it beats letting it fail",
  );
});

// The tri-state's whole reason to exist: "I could not check" is a THIRD answer.
// Rendering unknown as usable would be a promise nobody verified — the picker
// telling the user "go ahead" about a backend it knows nothing about.
test("an unknown backend is never offered as usable, and says why", () => {
  const choices = backendChoices(
    catalog({
      backends: [
        { name: "local", status: "available" },
        { name: "docker", status: "unknown", reason: "cannot tell whether this repo can use backend=docker: its .agent-factory/config.toml could not be read (unexpected comma)" },
      ],
    }),
  );

  assert.equal(backendSelectable(choices, "docker"), false, "unknown must not be presented as a working choice");
  assert.match(backendNotice(choices, "docker"), /could not be read/, "and the user is told what stopped the check, not a made-up requirement");

  // Blocking unknown can never strand the user: local reads no repo config, so it
  // stays available through exactly the failure that makes the others unknown.
  assert.equal(backendSelectable(choices, "local"), true, "a session is still creatable while the config is broken");
});

test("a repo whose configured default is not a known backend reports NO default", () => {
  // The misconfiguration case: `backend = "bogus"`. There is no default to name —
  // that create fails rather than falling back to local — so the label must not
  // invent one, and the reason must carry the misconfiguration.
  const choices = backendChoices(
    catalog({
      default: "",
      default_status: "unavailable",
      default_reason: 'this repo\'s .agent-factory/config.json sets backend = "bogus", which is not a known backend (valid: local, docker, ssh, hook)',
    }),
  );

  assert.equal(choices[0].label, "Repo default", "no parenthetical: there is no resolved default to claim");
  assert.match(choices[0].reason, /"bogus"/, "the offending value is named");
  assert.equal(backendSelectable(choices, REPO_DEFAULT), false, "and the doomed default cannot be submitted");
});

test("an unrecognized selection is allowed through rather than vetoed locally", () => {
  // The daemon is the authority on what it accepts. A client that blocked a value it
  // merely does not recognize would be the hardcoded-enum bug (#1933) again.
  const choices = backendChoices(catalog());
  assert.equal(backendSelectable(choices, "fargate"), true);
});

test("the notice tracks the selected choice", () => {
  const choices = backendChoices(catalog());

  assert.equal(backendNotice(choices, REPO_DEFAULT), "", "a healthy default has nothing to say");
  assert.match(backendNotice(choices, "docker"), /docker\.image/, "selecting an unconfigured backend explains itself");
  assert.equal(backendNotice(choices, "local"), "", "a usable backend shows no notice");
  assert.equal(backendNotice(choices, "nonexistent"), "", "an unknown selection invents no message");
});

test("an unavailable catalog degrades to the repo default alone", () => {
  // The ListBackends call failing must cost the user the CHOICE, not the session:
  // "repo default" still creates exactly as the web did before this field existed.
  const choices = backendChoices(null);

  assert.deepEqual(choices.map((c) => c.value), [REPO_DEFAULT]);
  assert.equal(choices[0].label, "Repo default");
  assert.equal(backendSelectable(choices, REPO_DEFAULT), true, "a create must still be possible when the catalog is unknown");
});
