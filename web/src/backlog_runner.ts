// The Backlog view's runner bar (backlog proposal, ui.md "Runner bar"): the
// runner switch, max running, plan use of the window closest to the limit,
// the paused reason with its main action, Run next, and the ⋯ settings. Its
// controls live across renders so a backlog.spend event never takes focus or
// a half-typed value.

import type { BudgetUpdate } from "./backlog_api.js";
import { type BacklogView, type Budget, formatID } from "./backlog_model.js";
import { type Usage, WINDOW_LABELS, pct, tighterWindow, usageHover, usageLines, usd, webReason } from "./backlog_usage.js";
import { h } from "./dom.js";

export interface RunnerActions {
  setRunner(on: boolean): void;
  setMaxRunning(n: number): void;
  setOnDone(onDone: string): void;
  /** Settles when the write and the read after it have landed. */
  setBudget(update: BudgetUpdate): Promise<unknown> | void;
  /** One runner pass now. Absent while the daemon has no RunBacklog route: the button is then not drawn. */
  runNext?(): void;
  openItem(id: number): void;
}

const MAX_RUNNING = 8;

const RUNNER_STATES: Record<string, string> = {
  off: "Runner off",
  idle: "Runner on",
  running: "Runner on",
  waiting: "Runner waiting",
  stopped: "Runner paused",
};

export interface RunnerWords {
  state: string;
  reason: string;
  warn: boolean;
}

export function runnerWords(view: BacklogView): RunnerWords {
  const r = view.runner;
  return {
    state: RUNNER_STATES[r.state] ?? "Runner on",
    reason: webReason(r.reason ?? "").replace(/\.$/, ""),
    warn: r.state === "stopped" || r.state === "waiting",
  };
}

/** The item a paused runner's main action opens: the first unmeterable run
 *  of this project. */
function unmeterableItem(view: BacklogView, usage: Usage | null): number | null {
  const sessions = Object.keys(usage?.unmeterable ?? {});
  for (const it of view.backlog.items) {
    if ((it.runs ?? []).some((r) => sessions.includes(r.session_id))) return it.id;
  }
  const m = /\bB-(\d+)\b/.exec(view.runner.reason ?? "");
  return m ? Number(m[1]) : null;
}

/** The paused runner's main action: the limit gate changes the limit, an
 *  unmeterable run opens its item, no reading runs the next item. */
export type MainAction = { kind: "limit" } | { kind: "open"; id: number } | { kind: "run" } | null;

export function mainAction(view: BacklogView, usage: Usage | null): MainAction {
  if (view.runner.state !== "stopped" && view.runner.state !== "waiting") return null;
  switch (view.runner.gate) {
    case "limit":
      return { kind: "limit" };
    case "no_reading":
      return { kind: "run" };
    case "unmeterable": {
      const id = unmeterableItem(view, usage);
      return id === null ? null : { kind: "open", id };
    }
  }
  return null;
}

type BudgetKey = keyof BudgetUpdate;

interface BudgetField {
  key: BudgetKey;
  label: string;
  unit: string;
  max: number;
  step: number;
}

const BUDGET_FIELDS: readonly BudgetField[] = [
  { key: "limit", label: "Plan limit", unit: "% of either window, any use · new runs start only below it", max: 100, step: 1 },
  { key: "max_per_run", label: "Cap per run", unit: "% of the 5-hour window", max: 100, step: 1 },
  { key: "usd_limit", label: "Dollar limit", unit: "$ per window at API prices · 0 is off", max: 1_000_000, step: 0.01 },
];

export class RunnerBar {
  readonly el: HTMLElement;
  private readonly toggle: HTMLButtonElement;
  private readonly toggleLabel = h("span", { class: "af-backlog-runner-state" });
  private readonly reason = h("span", { class: "af-backlog-reason" });
  private readonly slots: HTMLSelectElement;
  private readonly plan = h("button", { type: "button", class: "af-backlog-plan" });
  private readonly planDetail = h("div", { class: "af-backlog-plan-detail", hidden: true });
  private slotsKey = "";
  private readonly main: HTMLButtonElement;
  private readonly run: HTMLButtonElement;
  private readonly menu: HTMLDetailsElement;
  private readonly inputs = new Map<keyof BudgetUpdate, HTMLInputElement>();
  private readonly onDone: HTMLSelectElement;
  private view: BacklogView | null = null;
  private budget: Budget | null = null;
  // Budget values asked for and not yet acknowledged, by field, and the chain that sends them.
  private readonly pendingBudget = new Map<BudgetKey, { v: number; rev: number }>();
  private budgetRev = 0;
  private budgetQueue: Promise<unknown> = Promise.resolve();
  private usage: Usage | null = null;
  private action: MainAction = null;

  constructor(private readonly actions: RunnerActions, private readonly now: () => number = Date.now) {
    this.toggle = h("button", { type: "button", class: "af-backlog-toggle" },
      h("span", { class: "af-backlog-toggle-knob", ariaHidden: "true" }), this.toggleLabel);
    this.toggle.setAttribute("role", "switch");
    this.toggle.setAttribute("aria-label", "Runner");
    this.toggle.addEventListener("click", () => {
      if (this.view) actions.setRunner(!this.view.backlog.settings.runner);
    });
    this.slots = h("select", { class: "af-backlog-slots" });
    this.slots.setAttribute("aria-label", "Max running");
    this.slots.addEventListener("change", () => actions.setMaxRunning(Number(this.slots.value)));
    const hover = (): void => { if (this.usage && this.budget) this.plan.title = usageHover(this.usage, this.budget, this.now()); };
    this.plan.addEventListener("pointerenter", hover);
    this.plan.setAttribute("aria-expanded", "false");
    this.plan.addEventListener("click", () => {
      this.planDetail.hidden = !this.planDetail.hidden;
      this.plan.setAttribute("aria-expanded", String(!this.planDetail.hidden));
      this.fillDetail();
    });
    this.main = h("button", { type: "button", class: "af-primary", hidden: true });
    this.main.addEventListener("click", () => this.runMain());
    this.run = h("button", { type: "button", class: "af-ghost" }, "Run next");
    this.run.hidden = actions.runNext === undefined;
    this.run.addEventListener("click", () => actions.runNext?.());
    this.onDone = h("select", { class: "af-backlog-move-select" },
      h("option", { value: "keep" }, "Keep session"), h("option", { value: "archive" }, "Archive session"));
    this.onDone.setAttribute("aria-label", "When an item is done");
    this.onDone.addEventListener("change", () => actions.setOnDone(this.onDone.value));
    this.menu = h("details", { class: "af-backlog-settings" }, this.menuSummary(), this.menuPanel());
    this.el = h("div", { class: "af-backlog-head" },
      h("span", { class: "af-backlog-heading" }, "Backlog"),
      h("div", { class: "af-backlog-runner" }, this.toggle, this.reason, this.slots, this.plan),
      h("div", { class: "af-backlog-runner-actions" }, this.main, this.run, this.menu), this.planDetail);
  }

  private menuSummary(): HTMLElement {
    const s = h("summary", { class: "af-ghost af-backlog-more" }, "⋯");
    s.setAttribute("aria-label", "Runner and budget settings");
    return s;
  }

  private menuPanel(): HTMLElement {
    const fields = BUDGET_FIELDS.map((f) => {
      const input = h("input", { type: "number", class: "af-backlog-number", min: "0", max: String(f.max), step: String(f.step) });
      input.setAttribute("aria-label", f.label);
      input.addEventListener("change", () => {
        const v = Number(input.value);
        if (input.value.trim() === "" || !Number.isFinite(v) || v < 0 || v > f.max) {
          this.fill();
          return;
        }
        // Against the value last asked for: the budget shown is the daemon's, which still holds
        // the old one while a write is out, so a change back would otherwise be taken for no change.
        const current = this.pendingBudget.get(f.key)?.v ?? this.budget?.[f.key];
        if (current === v) return;
        const rev = ++this.budgetRev;
        this.pendingBudget.set(f.key, { v, rev });
        this.writeBudget(f.key, v, rev);
      });
      this.inputs.set(f.key, input);
      return h("label", { class: "af-backlog-setting" },
        h("span", { class: "af-backlog-label" }, f.label), input, h("span", { class: "af-backlog-reason" }, f.unit));
    });
    return h("div", { class: "af-backlog-settings-panel" },
      h("p", { class: "af-backlog-reason" }, "Budget · all projects"), ...fields,
      h("p", { class: "af-backlog-reason" }, "Runner · this project"),
      h("label", { class: "af-backlog-setting" }, h("span", { class: "af-backlog-label" }, "When an item is done"), this.onDone));
  }

  /** Writes go out one at a time, in the order they were made, so the last edit of a field is the
   *  one the daemon ends up with. */
  private writeBudget(key: BudgetKey, v: number, rev: number): void {
    // An acknowledgement retires its own edit only: an older write of the same value must not
    // retire the draft of a later one.
    const done = (): void => {
      if (this.pendingBudget.get(key)?.rev === rev) this.pendingBudget.delete(key);
      this.fill();
    };
    this.budgetQueue = this.budgetQueue
      .then(() => this.actions.setBudget({ [key]: v }))
      .then(done, done);
  }

  private runMain(): void {
    const a = this.action;
    if (!a) return;
    if (a.kind === "run") this.actions.runNext?.();
    else if (a.kind === "open") this.actions.openItem(a.id);
    else {
      this.menu.open = true;
      this.inputs.get("limit")?.focus();
    }
  }

  update(view: BacklogView, budget: Budget | null, usage: Usage | null, busy: boolean): void {
    this.view = view;
    this.budget = budget;
    this.usage = usage;
    const words = runnerWords(view);
    const on = view.backlog.settings.runner;
    this.toggle.setAttribute("aria-checked", String(on));
    this.toggle.classList.toggle("af-backlog-toggle-on", on);
    this.toggleLabel.textContent = words.state;
    this.toggleLabel.classList.toggle("af-backlog-warn", words.warn);
    this.reason.textContent = words.reason ? `· ${words.reason}` : "";
    this.fillSlots(view);
    this.fillPlan();
    this.action = mainAction(view, usage);
    const a = this.action;
    this.main.hidden = a === null || a.kind === "run";
    this.main.textContent = a?.kind === "open" ? `Open ${formatID(a.id)}` : "Change limit";
    this.run.className = a?.kind === "run" ? "af-primary" : "af-ghost";
    this.run.disabled = busy;
    this.fill();
  }

  /** Rebuilds the options only when their words change: replacing them
   *  closes a native select the user has open. */
  private fillSlots(view: BacklogView): void {
    const used = view.runner.used;
    const max = Math.max(1, view.backlog.settings.max_running);
    const key = `${used}/${max}`;
    if (key === this.slotsKey) return;
    this.slotsKey = key;
    const top = Math.max(MAX_RUNNING, max);
    const opts = [];
    for (let n = 1; n <= top; n++) opts.push(h("option", { value: String(n) }, `${used} of ${n} working`));
    this.slots.replaceChildren(...opts);
    this.slots.value = String(max);
  }

  private fillDetail(): void {
    if (this.planDetail.hidden || !this.usage) return;
    if (!this.budget) return;
    this.planDetail.replaceChildren(...usageLines(this.usage, this.budget, this.now()).map((l) => h("div", {}, l)));
  }

  private fillPlan(): void {
    const u = this.usage;
    const b = this.budget;
    this.plan.hidden = !u || !b;
    if (!u || !b) {
      this.planDetail.hidden = true;
      return;
    }
    this.plan.title = usageHover(u, b, this.now());
    this.fillDetail();
    if (u.metering) {
      this.plan.replaceChildren("Reading plan usage…");
      return;
    }
    const k = tighterWindow(u, this.now());
    if (k === null) {
      this.plan.replaceChildren(b.usd_limit > 0 ? `Backlog limit ${usd(b.usd_limit)} per window` : `Plan limit ${pct(b.limit)}`);
      return;
    }
    const w = u[k];
    const fill = h("span", { class: "af-backlog-meter-fill" });
    fill.style.width = `${b.limit > 0 ? Math.min(100, (w.plan / b.limit) * 100) : 100}%`;
    const meter = h("span", { class: "af-backlog-meter", role: "img" }, fill);
    meter.setAttribute("aria-label", `${pct(w.plan)} of a ${pct(b.limit)} limit`);
    this.plan.replaceChildren(`Plan ${pct(w.plan)} of ${WINDOW_LABELS[k].bar} `, meter, ` limit ${pct(b.limit)}`);
  }

  private fill(): void {
    for (const [key, input] of this.inputs) {
      const shown = this.pendingBudget.get(key)?.v ?? this.budget?.[key];
      if (document.activeElement !== input && shown !== undefined) input.value = String(shown);
    }
    if (this.view && document.activeElement !== this.onDone) this.onDone.value = this.view.backlog.settings.on_done || "keep";
  }
}
