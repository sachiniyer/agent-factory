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

import { h } from "./modals.js";
import type { ConfigEntry } from "./types.js";

/** What the config view can ask the shell to do. Saving is the shell's job (it
 *  owns the token and the refresh), so the pane reports intent and renders the
 *  outcome it is handed back. */
export interface ConfigActions {
  save: (key: string, value: string) => void;
}

/** The outcome of the last save, as the shell learned it from the daemon. */
export interface ConfigStatus {
  key: string;
  /** The echo — the canonical value the daemon actually wrote, which may differ
   *  from what was typed. Empty when `error` is set. */
  value: string;
  /** The daemon's own restart notice, shown verbatim. */
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
export type ControlKind = "readonly" | "checkbox" | "select" | "text";

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
 *  - `editable: false` → read-only, with `edit_hint` saying what to do instead.
 *    NOT `settable`: that is true for a dynamic family (program_overrides),
 *    meaning its LEAVES are settable, and offering the bare key as one field
 *    dead-ends at a save the writer refuses. `editable` is derived Go-side from
 *    the real allowlist.
 *  - `bool` → checkbox.
 *  - enumerated → picker. Excluded for a table, where the enum constrains entry
 *    NAMES rather than the value; offering it as a value picker would be a small
 *    lie about what the key takes.
 *  - anything else → text.
 */
export function controlKind(e: ConfigEntry): ControlKind {
  if (!e.editable) {
    return "readonly";
  }
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

export class ConfigPane {
  readonly el: HTMLElement;

  private entries: ConfigEntry[] = [];
  private path = "";
  private status: ConfigStatus | null = null;
  private showAdvanced = false;
  /** The key whose field is open, if any. Only one row edits at a time: a config
   *  write is per-key (like `af config set`), so a multi-row "save all" would
   *  imply an atomicity across keys that the writer does not offer. */
  private editing: string | null = null;
  private draft = "";

  private lastEntries: ConfigEntry[] | null = null;
  private lastStatus: ConfigStatus | null = null;

  constructor(private readonly actions: ConfigActions) {
    this.el = h("section", { class: "af-config" });
    this.el.setAttribute("aria-label", "Config");
  }

  /** Feeds the pane fresh manifest rows. Re-rendering is skipped when nothing
   *  changed, matching the rest of the shell's patch-in-place model. */
  update(entries: ConfigEntry[], path: string, status: ConfigStatus | null): void {
    if (this.lastEntries === entries && this.lastStatus === status) {
      return;
    }
    this.lastEntries = entries;
    this.lastStatus = status;
    this.entries = entries;
    this.path = path;
    this.status = status;
    // A save closes the field it came from: the value is committed, and leaving
    // it open would invite a second write of the same thing.
    if (status && !status.error && status.key === this.editing) {
      this.editing = null;
    }
    this.render();
  }

  private render(): void {
    const head = h(
      "div",
      { class: "af-config-head" },
      h("span", { class: "af-config-title" }, "Config"),
      h("span", { class: "af-view-count" }, String(this.entries.length)),
    );
    if (this.path !== "") {
      // Name the file being edited: a user with AF_HOME set is otherwise left
      // guessing which config.toml this is.
      head.append(h("span", { class: "af-config-path" }, this.path));
    }

    const sections: HTMLElement[] = [];
    for (const { tier, name } of tiersInOrder(this.entries)) {
      const inTier = this.entries.filter((e) => e.tier === tier);
      if (inTier.length === 0) {
        continue;
      }
      const folded = tier === TIER_ADVANCED && !this.showAdvanced;
      const heading = h("div", { class: "af-config-tier" }, h("span", { class: "af-config-tier-name" }, name));

      if (tier === TIER_ADVANCED) {
        const toggle = h(
          "button",
          { type: "button", class: "af-ghost af-config-toggle" },
          folded ? `Show ${inTier.length} advanced settings` : "Hide advanced settings",
        );
        toggle.addEventListener("click", () => {
          this.showAdvanced = !this.showAdvanced;
          this.render();
        });
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

    this.el.replaceChildren(head, h("div", { class: "af-config-list" }, ...sections));
  }

  /** One key: its name, purpose, control, and — when it is the row just written
   *  or just refused — the echo or the error. */
  private renderRow(e: ConfigEntry): HTMLElement {
    const row = h("div", { class: "af-config-row" });
    row.setAttribute("data-key", e.key);

    const label = h(
      "div",
      { class: "af-config-label" },
      h("span", { class: "af-config-key" }, e.key),
      h("span", { class: "af-config-purpose" }, e.purpose),
    );
    row.append(label);
    row.append(this.renderControl(e));

    const status = this.status;
    if (status && status.key === e.key) {
      if (status.error !== "") {
        // The validator's message, verbatim — never reworded here.
        row.append(h("div", { class: "af-config-error" }, status.error));
      } else {
        row.append(h("div", { class: "af-config-echo" }, `set ${status.key} = ${status.value}`));
        if (status.notice !== "") {
          // The restart notice, at the moment of the edit.
          row.append(h("div", { class: "af-config-notice" }, status.notice));
        }
      }
    }
    return row;
  }

  /** The control for one key, chosen from the manifest's own description of it:
   *  a picker when the values are enumerated, a checkbox for a bool, a text
   *  field otherwise — and a read-only value when `af config set` will not take
   *  the key at all.
   *
   *  The mapping reads `settable` and `enum` from the manifest rather than
   *  deciding locally, because both are pinned Go-side against the real
   *  allowlist. A form that offered a field the writer would refuse is a dead
   *  end the user only discovers by pressing save. */
  private renderControl(e: ConfigEntry): HTMLElement {
    const kind = controlKind(e);

    if (kind === "readonly") {
      return h(
        "div",
        { class: "af-config-control" },
        h("code", { class: "af-config-value" }, e.value),
        h("span", { class: "af-config-readonly" }, e.edit_hint ?? "hand-edited in config.toml"),
      );
    }

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
    input.value = this.editing === e.key ? this.draft : e.value;
    input.setAttribute("aria-label", e.key);

    const save = h("button", { type: "button", class: "af-primary af-config-save" }, "Save");

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
    };

    input.addEventListener("input", () => {
      this.editing = e.key;
      this.draft = input.value;
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

    return h("div", { class: "af-config-control" }, input, save);
  }
}
