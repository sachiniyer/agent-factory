// The CONFIG view of the web client: the browser analogue of the TUI's config
// editor overlay (ui/config_pane.go), and the direct counterpart to the
// conversational config agent (#1928). All three render from ONE description of
// configuration — the config manifest (config/manifest.go), delivered here by
// GetConfig — so none of them can fall behind config_types.go or each other.
//
// This file therefore contains NO list of config keys, no per-key type switch,
// and no copy of the defaults or the validation rules. It renders whatever the
// manifest says exists, in the tiers the manifest ranks, and it sends every edit
// to SetConfigValue, which the daemon hands to the same validated, file-locked,
// atomic writer `af config set` uses. A key added to config_types.go appears
// here with no edit to this file — that is the point, and
// config.test.ts pins it.
//
// Two things it must never do:
//
//   - Validate locally. A second copy of the rules is how a UI comes to accept a
//     value the loader rejects at startup, which the user then meets as a
//     failure to start instead of a red line under a field. The daemon's error
//     is shown verbatim.
//   - Imply an edit is live. config.toml is read at STARTUP, so a saved value
//     reaches af and the daemon on their next start. The restart notice comes
//     from the daemon on every successful write and is shown next to the echo,
//     at the moment of the edit.
//
// Patched in place like the rest of the shell and CSP-safe (createElement +
// addEventListener via the shared h() helper, no innerHTML with markup).

import {
  ACCOUNT_INPUT_ATTR,
  type AccountActions,
  type AccountsState,
  emptyAccountsState,
  renderAccountsSection,
} from "./accounts.js";
import {
  type QuotaState,
  emptyQuotaState,
  renderQuotaSection,
} from "./quota.js";
import {
  HOST_INPUT_ATTR,
  type HostActions,
  type HostAddDraft,
  type HostsState,
  emptyHostsState,
  renderHostsSection,
} from "./hosts.js";
import { h } from "./dom.js";
import type {
  ConfigEntry,
  ConfigSetResponse,
  ExplainConfigResponse,
  ResolvedValue,
} from "./types.js";
import { rebuildKeepingScroll } from "./scrollkeep.js";
import { THEME_CHOICES, type ThemeChoice } from "./theme.js";

/** The config list is ONE global manifest, so every rebuild shows the same list and
 *  the reader's place is always worth keeping — there is no project or filter here to
 *  change what the list contains. */
const CONFIG_LIST_TOKEN = "config";

/** What the config view can ask the shell to do. Saving is the shell's job (it
 *  owns the token and the refresh), so the pane reports intent and renders the
 *  outcome it is handed back. */
/** The settled result of one ExplainConfig call (#4803): the daemon's
 *  ResolvedValue, or its refusal already rendered to text by the shell (which
 *  owns errorText — this view renders strings, it does not interpret errors). */
export type ExplainOutcome =
  | { ok: true; resp: ExplainConfigResponse }
  | { ok: false; error: string };

export interface ConfigActions {
  save: (key: string, value: string) => void;
  /** Opens the conversational config assistant (#2467) — the web analogue of the
   *  TUI's config-agent takeover. The shell owns the token and the modal host, so
   *  the pane only reports the intent. */
  openAssistant: () => void;
  /** Resolves one key's provenance through the daemon's ExplainConfig — the
   *  SAME config.ResolvedValue `af config get --explain` renders (#4803), so
   *  this view formats a trace it did not compute and could not diverge from.
   *  Always settles: a refusal (unknown key, a daemon too old for the route)
   *  arrives as `{ok:false}` text rather than a rejection. */
  explain: (key: string) => Promise<ExplainOutcome>;
  /** The Accounts section's two verbs (#3385). They are NOT config writes and do
   *  not go through `save`: an account is a credential directory on the daemon
   *  host, not a settable key, and routing them through the config writer would
   *  be the category error #3385 asks this surface to avoid. */
  accounts: AccountActions;
  /** The Hosts section's verbs (#5571 W10.5). The add/remove pair DOES go
   *  through SetConfigValue — through its hosts_edit arm, the same atomic
   *  per-entry edit `af hosts` rides — so they are not routed through `save`,
   *  which serializes whole-value writes and could not express them. */
  hosts: HostActions;
  setTheme: (choice: ThemeChoice) => void;
  disconnect: () => void;
}

export function themeLabel(choice: ThemeChoice): string {
  switch (choice) {
    case "system":
      return "System";
    case "light":
      return "Light";
    case "dark":
      return "Dark";
  }
}

/** The outcome of the last save, as the shell learned it from the daemon. */
export interface ConfigStatus {
  key: string;
  /** The echo — the canonical value the daemon actually wrote, which may differ
   *  from what was typed. Empty when `error` is set. */
  value: string;
  /** The daemon's save outcome and warnings, shown verbatim. */
  notice: string;
  /** The validator's message, verbatim, when the value was refused. */
  error: string;
}

/** The tier headings, in the manifest's own order. Derived from the entries
 *  rather than hardcoded so a new tier needs no edit here. */
function tiersInOrder(entries: ConfigEntry[]): { tier: number; name: string }[] {
  const seen = new Map<number, string>();
  for (const e of entries) {
    if (!seen.has(e.tier)) {
      seen.set(e.tier, e.tier_name);
    }
  }
  return [...seen.entries()].sort((a, b) => a[0] - b[0]).map(([tier, name]) => ({ tier, name }));
}

/** The advanced tier's rank. Tier 3 is "correct by default and rarely touched",
 *  so it folds — the five keys that matter must not be buried under twenty. */
const TIER_ADVANCED = 3;

/** The kind of control a key gets. */
export type ControlKind = "checkbox" | "select" | "text";

/**
 * Chooses the control for a key FROM THE MANIFEST'S OWN DESCRIPTION of it —
 * never from the key's name, and never from a local table of known keys.
 *
 * This is the function that makes the form survive a config key it has never
 * heard of: an unfamiliar type still falls through to a text field rather than
 * being dropped from the form or throwing. It is exported so config.test.ts
 * locks the REAL decision rather than a copy of it — a copy would drift from
 * renderControl, which is the very failure this whole design is avoiding.
 *
 *  - `bool` → checkbox.
 *  - enumerated → picker. Excluded for a table, where the enum constrains entry
 *    NAMES rather than the value; offering it as a value picker would be a small
 *    lie about what the key takes.
 *  - anything else → text.
 *
 * A pre-#3345 daemon's retired `editable: false` wire member is deliberately
 * absent from ConfigEntry and ignored here. Recreating a read-only fallback
 * would violate the zero-read-only surface; during version skew the real field
 * instead shows that old daemon's explicit save rejection verbatim.
 */
export function controlKind(e: ConfigEntry): ControlKind {
  if (e.type === "bool") {
    return "checkbox";
  }
  if (e.enum && e.enum.length > 0 && e.type !== "table") {
    return "select";
  }
  return "text";
}

/**
 * Whether a field's current text is worth writing: only when it differs from what
 * the file already holds.
 *
 * A no-op write is not harmless. It still round-trips to the daemon, still prints
 * `set key = value`, and still raises the restart notice — so it reads as though
 * the user changed something and now owes a restart. Exported so config.test.ts
 * locks the REAL rule both the Save button and the Enter key gate on, rather than
 * a copy of it.
 */
export function canCommit(shown: string, current: string): boolean {
  return shown !== current;
}

/**
 * Whether a freshly delivered status should CLOSE the field that is open.
 *
 * A save closes the field it came from: the value is committed, and leaving it open
 * would invite a second write of the same thing. The subtlety is `statusIsNew`. A
 * success status is a LEVEL that stays on the store, not an EDGE that fires once —
 * `applyConfigValueNow` sets `configStatus` and then calls `refreshConfig()`, whose
 * commit writes only `config`/`configPath`. The refetch therefore re-enters update()
 * with fresh entries and the SAME status object, and a predicate that ignores the
 * transition closes whatever edit is open when it lands.
 *
 * That window is not hypothetical: it is precisely the window this view's focus
 * restoration exists to make usable, so the edit sitting in it is the user typing
 * again in the field they just saved. Closing it clears `editing` and retires the
 * committed draft from `drafts`, and the rebuild then paints the re-read value
 * over text that was on screen a frame earlier — a silent loss, with no error
 * and nothing to undo.
 *
 * The failure direction is deliberate. Requiring the edge can only ever leave an
 * edit OPEN that a re-delivery would have closed, and an edit that outlived its own
 * save is by definition typing the user did afterwards — the thing to keep. A field
 * left open costs a redundant Save at worst; a field closed early costs the user
 * their input. Exported so config.test.ts locks the rule rather than a copy of it.
 */
export function shouldCloseSavedField(
  status: ConfigStatus | null,
  editing: string | null,
  statusIsNew: boolean,
): boolean {
  return statusIsNew && status !== null && !status.error && status.key === editing;
}

/**
 * The one line the form shows under a saved field: the daemon's own account of
 * when the edit takes effect, plus — when the edit moved a listener — where the
 * daemon is accepting now (#3722).
 *
 * Both halves come from the daemon and neither is computed here. The address in
 * particular MUST NOT be inferred from the value that was sent: a rebind can
 * fail, in which case the daemon is still serving on the previous address and
 * says so, and a form that echoed what it typed would name an address nothing
 * answers precisely when the user most needs the real one.
 *
 * The restart notice is normally shown only for a key that requires a restart,
 * since "Applied" under every field is noise. A warning overrides that gate:
 * it is actionable for an ordinarily-live key, and both the warning and the
 * outcome sentence must reach the operator. The address is also independent of
 * the gate: a listener that moved is worth saying whether or not a restart is
 * owed, and for this form it is the address the browser must be re-pointed at.
 * Exported so config.test.ts locks the real rule rather than a copy.
 */
export function saveNotice(resp: ConfigSetResponse): string {
  const parts: string[] = [];
  // resp.warnings is the apply-time channel; resp.result.warnings is the
  // per-write channel the daemon fills even when the apply produced none
  // (e.g. default_accounts.<agent> is EffectAppliedLive). Fall back the way
  // the TUI does (ui/config_target.go) so every surface shows exactly one
  // notice — never the two differently-worded ones a daemon-running edit can
  // carry on both channels (#5266).
  let warnings = resp.warnings ?? [];
  if (warnings.length === 0) {
    warnings = resp.result.warnings ?? [];
  }
  // A warning makes the apply outcome actionable even for an ordinarily-live
  // key. In that case the restart notice must not be hidden by the key's static
  // requires-restart classification: it describes what happened to this save.
  if ((resp.result.requires_restart || warnings.length > 0) && resp.restart_notice !== "") {
    parts.push(resp.restart_notice);
  }
  parts.push(...warnings);
  const addr = resp.listener_addr ?? "";
  if (addr !== "") {
    parts.push(`Daemon now listening at ${addr}`);
  }
  return parts.join(" · ");
}

/** The comma-list keys — config.isCommaListKey's opt-in set (the cfgStringList
 *  leaves), mirrored here because the web bundle cannot import Go. This is not
 *  a second list trusted to stay current: the parity test in
 *  config/web_explain_parity_test.go parses this literal and asserts it equals
 *  isCommaListKey's opt-in set over SettableKeys(), so a comma-list key added
 *  on the Go side fails that test until this line is updated. */
const COMMA_LIST_KEYS: ReadonlySet<string> = new Set([
  "account_handoff.exclude_sessions",
  "limit_account_candidates",
  "network.allowed_hosts",
  "network.cors_allowed_origins",
]);

/** Mirrors config.commaJoinStringList's representable check: every element is
 *  a string, none carries a comma or a control character, and re-splitting
 *  the join through the same trim-and-drop-empty rule as splitListValue
 *  reproduces every element exactly. Otherwise the caller keeps the JSON
 *  spelling — the unambiguous form that signals the value cannot be typed
 *  back through `af config set <key> a,b` as one line.
 *
 *  One Go-side guard is deliberately not mirrored: the per-key leaf validator
 *  (spec.validate on the joined string) stays in config/, so a hand-edited
 *  element its leaf would reject keeps the JSON spelling on the CLI/TUI while
 *  the web joins it — an accepted gap rather than a second copy of the
 *  validation rules drifting from them. Every element a validated write path
 *  can produce joins identically on all three surfaces. */
function commaJoinStringList(value: unknown[]): string | null {
  if (!value.every((elem): elem is string => typeof elem === "string")) {
    return null;
  }
  for (const s of value) {
    if (s.includes(",") || /\p{Cc}/u.test(s)) {
      return null;
    }
  }
  const joined = value.join(",");
  const roundTripped = joined
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
  if (
    roundTripped.length !== value.length ||
    roundTripped.some((s, i) => s !== value[i])
  ) {
    return null;
  }
  return joined;
}

/** The web spelling of config.FormatExplainValueForKey (#4803, #6950): an
 *  explicitly configured empty string stays visible as `""` — a bare blank
 *  would read as absent — scalars print bare, composites print as compact
 *  JSON, and a string list under a comma-list key comma-joins into the form
 *  `af config set` accepts, so the trace shows exactly what the CLI and TUI
 *  show for the same key. Exported so config.test.ts locks the REAL renderer
 *  rather than a copy. */
export function explainValueText(key: string, value: unknown): string {
  if (COMMA_LIST_KEYS.has(key) && Array.isArray(value)) {
    const joined = commaJoinStringList(value);
    if (joined !== null) {
      return joined === "" ? '""' : joined;
    }
  }
  if (typeof value === "string") {
    return value === "" ? '""' : value;
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  if (value === null || value === undefined) {
    return "null";
  }
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

/**
 * Serializes async work per key: a second save for the same key waits for the
 * first to settle, so the LAST value the user chose is the last one written.
 *
 * WHY THE CLIENT AND NOT THE DAEMON. The daemon already owns the part it can
 * own: config.SetGlobalConfigValue takes a file lock, so concurrent writes
 * cannot corrupt or interleave, and that holds across the TUI, `af config set`,
 * and a hand-edit alike (config.toml is deliberately NOT daemon-exclusive state
 * — see configset.go). What a lock cannot supply is INTENT. Two in-flight POSTs
 * for the same key are ordered by the network, not by the user, so a checkbox
 * toggled on→off→on can land off — every write succeeded, none was clobbered,
 * and the answer is still wrong. Only the side that knows the user pressed off
 * before on can keep that order.
 *
 * Per KEY, not global: two different keys have no ordering relationship, and
 * queueing them behind each other would make a slow write block an unrelated one
 * for no gain.
 *
 * The returned promise settles when this item (and anything ahead of it for the
 * same key) settles. Production may ignore it; tests and other coordination
 * points can await real queue completion instead of guessing with a timer.
 */
export function createKeyedQueue(): (key: string, run: () => Promise<void>) => Promise<void> {
  const tails = new Map<string, Promise<void>>();
  return (key, run) => {
    const prev = tails.get(key) ?? Promise.resolve();
    // The chain must never reject, for two reasons: a failed save must not
    // cancel the next one (one rejected value would wedge that key for the
    // session), and a rejected tail with nothing queued behind it surfaces as an
    // unhandled rejection — a red error in the user's console every time the
    // validator refuses a value. The error is not swallowed: applyConfigValue's
    // own catch has already put the validator's message on screen. This only
    // keeps the QUEUE resolvable.
    const next = prev.then(run, run).catch(() => {});
    tails.set(key, next);
    void next.finally(() => {
      // Drop the entry only if nothing queued behind us, so the map stays the
      // size of the keys in flight rather than growing forever.
      if (tails.get(key) === next) {
        tails.delete(key);
      }
    });
    return next;
  };
}

export class ConfigPane {
  readonly el: HTMLElement;

  private entries: ConfigEntry[] = [];
  private registration = { open: false, agent: "" };
  /** The Hosts section's add-form disclosure state (#5571 W10.5). */
  private hostDisclosure = { open: false };
  private path = "";
  private status: ConfigStatus | null = null;
  /** The Accounts section's data (#3385). It is rendered by this view but is not
   *  config: see accounts.ts. */
  private accounts: AccountsState = emptyAccountsState();
  /** The Hosts section's data (#5571 W10.5): the daemon's hosts-edit capability
   *  bit, this view's probe results, and the last mutation's outcome. It
   *  manages config keys (`hosts`, `default_host`) but is not itself a key, so
   *  it rides beside `accounts` rather than inside `entries`. */
  private hosts: HostsState = emptyHostsState();
  /** The Usage section's data (#2983) — daemon's QuotaReport, rendered below
   *  Accounts. Same "not config" rule: see quota.ts. */
  private quota: QuotaState = emptyQuotaState();
  private theme: ThemeChoice = "system";
  private showAdvanced = false;
  /** The key whose field the focus machinery is tracking — the row
   *  `editingInput` repoints to on each rebuild and `shouldCloseSavedField`
   *  closes. Typing into a row marks it; only that row's own successful save
   *  unmarks it, so it names where the user most recently typed, not the only
   *  row holding a draft. */
  private editing: string | null = null;
  /** Every row's typed-but-unsaved text, keyed by config key. A rebuild repaints
   *  from this map rather than from `editing`/`draft`, so a refresh never eats
   *  a draft the user did not submit: the single-slot pair it replaced kept
   *  only the most recently typed row and silently dropped every earlier one
   *  (#6692). A key's own successful save deletes its entry — a committed draft
   *  is done, and replaying it would show a saved value as unsaved — while a
   *  refused save keeps it so the rejected text stays to be corrected. */
  private readonly drafts = new Map<string, string>();
  // The live controls a rebuild replaces, so focus can be handed back to whichever of
  // them had it (#2933). Null whenever that control is not currently rendered.
  private editingInput: HTMLInputElement | null = null;
  /** The config key whose input held DOM focus when the in-progress rebuild started,
   *  so render() can re-point `editingInput` at that row's replacement even when the
   *  row is no longer the open edit. Live only for the duration of one render (set
   *  and cleared around the single `this.render()` call). */
  private restoreKey: string | null = null;
  private advancedToggle: HTMLElement | null = null;

  // The open provenance trace (#4803): at most one row explains at a time, the
  // same way at most one edits — two open traces would read as though the keys
  // resolved together. `explainGeneration` drops a stale fetch that lands after
  // the user opened a different row or closed this one.
  private explainKey: string | null = null;
  private explainResp: ExplainConfigResponse | null = null;
  private explainError = "";
  private explainBusy = false;
  private explainGeneration = 0;

  private lastEntries: ConfigEntry[] | null = null;
  private lastStatus: ConfigStatus | null = null;
  private lastAccounts: AccountsState | null = null;
  private lastHosts: HostsState | null = null;
  private lastQuota: QuotaState | null = null;
  private lastTheme: ThemeChoice | null = null;

  /** The Hosts add form exactly as it stood when each send went out — keyed by
   *  HOST_INPUT_ATTR like readHostDrafts' map, and filed under the submitted
   *  name so a success edge retires ITS OWN send's draft. Adds are not
   *  serialized: a second "Add host" can go out while the first is still in
   *  flight, and the answers may land in either order — one snapshot would
   *  retire the wrong send's values. On the success edge each field still
   *  holding what was sent is retired; one retyped since is the NEXT host's
   *  draft, not a replay of the saved one (#6861). */
  private readonly committedHostAdds = new Map<string, Map<string, { value: string; checked: boolean }>>();
  /** HostActions with `add` wrapped to snapshot the form at send time — the
   *  only record of what was committed, since the returned status names no
   *  draft. The other verbs pass straight through. */
  private readonly hostActions: HostActions;

  constructor(private readonly actions: ConfigActions) {
    this.el = h("section", { class: "af-config" });
    this.el.setAttribute("aria-label", "Config");
    this.hostActions = {
      ...actions.hosts,
      add: (draft: HostAddDraft) => {
        this.committedHostAdds.set(draft.name, this.readHostDrafts());
        actions.hosts.add(draft);
      },
    };
  }

  /** Feeds the pane fresh manifest rows. Re-rendering is skipped when nothing
   *  changed, matching the rest of the shell's patch-in-place model. */
  update(
    entries: ConfigEntry[],
    path: string,
    status: ConfigStatus | null,
    accounts: AccountsState,
    quota: QuotaState,
    theme: ThemeChoice = this.theme,
    hosts: HostsState = this.hosts,
  ): void {
    if (this.lastEntries === entries && this.lastStatus === status && this.lastAccounts === accounts
      && this.lastQuota === quota && this.lastTheme === theme && this.lastHosts === hosts) {
      return;
    }
    // Captured before `lastStatus`/`lastEntries` are overwritten below: the
    // status edge is what `shouldCloseSavedField` gates on (by then the level
    // would look unchanged), and the entries edge is what decides whether an
    // open explanation must re-resolve (see below).
    const statusIsNew = status !== this.lastStatus;
    const entriesAreNew = this.lastEntries !== entries;
    const registrationSucceeded = accounts.status !== this.accounts.status && accounts.status
      && accounts.status.name === "" && !accounts.status.error;
    if (registrationSucceeded) {
      const submitted = this.accountInput(accounts.status!.agent);
      if (submitted) submitted.value = "";
    }
    // The Hosts add form's equivalent edge (#6861): a successful add arrives as
    // a `name === ""` non-error status — the register form's own convention —
    // but ONLY when it is not a check outcome, which carries the same shape
    // with `check: true` (a check-all's "no hosts saved" answer must never
    // retire a half-typed form). Retiring happens now, before
    // rerenderKeepingUserState's readHostDrafts, so the committed values are
    // not snapshotted and replayed onto the fresh form.
    const hostAddSucceeded = hosts.status !== this.hosts.status && hosts.status !== null
      && hosts.status.name === "" && !hosts.status.error && hosts.status.check !== true;
    if (hostAddSucceeded) {
      this.retireCommittedHostAdd(hosts.status!.message);
    }
    this.lastEntries = entries;
    this.lastStatus = status;
    this.lastAccounts = accounts;
    this.lastQuota = quota;
    this.lastTheme = theme;
    this.lastHosts = hosts;
    this.theme = theme;
    // Retired web appearance keys from older daemons are never editable here.
    this.entries = entries.filter((entry) => entry.key !== "theme" && !entry.key.startsWith("theme."));
    this.path = path;
    this.status = status;
    this.accounts = accounts;
    this.quota = quota;
    this.hosts = hosts;
    if (shouldCloseSavedField(status, this.editing, statusIsNew)) {
      this.editing = null;
    }
    // A committed draft is done: the success edge retires that key's text from
    // the map — whether or not it is still the open edit — so the rebuild shows
    // what the daemon now holds rather than replaying saved text as though it
    // were still unsaved. A refused save carries `error`, keeps the draft, and
    // leaves the rejected text to be corrected.
    if (statusIsNew && status !== null && status.error === "") {
      this.drafts.delete(status.key);
    }
    // An open trace describes the manifest it was fetched from. A refresh that
    // carries NEW entries — a save landing, a hand-edit, an `af config set`
    // elsewhere — has already moved the row's effective value; leaving the old
    // ResolvedValue painted under it would show provenance for a value the row
    // no longer reports. Re-resolve in place rather than close the disclosure:
    // the generation guard drops the answer if the user has since opened
    // another row or closed this one, so the refetch is free of the race it
    // could otherwise lose.
    if (entriesAreNew && this.explainKey !== null) {
      this.requestExplain(this.explainKey);
    }
    this.rerenderKeepingUserState();
  }

  /**
   * Re-renders the pane without throwing away what the user was in the middle of.
   *
   * Every rebuild replaces the controls, which costs three things the render itself
   * cannot recover: the reader's scroll offset, focus, and the caret. The typed text
   * already survives (render reads `draft`); focus did not, and that is the one that
   * bites hardest — the app's document-level keys are gated on
   * `isNativeControl(document.activeElement)` (index.ts), so once focus falls back to
   * <body> the REST of what the user types stops being a value and starts being
   * shortcuts: a "[" or "]" in a path cycles the view out from under them mid-edit.
   *
   * Focus is handed back only to a control that genuinely HAD it, so a rebuild can
   * never steal focus from wherever the user actually is. Both re-render paths go
   * through here — the data update and the advanced-settings toggle — because a toggle
   * that drops its own focus cannot be operated twice from the keyboard.
   */
  private rerenderKeepingUserState(): void {
    const active = document.activeElement;
    // The account register fields (#3385) are rebuilt like everything else, and
    // they hold a name the user is part-way through typing. Capturing every
    // field's text — not only the focused one — is what keeps a refresh landing
    // from another action (a register elsewhere, a login closing) from eating a
    // draft the user never submitted.
    const accountDrafts = this.readAccountDrafts();
    const accountAgentFocused = active?.getAttribute("aria-label") === "Account agent";
    const accountSummaryFocused = active === this.el.querySelector(".af-account-disclosure summary");
    const focusedAccount =
      active instanceof HTMLInputElement ? active.getAttribute(ACCOUNT_INPUT_ATTR) : null;
    const accountCaret = focusedAccount !== null && active instanceof HTMLInputElement
      ? { start: active.selectionStart, end: active.selectionEnd }
      : null;
    const wasEditing = this.editingInput !== null && active === this.editingInput;
    // Both ends, not just the start: a rebuild landing while text is SELECTED would
    // otherwise silently collapse the selection the user was about to overwrite.
    const caretStart = wasEditing ? (this.editingInput?.selectionStart ?? null) : null;
    const caretEnd = wasEditing ? (this.editingInput?.selectionEnd ?? null) : null;
    const wasToggle = this.advancedToggle !== null && active === this.advancedToggle;
    // The explain trigger is rendered on every row, so the one that held focus
    // is named by its data-key — restored below whether the disclosure it drove
    // ended the rebuild open or closed.
    const explainFocusedKey =
      active instanceof HTMLElement && active.classList.contains("af-config-explain-trigger")
        ? active.getAttribute("data-key")
        : null;
    // The pane's own checkbox/select rows commit on `change`, and a text row the
    // user has only CLICKED into — before the first keystroke — commits nothing of
    // its own, but an UNRELATED rebuild can still land on it (a sibling's save, the
    // config view's own entry refetches, an accounts refresh). None of these is
    // tracked as `editingInput`: the text path assigns that only on the `input`
    // event, i.e. once the user has actually typed, and `editingInput` never names a
    // checkbox or select at all. So the rebuild dropped focus to <body> and the
    // user's next `[`/`]` became a view-cycle shortcut (other keystrokes silently
    // swallowed) — the exact body-focus hazard the rest of this method exists to
    // prevent. The controls already carry `aria-label = e.key`, so capture the
    // focused control's key here and re-locate its replacement after the rebuild.
    // Scoped to `.af-config-list` so the account-agent select (which lives in the
    // accounts section and has its own branch below) is never matched. The
    // `wasEditing` check above is first in the ladder, so a TYPED text row keeps the
    // caret-preserving branch; this branch only catches the untyped, click-focused
    // text case (plus checkbox/select, which `editingInput` never names) and is the
    // sole restorer for it. A checkbox/select has no caret; a click-focused text
    // input, however, may sit at a non-default position or selection, and that is
    // captured just below (`focusedConfigTextCaret`) and restored after the rebuild.
    const focusedConfigControlKey =
      active instanceof HTMLElement
        && active.closest(".af-config-list") !== null
        && (active instanceof HTMLSelectElement
          || (active instanceof HTMLInputElement
            && (active.type === "checkbox" || active.type === "text")))
        ? active.getAttribute("aria-label")
        : null;
    // A click-focused TEXT row may sit at a non-default caret (the user clicked a
    // position, or selected a substring) before the first keystroke assigns
    // `editingInput`; the `wasEditing` branch above does not cover it. Capture both
    // ends here so the rebuilt input resumes at the same place — the user's first
    // keystroke then inserts at, or replaces, exactly what they aimed at. A checkbox
    // has no selection; a select's value is its own state, so only the text case
    // carries a caret worth restoring.
    const focusedConfigTextCaret =
      focusedConfigControlKey !== null
        && active instanceof HTMLInputElement
        && active.type === "text"
        ? { start: active.selectionStart, end: active.selectionEnd }
        : null;
    // The row's Save button (button.af-config-save) is the primary commit gesture
    // for a text row — the only row kind that renders a Save button (checkbox and
    // select rows commit on `change` and return early in renderControl). A click,
    // or a Tab onto it followed by Enter, leaves the button holding DOM focus, and
    // its own commit() rebuilds the pane and detaches that focused button. The
    // captures above do not name it: `wasEditing` gates on `active ===
    // editingInput` (false here — the button, not the input, holds focus), and
    // `focusedConfigControlKey` gates on select/checkbox/text INPUT while the
    // button itself carries no `aria-label`. So a save via the Save button left the
    // restoration ladder with no matching rung and focus silently fell to <body> —
    // the exact hazard this method's own header comment warns about (the next
    // `[`/`]` then cycled the top-level view; other printable keys were swallowed).
    // Capture the row's key from its input's `aria-label` (set to `e.key` on every
    // text row) and re-locate the rebuilt row's input after the rebuild, the same
    // `.af-config-list [aria-label=…]` lookup the `focusedConfigControlKey` branch
    // uses — restored below as that branch's sibling.
    const saveFocusedKey =
      active instanceof HTMLButtonElement
        && active.closest(".af-config-list") !== null
        && active.classList.contains("af-config-save")
        ? active.closest(".af-config-control")?.querySelector("input")?.getAttribute("aria-label") ?? null
        : null;
    // The Accounts section's two commit buttons — Register (button.af-accounts-add)
    // and Log in (button.af-accounts-login) — live OUTSIDE `.af-config-list`, so the
    // Save-button rung above (gated on `closest(".af-config-list")`) never matches
    // them, and their own actions rebuild the pane while the clicked button still
    // holds focus. The login's "Starting the … login…" status write is synchronous
    // inside the click dispatch (index.ts doOpenAccountLogin → setAccountStatus →
    // store.set, whose listeners fire synchronously — see store.ts); the register's
    // status write lands in the RPC response's `.then`/`.catch`, while the button —
    // not the input — holds focus. In both cases `replaceChildren` detaches the
    // focused button and, with no matching capture or restore rung, focus fell to
    // <body> — the exact body-focus hazard this method exists to prevent (the next
    // `[`/`]` then cycled the top-level view out of config). The two rungs below name
    // them, keyed per row so a multi-account roster restores the RIGHT button: the
    // Log in button's row carries `data-account="agent/name"`, and the Register
    // button's form carries its agent on the input (`ACCOUNT_INPUT_ATTR`). Scoped to
    // the accounts buttons' own classes (not the Save button's) so the rungs do not
    // collide with `saveFocusedKey`'s `.af-config-save` match.
    const accountsLoginFocusedKey =
      active instanceof HTMLButtonElement
        && active.classList.contains("af-accounts-login")
        ? active.closest("[data-account]")?.getAttribute("data-account") ?? null
        : null;
    const accountsAddFocusedAgent =
      active instanceof HTMLButtonElement
        && active.classList.contains("af-accounts-add")
        ? active.closest(".af-accounts-register")?.querySelector<HTMLInputElement>(`input[${ACCOUNT_INPUT_ATTR}]`)?.getAttribute(ACCOUNT_INPUT_ATTR) ?? null
        : null;
    // The Hosts section's fields (#5571 W10.5) get the same treatment: the add
    // form holds a half-typed draft a refresh must not eat, and its buttons'
    // own actions rebuild the pane while they still hold focus. Drafts are
    // keyed by HOST_INPUT_ATTR; buttons by their class + row's data-host so a
    // multi-host roster restores the right one — and the add-form's submit by
    // its class alone (its draft's identity lives in the fields, not the
    // button).
    const hostDrafts = this.readHostDrafts();
    const focusedHostField =
      active instanceof HTMLInputElement || active instanceof HTMLSelectElement
        ? active.getAttribute(HOST_INPUT_ATTR)
        : null;
    const hostCaret = focusedHostField !== null && active instanceof HTMLInputElement
      && (active.type === "text" || active.type === "number")
      ? { start: active.selectionStart, end: active.selectionEnd }
      : null;
    const hostSummaryFocused = active === this.el.querySelector(".af-host-disclosure summary");
    const focusedHostButton =
      active instanceof HTMLButtonElement
        && (active.classList.contains("af-hosts-check")
          || active.classList.contains("af-hosts-remove")
          || active.classList.contains("af-hosts-add")
          || active.classList.contains("af-hosts-check-all"))
        ? {
          cls: ["af-hosts-check", "af-hosts-remove", "af-hosts-add", "af-hosts-check-all"].find((c) => active.classList.contains(c))!,
          host: active.closest("[data-host]")?.getAttribute("data-host") ?? null,
        }
        : null;
    // The config list is always the same list — one global manifest — so a rebuild
    // never legitimately starts at the top. Keeping the place matters most right after
    // a save, which is exactly when this fires.
    // Which ROW to hand focus back to is decided here, from the element that
    // actually holds it, and never from `status.key`. Saves are serialized per key
    // (`createKeyedQueue` in index.ts), so two keys in flight together can land out
    // of order: save A, move to B, save B, B answers first. A's later status would
    // name row A while the user is sitting in row B — and re-pointing by status key
    // would then move focus and the captured caret into A, so the next keystroke
    // edits the wrong setting. The key that held focus is the only thing that
    // answers "where was the user", which is the question restoration is asking.
    // aria-label carries it because render() sets it to `e.key` on every row input.
    this.restoreKey = wasEditing ? this.editingInput?.getAttribute("aria-label") ?? null : null;
    rebuildKeepingScroll(this.el, CONFIG_LIST_TOKEN, CONFIG_LIST_TOKEN, () => this.render());
    this.restoreKey = null;
    // preventScroll on BOTH: focus() scrolls its target into view by default, which
    // would undo the offset rebuildKeepingScroll just restored. That is not a corner
    // case — a user who wheels the pane while a field still holds focus is exactly the
    // reader this change exists for, and a plain focus() would yank them back to the
    // field the moment a save or an external refresh landed.
    this.restoreAccountDrafts(accountDrafts);
    this.restoreHostDrafts(hostDrafts);
    if (wasEditing && this.editingInput) {
      this.editingInput.focus({ preventScroll: true });
      if (caretStart !== null) {
        this.editingInput.setSelectionRange(caretStart, caretEnd ?? caretStart);
      }
    } else if (wasToggle && this.advancedToggle) {
      this.advancedToggle.focus({ preventScroll: true });
    } else if (explainFocusedKey !== null) {
      this.el
        .querySelector<HTMLElement>(`.af-config-explain-trigger[data-key="${CSS.escape(explainFocusedKey)}"]`)
        ?.focus({ preventScroll: true });
    } else if (focusedConfigControlKey !== null) {
      // Hand focus back to the rebuilt checkbox/select/text control for the key
      // that genuinely held it. preventScroll for the same reason as above: focus()
      // would otherwise undo the offset rebuildKeepingScroll just restored.
      const restored = this.el.querySelector<HTMLElement>(
        `.af-config-list [aria-label="${CSS.escape(focusedConfigControlKey)}"]`,
      );
      restored?.focus({ preventScroll: true });
      // A click-focused text field may have a non-default caret or selection; put
      // it back where the user left it so the first keystroke lands correctly. A
      // checkbox/select has nothing to restore, so the gate narrows to text inputs.
      if (
        restored instanceof HTMLInputElement
        && restored.type === "text"
        && focusedConfigTextCaret?.start != null
      ) {
        restored.setSelectionRange(
          focusedConfigTextCaret.start,
          focusedConfigTextCaret.end ?? focusedConfigTextCaret.start,
        );
      }
    } else if (saveFocusedKey !== null) {
      // A save committed via the row's Save button rebuilt the pane while the
      // button held focus; the focused button was detached and, with no matching
      // restoration rung, focus fell to <body> (the body-focus hazard this method
      // exists to prevent). Hand focus to the rebuilt row's text input — the
      // control the user was operating — via its `aria-label`, reusing the
      // `focusedConfigControlKey` branch's lookup + preventScroll. The just-saved
      // row's edit is closed (`editing` cleared in update()), so this does not
      // re-open it; the rebuilt input shows the daemon's re-read value and its Save
      // button is again disabled via `canCommit`.
      this.el
        .querySelector<HTMLElement>(
          `.af-config-list [aria-label="${CSS.escape(saveFocusedKey)}"]`,
        )
        ?.focus({ preventScroll: true });
    } else if (accountAgentFocused) {
      this.el.querySelector<HTMLElement>('[aria-label="Account agent"]')?.focus({ preventScroll: true });
    } else if (accountSummaryFocused) {
      this.el.querySelector<HTMLElement>(".af-account-disclosure summary")?.focus({ preventScroll: true });
    } else if (focusedAccount !== null) {
      const field = this.accountInput(focusedAccount);
      // preventScroll for the same reason as above: focus() would otherwise undo
      // the offset rebuildKeepingScroll just restored.
      field?.focus({ preventScroll: true });
      if (field && accountCaret?.start != null) {
        field.setSelectionRange(accountCaret.start, accountCaret.end ?? accountCaret.start);
      }
    } else if (accountsLoginFocusedKey !== null) {
      // Hand focus back to the rebuilt "Log in" button for the same account row the
      // user clicked — keyed by `data-account` so a multi-account roster restores the
      // right one, not the first match. preventScroll for the same reason as every
      // other rung: focus() would otherwise undo the scroll offset
      // rebuildKeepingScroll just restored. For the login flow that proceeds to an
      // overlay terminal, the later `term.focus()` overwrites this — the desired
      // outcome in both branches.
      this.el
        .querySelector<HTMLElement>(
          `.af-accounts-row[data-account="${CSS.escape(accountsLoginFocusedKey)}"] .af-accounts-login`,
        )
        ?.focus({ preventScroll: true });
    } else if (accountsAddFocusedAgent !== null) {
      // Hand focus back to the rebuilt "Register" button for the agent whose form the
      // user submitted from. There is one register form per agent — only the selected
      // agent's is visible — so the agent the input carries is the identity of the
      // right button. `this.accountInput(agent)` re-locates the rebuilt form's input
      // (it already escapes the agent), and the form's sibling button is the one to
      // re-focus, mirroring the Save-button rung's re-locate-then-focus shape.
      this.accountInput(accountsAddFocusedAgent)
        ?.closest(".af-accounts-register")
        ?.querySelector<HTMLElement>(".af-accounts-add")
        ?.focus({ preventScroll: true });
    } else if (hostSummaryFocused) {
      this.el.querySelector<HTMLElement>(".af-host-disclosure summary")?.focus({ preventScroll: true });
    } else if (focusedHostField !== null) {
      const field = this.hostInput(focusedHostField);
      field?.focus({ preventScroll: true });
      if (field instanceof HTMLInputElement && hostCaret?.start != null
        && (field.type === "text" || field.type === "number")) {
        field.setSelectionRange(hostCaret.start, hostCaret.end ?? hostCaret.start);
      }
    } else if (focusedHostButton !== null) {
      // The row-keyed rung: check-all and the add-form's submit have no
      // data-host row, so they restore by class alone; a row's Check/Remove
      // restores within its own `data-host` so the rebuilt button is the one
      // the user pressed, not the first match.
      const scope = focusedHostButton.host !== null
        ? `[data-host="${CSS.escape(focusedHostButton.host)}"] `
        : "";
      this.el
        .querySelector<HTMLElement>(`${scope}button.${focusedHostButton.cls}`)
        ?.focus({ preventScroll: true });
    }
  }

  /** Every register field's current text, keyed by agent. */
  private readAccountDrafts(): Map<string, string> {
    const drafts = new Map<string, string>();
    for (const field of this.el.querySelectorAll<HTMLInputElement>(`input[${ACCOUNT_INPUT_ATTR}]`)) {
      const agent = field.getAttribute(ACCOUNT_INPUT_ATTR);
      if (agent !== null && field.value !== "") {
        drafts.set(agent, field.value);
      }
    }
    return drafts;
  }

  /** Puts them back on the freshly rendered fields. An agent whose row is gone —
   *  a roster that shrank between renders — simply drops its draft; there is no
   *  field left to hold it. */
  private restoreAccountDrafts(drafts: Map<string, string>): void {
    for (const [agent, value] of drafts) {
      const field = this.accountInput(agent);
      if (field) {
        field.value = value;
      }
    }
  }

  private accountInput(agent: string): HTMLInputElement | null {
    return this.el.querySelector<HTMLInputElement>(
      `input[${ACCOUNT_INPUT_ATTR}="${CSS.escape(agent)}"]`,
    );
  }

  /** Every add-form field's current value, keyed by HOST_INPUT_ATTR. Checkboxes
   *  record their checked flag rather than a value; selects record the chosen
   *  option. Skipping EMPTY text fields would lose a deliberate blank, so they
   *  are recorded unconditionally — the restore re-applies "" harmlessly. */
  private readHostDrafts(): Map<string, { value: string; checked: boolean }> {
    const drafts = new Map<string, { value: string; checked: boolean }>();
    for (const field of this.el.querySelectorAll<HTMLInputElement | HTMLSelectElement>(`[${HOST_INPUT_ATTR}]`)) {
      const key = field.getAttribute(HOST_INPUT_ATTR);
      if (key === null) continue;
      const checked = field instanceof HTMLInputElement && field.type === "checkbox" ? field.checked : false;
      drafts.set(key, { value: field.value, checked });
    }
    return drafts;
  }

  /** Puts them back on the freshly rendered fields. A field that is gone — the
   *  capability gate closed the form mid-draft — simply drops its draft. */
  private restoreHostDrafts(drafts: Map<string, { value: string; checked: boolean }>): void {
    for (const [key, draft] of drafts) {
      const field = this.hostInput(key);
      if (field === null) continue;
      if (field instanceof HTMLInputElement && field.type === "checkbox") {
        field.checked = draft.checked;
      } else {
        field.value = draft.value;
      }
    }
  }

  /** Retires the committed add-form draft field by field (#6861): a field
   *  still holding exactly what was sent clears, while one retyped since keeps
   *  its new text — that text is the NEXT host's draft, and clearing it would
   *  be the same silent loss the draft mechanism exists to prevent. Runs before
   *  readHostDrafts so retired values are never snapshotted for a replay.
   *
   *  `message` names the send this success answers (`saved host <name> (…)` —
   *  host names carry no spaces or parens, config/hosts.go hostNameRE), so an
   *  add landing while another is still in flight retires its own draft rather
   *  than whichever send happened to be last. When the name cannot be matched
   *  — the format drifting, or a send this pane never saw — the oldest pending
   *  snapshot is the best available answer. */
  private retireCommittedHostAdd(message: string): void {
    const named = /^saved host ([a-z0-9-]+)/.exec(message)?.[1];
    const key = named !== undefined && this.committedHostAdds.has(named)
      ? named
      : this.committedHostAdds.keys().next().value;
    if (key === undefined) {
      return;
    }
    const committed = this.committedHostAdds.get(key)!;
    this.committedHostAdds.delete(key);
    for (const [field, sent] of committed) {
      const input = this.hostInput(field);
      if (input === null) {
        continue;
      }
      if (input instanceof HTMLInputElement && input.type === "checkbox") {
        if (input.checked === sent.checked) {
          input.checked = false;
        }
      } else if (input.value === sent.value) {
        input.value = "";
      }
    }
  }

  private hostInput(key: string): HTMLInputElement | HTMLSelectElement | null {
    return this.el.querySelector<HTMLInputElement | HTMLSelectElement>(
      `[${HOST_INPUT_ATTR}="${CSS.escape(key)}"]`,
    );
  }

  private render(): void {
    // Dropped first: every rebuild replaces the controls, and a handle to a previous
    // one would name a detached node for the rest of this render.
    this.editingInput = null;
    this.advancedToggle = null;
    const head = h(
      "div",
      { class: "af-config-head" },
      h("span", { class: "af-config-title" }, "Config"),
      h("span", { class: "af-view-count" }, String(this.entries.length)),
    );
    if (this.path !== "") {
      // Name the file being edited: a user with AF_HOME set is otherwise left
      // guessing which config.toml this is.
      head.append(h("span", { class: "af-config-path" }, `Daemon ${location.host} · ${this.path}`));
    }
    // The conversational assistant (#2467): a chat that helps configure the service,
    // the web counterpart of the TUI's config-agent takeover. Opening it is the
    // shell's job (it owns the token + modal host), so the button only reports intent.
    const assistantBtn = h(
      "button",
      { type: "button", class: "af-ghost af-config-assistant-btn" },
      "Configure with assistant",
    );
    assistantBtn.addEventListener("click", () => this.actions.openAssistant());
    head.append(assistantBtn);

    const sections: HTMLElement[] = [];
    for (const { tier, name } of tiersInOrder(this.entries)) {
      const inTier = this.entries.filter((e) => e.tier === tier);
      if (inTier.length === 0) {
        continue;
      }
      const folded = tier === TIER_ADVANCED && !this.showAdvanced;
      const heading = h("div", { class: "af-config-tier" }, h("span", { class: "af-config-tier-name" }, name.charAt(0).toUpperCase() + name.slice(1)));

      if (tier === TIER_ADVANCED) {
        const toggle = h(
          "button",
          { type: "button", class: "af-ghost af-config-toggle" },
          folded ? `Show ${inTier.length} advanced settings` : "Hide advanced settings",
        );
        toggle.addEventListener("click", () => {
          this.showAdvanced = !this.showAdvanced;
          this.rerenderKeepingUserState();
        });
        this.advancedToggle = toggle;
        heading.append(toggle);
      }
      sections.push(heading);
      if (folded) {
        continue;
      }
      for (const e of inTier) {
        sections.push(this.renderRow(e));
      }
    }

    const content =
      sections.length > 0
        ? sections
        : [
            h(
              "p",
              { class: "af-config-empty" },
              "No settings available. Try Configure with assistant.",
            ),
          ];
    // Hosts sits between the config list and the non-config sections: it
    // manages the `hosts`/`default_host` keys, so it is closer in kind to the
    // list than to Accounts. Accounts LAST, then Usage (#2983): the config
    // keys are what this view is for, and daemon-reported sections above them
    // would push them below the fold.
    this.el.replaceChildren(
      head,
      h("div", { class: "af-config-list" }, ...this.renderBrowser(), ...content),
      renderHostsSection(this.entries, this.hosts, this.hostActions, this.hostDisclosure),
      renderAccountsSection(this.accounts, this.actions.accounts, this.registration),
      renderQuotaSection(this.quota),
    );
  }

  /** Theme and the saved token live in this browser's localStorage, not in
   *  config.toml, so neither goes through `save`. */
  private renderBrowser(): HTMLElement[] {
    const heading = h("div", { class: "af-config-tier" }, h("span", { class: "af-config-tier-name" }, "This browser"));
    const select = h("select", { class: "af-input af-config-input af-theme-select" });
    select.setAttribute("aria-label", "Theme");
    for (const choice of THEME_CHOICES) {
      const opt = h("option", { value: choice }, themeLabel(choice));
      opt.selected = choice === this.theme;
      select.append(opt);
    }
    select.addEventListener("change", () => this.actions.setTheme(select.value as ThemeChoice));
    const row = h(
      "div",
      { class: "af-config-row af-theme-row" },
      h(
        "div",
        { class: "af-config-label" },
        h("span", { class: "af-config-key" }, "Theme"),
        h("span", { class: "af-config-purpose" }, "Color scheme for this browser. System follows your device setting."),
      ),
      h("div", { class: "af-config-control" }, select),
    );
    const disconnect = h("button", { type: "button", class: "af-ghost af-config-disconnect" }, "Log out");
    disconnect.addEventListener("click", () => this.actions.disconnect());
    const tokenRow = h(
      "div",
      { class: "af-config-row af-disconnect-row" },
      h(
        "div",
        { class: "af-config-label" },
        h("span", { class: "af-config-key" }, "Saved token"),
        h("span", { class: "af-config-purpose" }, "Forget the token saved in this browser and go back to the login screen."),
      ),
      h("div", { class: "af-config-control" }, disconnect),
    );
    return [heading, row, tokenRow];
  }

  /** One key: its name, purpose, control, and — when it is the row just written
   *  or just refused — the echo or the error. */
  private renderRow(e: ConfigEntry): HTMLElement {
    const row = h("div", { class: "af-config-row" });
    row.setAttribute("data-key", e.key);

    const open = this.explainKey === e.key;
    // The key itself is the explain trigger (#4803): the caret marks it as a
    // disclosure, and the trace it opens is about exactly this name. Keeping the
    // affordance on the key's own line means a closed trace adds no row height —
    // the most repeated control on the page stays the quietest.
    const caret = h("span", { class: "af-config-explain-caret" }, open ? "▾" : "▸");
    caret.setAttribute("aria-hidden", "true");
    const trigger = h(
      "button",
      {
        type: "button",
        class: "af-config-key af-config-explain-trigger",
        title: "Which config layer supplied this value",
      },
      caret,
      e.key,
    );
    trigger.setAttribute("aria-expanded", String(open));
    trigger.setAttribute("aria-controls", `af-config-explain-${e.key}`);
    trigger.setAttribute("data-key", e.key);
    trigger.addEventListener("click", () => this.toggleExplain(e.key));

    const label = h(
      "div",
      { class: "af-config-label" },
      trigger,
      h("span", { class: "af-config-purpose" }, e.purpose),
    );
    row.append(label);
    row.append(this.renderControl(e));

    const status = this.status;
    // A status for a dotted LEAF (web.terminal_font_size_desktop, written by the
    // A−/A+ controls rather than this form) renders under its table's row — the
    // only row the manifest offers for it. Without the prefix match the leaf's
    // save outcome is invisible in this view entirely.
    if (status && (status.key === e.key || status.key.startsWith(`${e.key}.`))) {
      if (status.error !== "") {
        // The validator's message, verbatim — never reworded here.
        row.append(h("div", { class: "af-config-error", role: "alert" }, status.error));
      } else {
        row.append(h("div", { class: "af-config-echo" }, `set ${status.key} = ${status.value}`));
        if (status.notice !== "") {
          // The restart notice, at the moment of the edit.
          row.append(h("div", { class: "af-config-notice" }, status.notice));
        }
      }
    }
    if (open) {
      row.append(this.renderExplainSection(e));
    }
    return row;
  }

  /** The open provenance panel (#4803): the SAME candidate trace
   *  `af config get --explain` prints — which on-disk layer won, and which were
   *  shadowed, absent, or disallowed. The daemon computes the ResolvedValue;
   *  this view only formats it, so the two surfaces cannot disagree about what
   *  a layer did. Rendered only while the row's key trigger is open, so a closed
   *  row carries no trace chrome. */
  private renderExplainSection(e: ConfigEntry): HTMLElement {
    const section = h("div", { class: "af-config-explain", id: `af-config-explain-${e.key}` });
    section.append(this.renderExplainBody());
    return section;
  }

  private toggleExplain(key: string): void {
    if (this.explainKey === key) {
      this.explainKey = null;
      this.explainResp = null;
      this.explainError = "";
      this.explainBusy = false;
      this.rerenderKeepingUserState();
      return;
    }
    this.explainKey = key;
    this.explainResp = null;
    this.explainError = "";
    this.explainBusy = true;
    const generation = ++this.explainGeneration;
    this.rerenderKeepingUserState();
    this.requestExplain(key, generation);
  }

  /** Fetches one key's trace into the open disclosure. update() calls this
   *  without a generation when fresh manifest rows arrive (the trace re-resolves
   *  in place); toggleExplain passes its own so a second open supersedes the
   *  first. The settle check is the same either way: the answer paints only if
   *  it is still the newest request for the row still open. */
  private requestExplain(key: string, generation?: number): void {
    const gen = generation ?? ++this.explainGeneration;
    void this.actions.explain(key).then((outcome) => {
      // A stale answer must not paint over a newer open: the user may have
      // explained another key — or closed this one — while the fetch flew.
      if (gen !== this.explainGeneration || this.explainKey !== key) {
        return;
      }
      this.explainBusy = false;
      // Exclusive: the body renders the error first, so a stale one would mask
      // a fresh trace — and a stale trace would do the same to a fresh error.
      // Whichever this newest answer is, it is the only one still standing.
      if (outcome.ok) {
        this.explainResp = outcome.resp;
        this.explainError = "";
      } else {
        this.explainResp = null;
        this.explainError = outcome.error;
      }
      this.rerenderKeepingUserState();
    });
  }

  /** The trace body for the open row: busy, refusal, or the ResolvedValue the
   *  daemon answered — effective value, default, merge policy, then each
   *  candidate's value, location, and verdict, plus per-leaf origins for
   *  composite keys. */
  private renderExplainBody(): HTMLElement {
    const body = h("div", { class: "af-config-explain-body", role: "status" });
    if (this.explainBusy) {
      body.append(h("div", { class: "af-config-explain-line" }, "Resolving provenance…"));
      return body;
    }
    if (this.explainError !== "") {
      body.append(h("div", { class: "af-config-error", role: "alert" }, this.explainError));
      return body;
    }
    const v: ResolvedValue | undefined = this.explainResp?.explanation;
    if (!v) {
      body.append(h("div", { class: "af-config-error", role: "alert" }, "The daemon returned no explanation."));
      return body;
    }

    body.append(
      h("div", { class: "af-config-explain-effective" }, `${v.key} = ${explainValueText(v.key, v.value)}`),
      h(
        "div",
        { class: "af-config-explain-line" },
        v.default ? `default: ${v.default} · ` : "",
        `policy: ${v.merge} · ${v.precedence.join(" < ")}`,
      ),
    );
    for (const cand of v.candidates) {
      const result = cand.reason !== "" ? `${cand.result} · ${cand.reason}` : cand.result;
      // Candidates format under the PARENT key — the same argument the CLI
      // passes (commands/configexplain.go formats candidate.Value with
      // value.Key), so a dotted leaf's candidates join like its own rows.
      const value = cand.present ? explainValueText(v.key, cand.value) : "—";
      const location = cand.path ? `${cand.path}:${cand.key_path}` : "compiled default";
      body.append(
        h("div", { class: `af-config-explain-cand af-config-explain-${cand.result}` },
          h("div", { class: "af-config-explain-layer" }, `${cand.layer}: ${result}`),
          h("div", { class: "af-config-explain-src" }, `${value} · ${location}`),
        ),
      );
    }
    const origins = v.origins ?? {};
    const leaves = Object.keys(origins).sort();
    if (leaves.length > 0) {
      body.append(h("div", { class: "af-config-explain-line" }, "origins:"));
      for (const leaf of leaves) {
        const origin = origins[leaf];
        const location = origin.path ? `${origin.path}:${origin.key_path}` : "compiled default";
        body.append(
          h("div", { class: "af-config-explain-src" }, `${leaf}: ${origin.layer} · ${location}`),
        );
      }
    }
    // The route is explicit about this (running_value_checked): the trace says
    // what the FILES resolve to, which is also what the next start reads — a
    // running daemon's snapshot is a different question.
    body.append(
      h("div", { class: "af-config-explain-note" }, "On-disk sources · the running daemon's value was not checked."),
    );
    return body;
  }

  /** The control for one key, chosen from the manifest's own description of it:
   *  a picker when the values are enumerated, a checkbox for a bool, and a text
   *  field otherwise. Structured rows use the compact JSON CurrentValue emits.
   *
   *  The enum and type come from the manifest; Go-side coverage separately pins
   *  every global row to the real writer so a rendered field cannot dead-end at
   *  a save the writer refuses. */
  private renderControl(e: ConfigEntry): HTMLElement {
    const kind = controlKind(e);

    if (kind === "checkbox") {
      const box = h("input", { type: "checkbox", class: "af-config-check" });
      box.checked = e.value === "true";
      box.setAttribute("aria-label", e.key);
      // A checkbox has no separate commit gesture: toggling IS the edit.
      box.addEventListener("change", () => this.actions.save(e.key, box.checked ? "true" : "false"));
      return h("div", { class: "af-config-control" }, box);
    }

    if (kind === "select") {
      const select = h("select", { class: "af-input af-config-input" });
      select.setAttribute("aria-label", e.key);
      // controlKind only returns "select" for a non-empty enum, but narrow it
      // explicitly rather than asserting: the compiler is the cheapest place to
      // keep the two in agreement.
      for (const v of e.enum ?? []) {
        const opt = h("option", { value: v }, v);
        if (v === e.value) {
          opt.selected = true;
        }
        select.append(opt);
      }
      select.addEventListener("change", () => this.actions.save(e.key, select.value));
      return h("div", { class: "af-config-control" }, select);
    }

    const input = h("input", { type: "text", class: "af-input af-config-input", autocomplete: "off" });
    // The row repaints its own draft when it has one: drafts is a per-key map,
    // so every typed row survives the rebuild — not only the one `editing`
    // names. A key absent from the map paints the manifest value.
    input.value = this.drafts.get(e.key) ?? e.value;
    if (this.editing === e.key) {
      this.editingInput = input;
    }
    // A same-key Enter-save closes this field (update() clears `editing` before the
    // rerender), so the gate above does not re-point `editingInput` at the rebuilt
    // input — and `rerenderKeepingUserState`'s `if (wasEditing && this.editingInput)`
    // restoration then no-ops, dropping focus to <body>. The user's next keystrokes
    // become document shortcuts (`[`/`]` cycle the view) or, for any other printable
    // key, are silently swallowed (`decideKey` returns `{kind:"none"}` without
    // preventDefault for a body-focused key in config view). #2955's gate covered
    // only the unrelated-rebuild case; the same-key variation was never analyzed.
    //
    // Keep the handle live for the row that HELD FOCUS so the existing restoration
    // branch hands focus (and the caret) back to its replacement. `restoreKey` is
    // read rather than `status.key` because the two diverge exactly when it matters
    // — see rerenderKeepingUserState. The `=== null` guard means an actively-edited
    // row's capture above always wins. This re-points the handle on every rebuild
    // that finds the row still focused, including the one the post-save refetch
    // triggers, so focus survives both renders. It does not re-open the edit:
    // `editing` stays null and the Save button stays disabled via `canCommit`
    // against the refreshed `e.value`.
    if (this.restoreKey === e.key && this.editingInput === null) {
      this.editingInput = input;
    }
    input.setAttribute("aria-label", e.key);

    const save = h("button", { type: "button", class: "af-primary af-config-save" }, "Save");

    const dirty = h("span", { class: "af-config-dirty", role: "status" }, "Unsaved");

    // ONE gate, both gestures. There are two ways to commit this field — the
    // button and Enter — and they must agree. Enter used to call save()
    // unconditionally while the button honored `disabled`, so pressing Enter on
    // an untouched field wrote it anyway: an echo and a restart notice for a
    // no-op, telling the user something happened when nothing did. Routing both
    // through commit() is what keeps a disabled control actually disabled.
    const commit = () => {
      if (!canCommit(input.value, e.value)) {
        return;
      }
      this.actions.save(e.key, input.value);
    };

    const syncSave = () => {
      save.disabled = !canCommit(input.value, e.value);
      dirty.hidden = save.disabled;
    };

    input.addEventListener("input", () => {
      this.editing = e.key;
      this.drafts.set(e.key, input.value);
      this.editingInput = input;
      syncSave();
    });
    input.addEventListener("keydown", (ev: KeyboardEvent) => {
      if (ev.key === "Enter") {
        ev.preventDefault();
        commit();
      }
    });
    save.addEventListener("click", commit);
    syncSave();

    return h("div", { class: "af-config-control" }, input, dirty, save);
  }
}
