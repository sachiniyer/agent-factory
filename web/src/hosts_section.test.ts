// The Hosts section's rendered outcomes (#5571 W10.5): where a status line may
// live so it is never invisible. `doHostCheck("")` — the Check all button —
// records its failure under the empty name, which the add form claims when it
// renders. A check-all failure is NOT an add-form outcome, though: behind a
// closed disclosure, or where the capability gate leaves no form at all, the
// error would render nowhere and the button would look like it simply
// finished. The empty name therefore renders at the section foot whenever the
// open add form is not already showing it.

import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";

import { installFakeDom, type FakeElement } from "./fakedom.js";
import { renderHostsSection, type HostActions, type HostAddDraft, type HostStatus, type HostsState } from "./hosts.js";
import type { ConfigEntry, HostCheckResult } from "./types.js";

let dom: ReturnType<typeof installFakeDom>;

beforeEach(() => {
  dom = installFakeDom();
});

afterEach(() => {
  dom.restore();
});

const entry = (over: Partial<ConfigEntry> = {}): ConfigEntry => ({
  key: "default_program",
  type: "string",
  default: "claude",
  purpose: "The agent a new session runs.",
  tier: 1,
  tier_name: "core",
  settable: true,
  value: "claude",
  requires_restart: true,
  ...over,
});

const hostsEntry = (value: string): ConfigEntry => entry({ key: "hosts", type: "table", value });

const entries = [hostsEntry('{"box1":{"host":"box1.tailnet.ts.net","user":"ubuntu"}}')];

const actions: HostActions = {
  check: () => {},
  add: () => {},
  remove: () => {},
};

const state = (over: Partial<HostsState> = {}): HostsState => ({
  editable: true,
  checks: {},
  status: null,
  busy: [],
  ...over,
});

/** Error/echo blocks visible at section level — NOT swallowed by the add
 *  disclosure's content, which the browser paints only while open. */
function sectionLevelStatus(section: FakeElement): FakeElement[] {
  return ([".af-hosts-error", ".af-hosts-echo"] as const)
    .flatMap((sel) => section.querySelectorAll(sel))
    .filter((el) => el.closest("details.af-host-disclosure") === null);
}

const checkAllFailure: HostStatus = { name: "", message: "POST /v1/CheckHost: daemon went away", error: true };

test("a check-all failure renders at the section foot while the add form stays closed", () => {
  const section = renderHostsSection(entries, state({ status: checkAllFailure }), actions, { open: false }) as unknown as FakeElement;

  const shown = sectionLevelStatus(section);
  assert.equal(shown.length, 1, "the check-all error must live outside the closed Add host disclosure");
  assert.equal(shown[0].textContent, checkAllFailure.message);
});

test("a check-all failure renders when the capability gate leaves no add form at all", () => {
  for (const editable of [false, null] as const) {
    const section = renderHostsSection(entries, state({ editable, status: checkAllFailure }), actions, { open: false }) as unknown as FakeElement;

    const shown = sectionLevelStatus(section);
    assert.equal(shown.length, 1, `editable=${editable}: no form exists to carry the error — the section must`);
    assert.equal(shown[0].textContent, checkAllFailure.message);
  }
});

test("an open add form still owns the empty-name status, shown once", () => {
  const section = renderHostsSection(entries, state({ status: checkAllFailure }), actions, { open: true }) as unknown as FakeElement;

  const inForm = section.querySelectorAll(".af-hosts-error").filter((el) => el.closest("details.af-host-disclosure") !== null);
  assert.equal(inForm.length, 1, "the open form keeps the status next to the fields that produced it");
  assert.equal(sectionLevelStatus(section).length, 0, "and does not echo it a second time at the foot");
});

test("the empty-table check-all answer renders without a form to claim it", () => {
  // `af hosts check` with nothing saved answers like the CLI's empty list; with
  // zero rows the add details is hidden behind the empty state, so the foot is
  // again the only visible place.
  const empty = hostsEntry("{}");
  const status: HostStatus = { name: "", message: "no hosts saved; add one below", error: false };
  const section = renderHostsSection([empty], state({ status }), actions, { open: false }) as unknown as FakeElement;

  const shown = sectionLevelStatus(section);
  assert.equal(shown.length, 1);
  assert.equal(shown[0].textContent, status.message);
});

test("a vanished row's status still renders at the foot", () => {
  // The pre-existing orphan case: a remove or a manifest refresh can take the
  // row a named status belonged to.
  const orphan: HostStatus = { name: "gone", message: "removed host gone", error: false };
  const section = renderHostsSection(entries, state({ status: orphan }), actions, { open: false }) as unknown as FakeElement;

  const shown = sectionLevelStatus(section);
  assert.equal(shown.length, 1);
  assert.equal(shown[0].textContent, orphan.message);
});

// A verdict belongs to the destination it probed, not to the name it sits
// under: an edit to the raw `hosts` row (or a hand-edit the next refresh
// picks up) must retire it rather than display an old answer beside a new
// destination. The stored `entry` snapshot is what the render compares.
test("a verdict renders only while the saved entry is the one that was probed", () => {
  const verdict: HostCheckResult = { name: "box1", probed: true, reachable: true, complete: true, readiness: "ready" };
  const probedEntry = { host: "box1.tailnet.ts.net", user: "ubuntu" };
  const checkLines = (section: FakeElement) => section.querySelectorAll(".af-hosts-check-lines");

  // Unchanged entry: the verdict renders on the row. No ssh key in entries,
  // so the probe ran under the manifest's strict default.
  let section = renderHostsSection(
    entries,
    state({ checks: { box1: { entry: { ...probedEntry }, posture: "strict", result: verdict } } }),
    actions,
    { open: false },
  ) as unknown as FakeElement;
  assert.equal(checkLines(section).length, 1);
  assert.ok(
    checkLines(section)[0].textContent.includes("ready"),
    "the verdict the probe earned shows on the row",
  );

  // The raw `hosts` row now points the same name somewhere else: the verdict
  // retires rather than claiming a destination it never measured is ready.
  const edited = [hostsEntry('{"box1":{"host":"gpu.tailnet.ts.net","user":"ubuntu"}}')];
  section = renderHostsSection(
    edited,
    state({ checks: { box1: { entry: probedEntry, posture: "strict", result: verdict } } }),
    actions,
    { open: false },
  ) as unknown as FakeElement;
  assert.equal(checkLines(section).length, 0, "an entry change retires the verdict");

  // A verdict for a host that left the table before the answer landed
  // (entry undefined) must never render either.
  section = renderHostsSection(
    entries,
    state({ checks: { box1: { entry: undefined, posture: "strict", result: verdict } } }),
    actions,
    { open: false },
  ) as unknown as FakeElement;
  assert.equal(checkLines(section).length, 0, "an entry-less verdict stays hidden");
});

// A verdict also belongs to the POSTURE it was probed under (#6632, the #6622
// P2): the probe runs the effective host-key mode — the entry's own override,
// else the global ssh.host_key_verification — so a "ready" earned under
// insecure must not still show after the operator tightens the global to
// strict. The stored `posture` is what the render compares against the row's
// CURRENT effective posture.
test("a verdict renders only while the posture it was probed under still holds", () => {
  const verdict: HostCheckResult = { name: "box1", probed: true, reachable: true, complete: true, readiness: "ready" };
  const probedEntry = { host: "box1.tailnet.ts.net", user: "ubuntu" };
  const sshGlobal = (value: string) => entry({ key: "ssh.host_key_verification", value });
  const checkLines = (section: FakeElement) => section.querySelectorAll(".af-hosts-check-lines");

  // Probed under global insecure; the global still reads insecure: renders.
  const check = { entry: probedEntry, posture: "insecure", result: verdict };
  let section = renderHostsSection(
    [...entries, sshGlobal("insecure")],
    state({ checks: { box1: check } }),
    actions,
    { open: false },
  ) as unknown as FakeElement;
  assert.equal(checkLines(section).length, 1, "the verdict the probe earned shows while its posture holds");

  // The global tightens to strict: a verdict earned under insecure retires
  // rather than claim ready under a posture it never met.
  section = renderHostsSection(
    [...entries, sshGlobal("strict")],
    state({ checks: { box1: check } }),
    actions,
    { open: false },
  ) as unknown as FakeElement;
  assert.equal(checkLines(section).length, 0, "a global posture change retires a verdict probed under the old one");

  // An entry-level override insulates the row: the probe ran under the
  // override, so a global change it never depended on leaves it standing.
  const overrideEntry = { ...probedEntry, host_key_verification: "insecure" };
  const overrideRows = [hostsEntry('{"box1":{"host":"box1.tailnet.ts.net","user":"ubuntu","host_key_verification":"insecure"}}')];
  for (const global of ["insecure", "strict"]) {
    section = renderHostsSection(
      [...overrideRows, sshGlobal(global)],
      state({ checks: { box1: { entry: overrideEntry, posture: "insecure", result: verdict } } }),
      actions,
      { open: false },
    ) as unknown as FakeElement;
    assert.equal(checkLines(section).length, 1, `global=${global}: an override insulates the verdict from the global`);
  }

  // And a use_ssh_config host composes no host-key options at all — its probe
  // takes the operator's ssh_config — so its "" posture never moves with the
  // global either.
  const uscEntry = { host: "box1.tailnet.ts.net", use_ssh_config: true };
  const uscRows = [hostsEntry('{"box1":{"host":"box1.tailnet.ts.net","use_ssh_config":true}}')];
  section = renderHostsSection(
    [...uscRows, sshGlobal("strict")],
    state({ checks: { box1: { entry: uscEntry, posture: "", result: verdict } } }),
    actions,
    { open: false },
  ) as unknown as FakeElement;
  assert.equal(checkLines(section).length, 1, "a use_ssh_config verdict does not move with the global");
});

// A check-all probes every host: a per-host check that answered first would
// be overwritten by the slower aggregate, and a remove mid-probe leaves a
// verdict for a row that may not exist when the answer lands. The row verbs
// rest for its span.
test("a running check-all rests every row's verbs", () => {
  const section = renderHostsSection(entries, state({ busy: [""] }), actions, { open: false }) as unknown as FakeElement;

  assert.equal(section.querySelectorAll(".af-hosts-check").length, 1);
  assert.equal(section.querySelectorAll(".af-hosts-check")[0].disabled, true, "box1's Check rests while check-all is in flight");
  assert.equal(section.querySelectorAll(".af-hosts-remove")[0].disabled, true, "and its Remove rests with it");
});

// The port field stays type=text: a number input swallows text the browser
// reads as invalid (validity.badInput makes .value ""), which would submit as
// an OMITTED port. The typed text must reach doHostAdd's check so "1e" is
// refused rather than saved as the default port.
test("the port field preserves typed text for validation", () => {
  const sent: HostAddDraft[] = [];
  const capture: HostActions = { check: () => {}, add: (d) => { sent.push(d); }, remove: () => {} };
  const section = renderHostsSection(entries, state(), capture, { open: true }) as unknown as FakeElement;
  const field = (key: string) => section.querySelector(`input[data-host-input="${key}"]`) as unknown as { type: string; value: string };

  const port = field("port");
  assert.equal(port.type, "text", "not a number input — invalid text cannot be swallowed as an empty port");
  field("name").value = "box2";
  field("host").value = "gpu.tailnet.ts.net";
  port.value = "1e";
  section.querySelectorAll(".af-hosts-add")[0].click();

  assert.equal(sent.length, 1);
  assert.equal(sent[0].port, "1e", "the typed text reaches doHostAdd's port check verbatim");
});
