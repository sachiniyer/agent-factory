import { field } from "./components.js";
import { recoveryScreen } from "./recovery.js";
// The Hosts section of the web config view (#5571 W10.5) — the web analogue of
// `af hosts add|list|check|remove`.
//
// It MANAGES two manifest keys, and it is built on the same writer `af config
// set` uses, so it is deliberately NOT the accounts pattern's "not config" —
// `hosts` and `default_host` are global-only keys in config/manifest.go whose
// rows also render, raw, in the tiered list above. What this section adds is
// the per-entry workflow the compact-JSON row cannot express: add one entry
// (validated and applied by the daemon as ONE read-modify-write under the
// config file lock — config.EditGlobalHosts, #5855), remove one, probe one or
// all with CheckHost, and see each outcome on its own row.
//
// The same no-local-validation rule as the config form applies, harder:
// ValidateHostEntry's name and destination rules, the exists/default checks
// and the port range all live where the write happens, so a refusal comes back
// in the daemon's own words and is shown verbatim. The form splits the
// destination into fields rather than accepting `[user@]host[:port]` for the
// same reason — parseHostDestination is Go (commands/hostscmd.go), and a
// browser copy of it is how a UI comes to accept a shape the writer rejects.
// One thing is checked here: the port must be a NUMBER to cross the wire as
// `port` at all — a non-numeric answer cannot be sent, so it is refused
// locally rather than silently dropped.
//
// The ONE send the capability gate exists for is hosts_edit: a daemon older
// than SetConfigValueRequest.HostsEdit tolerates the unknown field and writes
// an EMPTY `hosts` value in its place (daemon/control_types.go). The section
// reads Ping's config_hosts_edit bit once per view entry — the same route
// af hosts add's daemon half honors — and refuses the send when it is absent
// or unreadable, saying so rather than risking the table.
//
// CheckHost is the diagnostic every surface shares: the TUI and CLI pickers
// READ the readiness its answer updates, and this section runs it because
// `af hosts check` is the loop a saved-but-not-ready host needs. Its copy
// mirrors renderHostCheck's arms (hostscmd.go) minus the Go-composed ssh
// remedy command, which this surface cannot build.
//
// CSP-safe like the rest of the client: createElement + addEventListener via
// h(), no innerHTML with markup.

import { h } from "./dom.js";
import type {
  ConfigEntry,
  HostCheckResult,
  HostEntry,
} from "./types.js";

/** What the Hosts section can ask the shell to do. The shell owns the token,
 *  the capability gate and the refresh, so the section reports intent and
 *  renders the outcome it is handed back — the same division the config rows
 *  and the accounts section keep. */
export interface HostActions {
  /** POST /v1/CheckHost. `name` "" probes every saved host, the flagless CLI
   *  form. */
  check: (name: string) => void;
  /** One atomic hosts_edit add — plus, when `makeDefault` is set, the
   *  `default_host` write and the probe `af hosts add` runs. */
  add: (draft: HostAddDraft) => void;
  /** One atomic hosts_edit remove. `clearDefault` is the remove's own answer
   *  to "this host is the default" — the button that sends it says so. */
  remove: (name: string, clearDefault: boolean) => void;
}

/** The add form's fields, already split — see the file comment for why the
 *  destination is never one `[user@]host[:port]` string here. */
export interface HostAddDraft {
  name: string;
  host: string;
  user: string;
  /** Text as typed; the shell turns it into the wire's `port` number or refuses
   *  locally when it is not a number at all. */
  port: string;
  identityFile: string;
  workspaceRoot: string;
  /** "" keeps the global `ssh.host_key_verification` posture; a value overrides
   *  it for this host. Ignored when useSSHConfig is set — the writer refuses
   *  the pair, so the form never sends it. */
  hostKeyVerification: string;
  useSSHConfig: boolean;
  makeDefault: boolean;
}

/** The outcome of the last host action, as the shell learned it. `name` ""
 *  belongs to the add form — the name it was for may be gone from the input by
 *  the time a refusal lands. */
export interface HostStatus {
  name: string;
  /** What happened, in the daemon's own words where it had any. */
  message: string;
  /** True when `message` is a refusal. */
  error: boolean;
  /** True when a Check produced this, so a later successful check retires it.
   *  Add/remove outcomes carry no flag: a probe's success does not resolve a
   *  write refusal. */
  check?: boolean;
}

/** One cached probe outcome plus the inputs it measured under. The pair is
 *  what makes the cache honest: a verdict belongs to the destination it
 *  probed, not to the name it sits under — an edit to the raw `hosts` row (or
 *  a hand-edit the next refresh picks up) retires it by changing the entry —
 *  and to the posture it ran under, so saving a new global
 *  `ssh.host_key_verification` retires a verdict earned under the old one
 *  (#6632). */
export interface HostCheck {
  /** The saved entry at request time; undefined when the host left the table
   *  before the answer landed. */
  entry: HostEntry | undefined;
  /** The effective host-key mode the probe ran under — the entry's own
   *  `host_key_verification`, else the global `ssh.host_key_verification` it
   *  inherited, and "" for a `use_ssh_config` host, whose probe composes no
   *  host-key options at all. */
  posture: string;
  result: HostCheckResult;
}

/** Everything the section renders. */
export interface HostsState {
  /** GET /v1/health's config_hosts_edit as last read: true when this daemon
   *  applies a hosts_edit atomically, false when it answered without the bit
   *  (older daemon), null when the read itself failed. This bit only ARMS the
   *  mutating controls — the send itself re-reads health fresh, the same
   *  Ping-before-send the CLI's EditGlobalHosts does. */
  editable: boolean | null;
  /** Probe results this view asked for, by host name — each held beside the
   *  entry it probed, so a config change retires the verdict rather than
   *  leaving an old answer beside a new destination. Empty until a check
   *  runs: the section shows nothing cached rather than claiming freshness it
   *  did not just earn. */
  checks: Record<string, HostCheck>;
  /** The last add/remove/check outcome for its row. */
  status: HostStatus | null;
  /** Names with a mutation or a probe in flight; their rows' buttons rest. */
  busy: string[];
}

/** The empty state, so the shell has one place to get it from. */
export function emptyHostsState(): HostsState {
  return { editable: null, checks: {}, status: null, busy: [] };
}

/** Marks an add-form field with the draft key it belongs to, so a rebuild can
 *  restore the draft and the focus to the same one (#2933's rule — a refresh
 *  landing mid-typing must not eat it). */
export const HOST_INPUT_ATTR = "data-host-input";

/** One saved host: its list name, its entry, and whether it is the default. */
export interface HostRow {
  name: string;
  entry: HostEntry;
  isDefault: boolean;
}

/** The section's read of the manifest: the `hosts` table entry plus the
 *  `default_host` name. `parseError` names a `hosts` value that did not decode
 *  — a hand-edit can leave one, and rendering it as "no hosts" would hide the
 *  file it actually holds. */
export function hostRows(entries: ConfigEntry[]): {
  rows: HostRow[];
  defaultName: string;
  parseError: string;
} {
  let raw = "{}";
  let defaultName = "";
  for (const e of entries) {
    if (e.key === "hosts") raw = e.value;
    if (e.key === "default_host") defaultName = e.value;
  }
  let table: Record<string, HostEntry>;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      return { rows: [], defaultName, parseError: `hosts is ${raw}, not a table of entries` };
    }
    table = parsed as Record<string, HostEntry>;
  } catch (err) {
    return { rows: [], defaultName, parseError: `hosts could not be read: ${err instanceof Error ? err.message : String(err)}` };
  }
  const rows = Object.keys(table)
    .sort()
    .map((name) => ({ name, entry: table[name], isDefault: name === defaultName }));
  return { rows, defaultName, parseError: "" };
}

/** The section's `host_key_verification` select options, taken from the
 *  manifest's OWN enum for `ssh.host_key_verification` — never a local copy:
 *  a fourth posture added server-side must reach this picker with no edit
 *  (#1970's rule). Returns [] when the served manifest predates the enum, in
 *  which case the form falls back to a text field the daemon validates. */
export function hostKeyPostures(entries: ConfigEntry[]): string[] {
  for (const e of entries) {
    if (e.key === "ssh.host_key_verification") {
      return e.enum ?? [];
    }
  }
  return [];
}

/** `af hosts add`'s parenthetical — describeHost in commands/hostscmd.go. */
export function describeHost(entry: HostEntry): string {
  let s = entry.host ?? "";
  if (entry.user) s = `${entry.user}@${s}`;
  if (entry.port) s = `${s}:${entry.port}`;
  return s;
}

/** The host-key mode a probe of `entry` actually runs under — hostPosture in
 *  session/ssh_command_host.go, which the daemon's CheckHost feeds with
 *  cfg.SSHHostKeyVerification (daemon/checkhost.go): the entry's own
 *  `host_key_verification` override, else the global
 *  `ssh.host_key_verification` as served, else the manifest's `strict`
 *  default. A `use_ssh_config` host answers "" — hostCheckShell composes no
 *  host-key options for it (session/host_probe.go), the operator's ssh_config
 *  is the authority, so the global is not an input its verdict can stale
 *  under. */
export function effectiveHostKeyPosture(entry: HostEntry, entries: ConfigEntry[]): string {
  if (entry.use_ssh_config === true) {
    return "";
  }
  if (entry.host_key_verification) {
    return entry.host_key_verification;
  }
  for (const e of entries) {
    if (e.key === "ssh.host_key_verification") {
      return e.value === "" ? "strict" : e.value;
    }
  }
  return "strict";
}

/** Whether two saved entries are the same probing target, field by field. A
 *  verdict recorded against one entry renders only while the row's saved
 *  entry still equals it: change the destination under a checked name and the
 *  old verdict is retired, not displayed. */
export function sameHostEntry(a: HostEntry | undefined, b: HostEntry): boolean {
  if (a === undefined) {
    return false;
  }
  return (
    a.host === b.host &&
    (a.user ?? "") === (b.user ?? "") &&
    (a.port ?? 0) === (b.port ?? 0) &&
    (a.identity_file ?? "") === (b.identity_file ?? "") &&
    (a.workspace_root ?? "") === (b.workspace_root ?? "") &&
    (a.host_key_verification ?? "") === (b.host_key_verification ?? "") &&
    (a.use_ssh_config ?? false) === (b.use_ssh_config ?? false)
  );
}

/** Whether a probed host answered recognisably with no blocking problem —
 *  hostReady in hostscmd.go. A reachable host whose probe af could not read is
 *  unknown, never ready (#5841). */
export function hostReady(r: HostCheckResult): boolean {
  return r.probed && r.reachable && r.complete && (r.problems ?? []).length === 0;
}

/** renderHostCheck's arms, as lines the row shows under itself. The one thing
 *  this surface cannot compose is the Go ssh remedy argv
 *  (session.OperatorSSHCommand), so the connectivity remedy names the same
 *  operator-owned facts without a fake command. */
export function hostCheckLines(r: HostCheckResult): string[] {
  const out: string[] = [];
  if (!r.probed) {
    out.push(`another probe is already running (cached state: ${r.readiness}); rerun in a moment`);
  } else if (!r.reachable) {
    out.push(`could not be probed${r.error ? `: ${r.error}` : ""}`);
    out.push("fix: make ssh to it succeed from the daemon host — bring the tailnet or VPN up, load the key, trust the host key — then check it again. af opens no tunnel and edits no ssh config.");
  } else if ((r.problems ?? []).length > 0) {
    out.push("cannot run a session yet");
    for (const p of r.problems ?? []) out.push(`fix: ${p}`);
    out.push("then check it again");
  } else if (!r.complete) {
    // The host answered something af could not read (truncated/garbled output,
    // banner or profile noise on a probe line). Unknown, never ready (#5841).
    out.push(`unknown: the host answered but not with a recognisable git, tmux and gh report${r.readiness ? `; cached state: ${r.readiness}` : ""}`);
    out.push("fix: check that a non-interactive ssh prints no banner or extra output for a plain command, then check it again");
  } else {
    out.push("ready (git, tmux and gh answered)");
  }
  for (const w of r.warnings ?? []) out.push(`note: ${w}`);
  return out;
}

/** The refusal `af hosts add|remove` prints when the running daemon lacks the
 *  hosts_edit bit (daemon/config_save_client.go EditGlobalHosts), with the same
 *  `%s` version substitution — the running daemon's version, or nothing when
 *  even Ping did not answer. */
export function hostsEditRefusal(version: string | undefined): string {
  const v = version !== undefined && version !== "" ? ` (${version})` : "";
  return `the running daemon${v} cannot apply a host-list edit atomically; run \`af daemon restart\` to upgrade it, then retry (nothing was written)`;
}

/**
 * Renders the Hosts section into a single element the config view appends.
 *
 * A function rather than a class, like the accounts section: it holds no state
 * of its own. Everything it draws comes from `entries` + `state`; the one piece
 * of local state a form needs — the add form's drafts — is read at submit time
 * and re-attributed via HOST_INPUT_ATTR so the pane's rebuild can hand it back.
 */
export function renderHostsSection(
  entries: ConfigEntry[],
  state: HostsState,
  actions: HostActions,
  disclosure = { open: false },
): HTMLElement {
  const section = h("section", { class: "af-hosts" });
  section.setAttribute("aria-label", "Hosts");

  const { rows, parseError } = hostRows(entries);
  // A check-all probes every host, so the row verbs rest for its span too:
  // a per-host check answered first would be overwritten by the slower
  // aggregate, and a remove mid-probe leaves a verdict for a row that may
  // not exist when the answer lands.
  const busy = (name: string) => state.busy.includes(name) || state.busy.includes("");

  const checkAllBtn = h(
    "button",
    { type: "button", class: "af-ghost af-hosts-check-all" },
    "Check all",
  );
  checkAllBtn.disabled = state.busy.length > 0;
  checkAllBtn.addEventListener("click", () => actions.check(""));

  const head = h(
    "div",
    { class: "af-hosts-head" },
    h("span", { class: "af-hosts-title" }, "Hosts"),
    h("span", { class: "af-view-count" }, String(rows.length)),
    checkAllBtn,
  );
  section.append(
    head,
    h(
      "p",
      { class: "af-hosts-note" },
      "The ssh hosts a sandbox session can run on · you own connectivity — af opens no tunnel and edits no ssh config.",
    ),
  );

  if (parseError !== "") {
    section.append(
      h("p", { class: "af-hosts-error", role: "alert" }, `The host table could not be read: ${parseError}`),
    );
    return section;
  }

  // The capability gate: only mutating controls rest on it. Check still runs —
  // it is a read the probe route either serves or refuses with its own
  // message, which the row shows.
  if (state.editable !== true) {
    const reason = state.editable === false
      ? "This daemon is older than atomic host-list edits; adding or removing a host here is refused rather than risk writing an empty host table. `af hosts` on the host itself works, or upgrade the daemon."
      : "The daemon's capabilities could not be read; adding or removing a host here is refused until they can. Check the connection and reopen this view.";
    section.append(h("p", { class: "af-hosts-notice" }, reason));
  }

  const list = h("div", { class: "af-hosts-list" });
  for (const row of rows) {
    list.append(renderHostRow(row, entries, state, actions, busy(row.name)));
  }
  section.append(list);

  if (state.editable === true) {
    const add = h("details", { class: "af-host-disclosure" });
    add.open = disclosure.open;
    add.addEventListener("toggle", () => {
      disclosure.open = add.open;
    });
    add.append(
      h("summary", {}, "Add host"),
      renderAddForm(entries, state.status, actions),
    );
    section.append(add);
    if (rows.length === 0 && !disclosure.open) {
      add.hidden = true;
      const empty = recoveryScreen({
        condition: "No hosts saved",
        action: "Add host",
        run: () => {
          empty.remove();
          add.hidden = false;
          add.open = true;
          disclosure.open = true;
          add.querySelector<HTMLElement>(`input[${HOST_INPUT_ATTR}="name"]`)?.focus();
        },
      });
      section.prepend(empty);
    }
  } else if (rows.length === 0) {
    section.append(h("p", { class: "af-hosts-empty" }, "No hosts saved."));
  }

  // A status that belongs to no row still says so at the foot rather than
  // disappearing: a named one whose row vanished, or the add form's empty
  // name — which a check-all failure also carries. The open form shows the
  // empty name itself; a closed disclosure, a hidden one (the empty table),
  // or no form at all (the capability gate) must not swallow it.
  if (state.status !== null) {
    const formShowsIt =
      state.status.name === "" && state.editable === true && disclosure.open;
    const orphaned =
      state.status.name !== "" && !rows.some((r) => r.name === state.status!.name);
    if (orphaned || (state.status.name === "" && !formShowsIt)) {
      section.append(statusBlock(state.status));
    }
  }
  return section;
}

/** One saved host: its name and destination, the default mark, its verbs, and
 *  the latest probe or mutation outcome under itself. */
function renderHostRow(
  row: HostRow,
  entries: ConfigEntry[],
  state: HostsState,
  actions: HostActions,
  isBusy: boolean,
): HTMLElement {
  const el = h("div", { class: "af-hosts-row" });
  el.setAttribute("data-host", row.name);

  el.append(
    h(
      "div",
      { class: "af-hosts-label" },
      h("span", { class: "af-hosts-name" }, row.name),
      h("span", { class: "af-hosts-desc" }, describeHost(row.entry)),
      row.isDefault ? h("span", { class: "af-hosts-default" }, "default") : "",
    ),
  );

  const buttons = h("div", { class: "af-hosts-verbs" });
  const checkBtn = h(
    "button",
    { type: "button", class: "af-ghost af-hosts-check" },
    "Check",
  );
  checkBtn.disabled = isBusy;
  checkBtn.addEventListener("click", () => actions.check(row.name));
  buttons.append(checkBtn);

  if (state.editable === true) {
    // Removing the default is refused without the explicit flag, the daemon's
    // own rule — the button names that decision rather than hiding it behind a
    // retry.
    const removeBtn = h(
      "button",
      { type: "button", class: "af-ghost af-hosts-remove" },
      row.isDefault ? "Remove and clear the default" : "Remove",
    );
    removeBtn.disabled = isBusy;
    removeBtn.addEventListener("click", () => actions.remove(row.name, row.isDefault));
    buttons.append(removeBtn);
  }
  el.append(buttons);

  // The verdict shows only while every input it was probed under still holds:
  // the same destination (sameHostEntry) and the same effective host-key
  // posture — a global `ssh.host_key_verification` the row inherits retires a
  // verdict the old posture earned (#6632).
  const check = state.checks[row.name];
  if (check !== undefined && sameHostEntry(check.entry, row.entry) && check.posture === effectiveHostKeyPosture(row.entry, entries)) {
    el.append(h("div", { class: "af-hosts-check-lines" }, ...hostCheckLines(check.result).map((line) => h("div", {}, line))));
  }
  if (state.status !== null && state.status.name === row.name) {
    el.append(statusBlock(state.status));
  }
  return el;
}

/** The add form — every field optional but the first two, and none of it
 *  validated here beyond what the wire cannot carry (see the file comment). */
function renderAddForm(
  entries: ConfigEntry[],
  status: HostStatus | null,
  actions: HostActions,
): HTMLElement {
  const form = h("div", { class: "af-hosts-row af-hosts-addform" });

  const text = (key: string, label: string, placeholder: string, type = "text"): { el: HTMLElement; input: HTMLInputElement } => {
    const input = h("input", { type, class: "af-input", placeholder }) as HTMLInputElement;
    input.setAttribute("aria-label", `New host ${label}`);
    input.setAttribute(HOST_INPUT_ATTR, key);
    return { el: field(label, input), input };
  };

  const name = text("name", "Name", "what --host takes");
  const host = text("host", "Host", "hostname or address — no user@, no :port");
  const user = text("user", "User", "ssh login, optional");
  // Deliberately type=text: a number input swallows text the browser reads
  // as invalid (badInput makes .value ""), which would submit as an OMITTED
  // port — the typed text must reach doHostAdd's check so "1e" is refused
  // rather than saved as the default port.
  const port = text("port", "Port", "22");
  const identityFile = text("identity_file", "Identity file", "absolute path, optional");
  const workspaceRoot = text("workspace_root", "Workspace root", "absolute path, optional");

  // The select's options come from the manifest's enum (see hostKeyPostures);
  // a manifest that predates it gets a text field the daemon validates instead.
  const postures = hostKeyPostures(entries);
  let hkvControl: HTMLElement;
  let hkvValue: () => string;
  if (postures.length > 0) {
    const sel = h("select", { class: "af-input" }) as HTMLSelectElement;
    sel.setAttribute("aria-label", "New host Host key verification");
    sel.setAttribute(HOST_INPUT_ATTR, "host_key_verification");
    sel.append(h("option", { value: "" }, "Global default"));
    for (const p of postures) sel.append(h("option", { value: p }, p));
    hkvControl = field("Host key verification", sel);
    hkvValue = () => sel.value;
  } else {
    const t = text("host_key_verification", "Host key verification", "strict | accept-new | insecure, optional");
    hkvControl = t.el;
    hkvValue = () => t.input.value.trim();
  }

  const sshConfig = h("input", { type: "checkbox" }) as HTMLInputElement;
  sshConfig.setAttribute("aria-label", "New host Use ssh_config");
  sshConfig.setAttribute(HOST_INPUT_ATTR, "use_ssh_config");
  sshConfig.addEventListener("change", () => {
    // The writer refuses use_ssh_config together with an explicit posture, so
    // the form mirrors the rule by not offering both at once.
    const overridden = hkvControl.querySelector("input, select");
    if (overridden instanceof HTMLInputElement || overridden instanceof HTMLSelectElement) {
      overridden.disabled = sshConfig.checked;
    }
  });
  const sshConfigField = field("Use your ssh_config for this host", sshConfig);

  const makeDefault = h("input", { type: "checkbox" }) as HTMLInputElement;
  makeDefault.setAttribute("aria-label", "New host Set as default");
  makeDefault.setAttribute(HOST_INPUT_ATTR, "make_default");
  const makeDefaultField = field("Make this the default host", makeDefault);

  const submit = () => {
    const draft: HostAddDraft = {
      name: name.input.value.trim(),
      host: host.input.value.trim(),
      user: user.input.value.trim(),
      port: port.input.value.trim(),
      identityFile: identityFile.input.value.trim(),
      workspaceRoot: workspaceRoot.input.value.trim(),
      hostKeyVerification: sshConfig.checked ? "" : hkvValue(),
      useSSHConfig: sshConfig.checked,
      makeDefault: makeDefault.checked,
    };
    if (draft.name === "" || draft.host === "") {
      // Nothing to ask for; an empty send would only earn the daemon's refusal,
      // which is noise on an empty box.
      return;
    }
    actions.add(draft);
  };

  const button = h("button", { type: "button", class: "af-ghost af-hosts-add" }, "Add host");
  button.addEventListener("click", submit);
  name.input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      submit();
    }
  });
  host.input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      submit();
    }
  });

  form.append(
    name.el,
    host.el,
    user.el,
    port.el,
    identityFile.el,
    workspaceRoot.el,
    hkvControl,
    sshConfigField,
    makeDefaultField,
    h("div", { class: "af-hosts-addrow" }, button),
  );
  if (status !== null && status.name === "") {
    form.append(statusBlock(status));
  }
  return form;
}

function statusBlock(status: HostStatus): HTMLElement {
  return status.error
    ? h("div", { class: "af-hosts-error", role: "alert" }, status.message)
    : h("div", { class: "af-hosts-echo" }, status.message);
}
