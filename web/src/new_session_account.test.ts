// The new-session modal's account wire contract (#6695): a configured default
// is DISPLAYED in the field, but only a deliberate user pick may go on the
// wire — an untouched field must serialize as no explicit account so the
// daemon applies its own default-account rules (and may drop the default on
// backends that cannot carry it, instead of refusing the create).
import assert from "node:assert/strict";
import test from "node:test";
import type { CreateSessionInput } from "./api.js";
import type { BackendCatalog } from "./backends.js";
import { FakeEvent, type FakeElement, installFakeDom } from "./fakedom.js";
import { newSessionModal } from "./modals.js";
import type { ProgramCatalog } from "./programs.js";
import type { AccountsResponse } from "./types.js";

const REGISTRY: AccountsResponse = {
  agents: ["claude", "codex"],
  defaults: { claude: "work", codex: "ops" },
  entries: [
    { agent: "claude", name: "personal", dir: "/accounts/personal", registration_only: false, logged_in: true },
    { agent: "claude", name: "work", dir: "/accounts/work", registration_only: false, logged_in: true },
    { agent: "codex", name: "ops", dir: "/accounts/ops", registration_only: false, logged_in: true },
  ],
};

const PROGRAMS: ProgramCatalog = {
  programs: [{ name: "claude" }, { name: "codex" }],
  default: "claude",
};

const BACKENDS: BackendCatalog = {
  backends: [
    { name: "local", label: "This machine", status: "available" },
    { name: "sandbox", label: "Sandbox — staging isolate", status: "available" },
  ],
  default: "local",
  default_status: "available",
};

function openModal(registry: AccountsResponse = REGISTRY) {
  const { restore } = installFakeDom();
  const submitted: CreateSessionInput[] = [];
  const handle = newSessionModal(["/repo"], "/repo", {
    onSubmit: (values) => submitted.push(values),
    onCancel: () => {},
    loadBackends: () => Promise.resolve(BACKENDS),
    loadPrograms: () => Promise.resolve(PROGRAMS),
    loadAccounts: () => Promise.resolve(registry),
    suggestName: () => Promise.resolve(""),
  });
  return { restore, root: handle.el as unknown as FakeElement, submitted };
}

const flush = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

function field(root: FakeElement, label: string): FakeElement {
  const el = root.querySelector(`[aria-label="${label}"]`);
  assert.ok(el, `no field labelled ${label}`);
  return el;
}

function submit(root: FakeElement): void {
  field(root, "Session title").value = "demo";
  const form = root.querySelector("form");
  assert.ok(form, "modal form not found");
  form.dispatchEvent(new FakeEvent("submit"));
}

test("an untouched account field sends no explicit account though it displays the default", async (t) => {
  const { restore, root, submitted } = openModal();
  t.after(restore);
  await flush();
  const account = field(root, "Account");
  assert.equal(account.value, "work"); // the configured default IS displayed
  submit(root);
  assert.equal(submitted.length, 1);
  assert.equal(submitted[0].account, "",
    "an untouched field must serialize as no explicit account — the daemon applies the default itself");
});

test("a deliberately picked account goes on the wire", async (t) => {
  const { restore, root, submitted } = openModal();
  t.after(restore);
  await flush();
  const account = field(root, "Account");
  account.value = "work";
  account.dispatchEvent(new FakeEvent("change"));
  submit(root);
  assert.equal(submitted.length, 1);
  assert.equal(submitted[0].account, "work");
});

test("switching agent mid-modal keeps the untouched field implicit", async (t) => {
  const { restore, root, submitted } = openModal();
  t.after(restore);
  await flush();
  const program = field(root, "Program");
  program.value = "codex";
  program.dispatchEvent(new FakeEvent("change"));
  const account = field(root, "Account");
  assert.equal(account.value, "ops"); // codex's configured default — still only displayed
  submit(root);
  assert.equal(submitted.length, 1);
  assert.equal(submitted[0].account, "");
});

test("switching backend mid-modal keeps the untouched field implicit", async (t) => {
  const { restore, root, submitted } = openModal();
  t.after(restore);
  await flush();
  const backend = field(root, "Backend");
  backend.value = "sandbox";
  backend.dispatchEvent(new FakeEvent("change"));
  submit(root);
  assert.equal(submitted.length, 1);
  assert.equal(submitted[0].account, "",
    "the sandbox create must reach the daemon without an explicit account, so its default can be dropped");
});
