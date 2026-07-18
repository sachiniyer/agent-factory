// The TASKS view of the web client (#1592 Phase 5 PR8): the browser analogue of the
// TUI's automations / task pane (ui/task_pane.go, ui/automations.go). It lists the
// scheduled tasks the daemon owns — name, cron/watch trigger, enabled, target
// session, last-run + status — and drives their lifecycle from the browser: add,
// enable/disable (UpdateTask), trigger-now (TriggerTask), and remove (RemoveTask).
//
// Every row-level MUTATION keys off the task's globally-unique `id`, never its
// (optional, non-unique) name — the api layer's requireTaskID fails a missing id
// closed, so this view only ever hands the daemon a real target (#1592 PR8, the
// #1678 id-scoping class). The daemon is the single writer (#1029 PR3): these
// callbacks POST the RPC and let the refreshed ListTasks / task.* event update the
// list, so the pane holds no source of truth of its own.
//
// It is patched in place like the rest of the shell and CSP-safe (createElement +
// addEventListener via the shared h() helper, no innerHTML with markup).
//
// The task form's cron field is the friendly schedule PICKER (#2057 phase 2, the
// browser twin of ui/schedule_picker.go): a schedule type plus its contextual
// inputs, generating the cron underneath. Cron is still the stored/wire format —
// only the input UX changed — and the raw expression stays reachable as the
// picker's Custom type.

import { asForm, field, h, modalChrome, type ModalHandle, projectLabel } from "./modals.js";
import { PROGRAM_REPO_DEFAULT, type ProgramCatalog, type ProgramChoice, programChoices } from "./programs.js";
import { SCHEDULE_TYPE_OPTIONS, type Schedule, type ScheduleType, cron as scheduleCron, describe as scheduleDescribe, parseCron } from "./schedule.js";
import type { TaskData } from "./types.js";

/** The add-task form's inputs (a subset of task.Task the browser fills; the daemon
 *  re-validates). Exactly one of cron / watchCmd is meaningful, per `trigger`. */
export interface AddTaskInput {
  name: string;
  projectPath: string;
  trigger: "cron" | "watch";
  cron: string;
  watchCmd: string;
  prompt: string;
  targetSession: string;
  program: string;
}

/** The row-level actions the tasks pane invokes; index.ts owns the real behavior
 *  (the daemon RPC + list refresh). Each mutation is handed the whole task so the
 *  handler has its stable id and current state; the toggle turns that into a
 *  field-level `{ enabled }` patch (UpdateTask), never a full-struct write (#1700). */
export interface TaskActions {
  /** Opens the add-task modal. */
  add(): void;
  /** Opens the edit modal seeded from this task; submits the changed fields via
   *  UpdateTask (the field-level patch, #1935). */
  edit(task: TaskData): void;
  /** Flips enabled via UpdateTask. */
  toggle(task: TaskData): void;
  /** Fires the task now via TriggerTask (enabled cron tasks only). */
  trigger(task: TaskData): void;
  /** Removes the task via RemoveTask. */
  remove(task: TaskData): void;
}

/** A random 8-char (4-byte) hex task id, matching task.GenerateID's shape and the
 *  [a-zA-Z0-9_-] id class (task.ValidateTaskID). Generated CLIENT-side because the
 *  daemon's AddTask validates a non-empty id on the incoming task (it does not mint
 *  one), exactly like the CLI's `af tasks add`. Web Crypto keeps it same-origin —
 *  no fetch, so the daemon's CSP holds. */
function genTaskId(): string {
  const b = new Uint8Array(4);
  crypto.getRandomValues(b);
  return [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
}

/** Builds a task.Task from the add-form input: a fresh id + created_at, exactly one
 *  trigger set per `trigger`, and enabled=true (a new task is armed). The daemon
 *  re-validates the trigger/prompt/program contract (task.AddTask). */
export function buildTask(input: AddTaskInput): TaskData {
  return {
    id: genTaskId(),
    name: input.name,
    prompt: input.prompt,
    cron_expr: input.trigger === "cron" ? input.cron : "",
    watch_cmd: input.trigger === "watch" ? input.watchCmd : "",
    target_session: input.targetSession,
    project_path: input.projectPath,
    program: input.program,
    enabled: true,
    created_at: new Date().toISOString(),
  };
}

/** The task's trigger as a one-line summary, mirroring the TUI's row detail
 *  (ui/automations.go rowDetail): the cron expression, or `watch: <cmd>`. */
function triggerSummary(t: TaskData): string {
  if (t.watch_cmd && t.watch_cmd.trim() !== "") {
    return `watch: ${t.watch_cmd}`;
  }
  if (t.cron_expr && t.cron_expr.trim() !== "") {
    return `cron: ${t.cron_expr}`;
  }
  return "no trigger";
}

/** Whether a task is a fire-now candidate: TriggerTask refuses disabled and watch
 *  tasks (they have no manual fire), so the Trigger action shows only for an enabled
 *  cron task — the same guard the daemon enforces, surfaced as UI. */
function canTrigger(t: TaskData): boolean {
  return t.enabled && !!t.cron_expr && t.cron_expr.trim() !== "" && !(t.watch_cmd && t.watch_cmd.trim() !== "");
}

/** The last-run line, or a dim placeholder before the first run. */
function lastRunSummary(t: TaskData): string {
  if (!t.last_run_at) {
    return "never run";
  }
  const status = t.last_run_status ? ` (${t.last_run_status})` : "";
  return `last run ${t.last_run_at}${status}`;
}

/**
 * The tasks pane: build once (its `el` mounts into the app body), then
 * update(tasks) re-renders on a change. A small stateful class like the projects
 * pane so a task.* event patches only this subtree.
 */
export class TasksPane {
  readonly el: HTMLElement;
  private lastTasks: TaskData[] | null = null;
  private lastProject: string | null = null;

  constructor(private readonly actions: TaskActions) {
    this.el = h("section", { class: "af-tasks" });
    this.el.setAttribute("aria-label", "Tasks");
  }

  /** Re-renders the tasks list SCOPED to the selected project (redesign PR2): only
   *  tasks whose project_path matches, so the tasks view operates within the same
   *  project the rail is scoped to. A null project (none exist) shows no tasks. */
  update(tasks: TaskData[], selectedProject: string | null): void {
    if (this.lastTasks === tasks && this.lastProject === selectedProject) {
      return;
    }
    this.lastTasks = tasks;
    this.lastProject = selectedProject;
    const scoped = selectedProject ? tasks.filter((t) => t.project_path === selectedProject) : [];
    this.render(scoped);
  }

  private render(tasks: TaskData[]): void {
    const addBtn = h("button", { type: "button", class: "af-tasks-add", title: "Add task" }, "+ Add");
    addBtn.addEventListener("click", () => this.actions.add());
    const head = h(
      "div",
      { class: "af-tasks-head" },
      h("span", { class: "af-tasks-title" }, "Tasks"),
      h("span", { class: "af-view-count" }, String(tasks.length)),
      addBtn,
    );
    if (tasks.length === 0) {
      this.el.replaceChildren(
        head,
        h(
          "p",
          { class: "af-tasks-empty" },
          "No scheduled tasks yet. Add one to deliver a prompt on a cron schedule.",
        ),
      );
      return;
    }
    const rows = [...tasks]
      .sort((a, b) => (a.created_at < b.created_at ? -1 : a.created_at > b.created_at ? 1 : 0))
      .map((t) => this.taskRow(t));
    this.el.replaceChildren(head, h("ul", { class: "af-tasks-list" }, ...rows));
  }

  private taskRow(t: TaskData): HTMLElement {
    const glyph = t.enabled ? "[✓]" : "[ ]";
    const enabledDot = h("span", { class: `af-task-enabled${t.enabled ? " af-task-on" : ""}` }, glyph);
    enabledDot.setAttribute("aria-hidden", "true");

    const name = h("div", { class: "af-task-name" }, t.name && t.name.trim() !== "" ? t.name : "(unnamed task)");
    const trigger = h("div", { class: "af-task-trigger" }, triggerSummary(t));
    const metaParts: string[] = [];
    if (t.target_session && t.target_session.trim() !== "") {
      metaParts.push(`→ ${t.target_session}`);
    }
    metaParts.push(lastRunSummary(t));
    const meta = h("div", { class: "af-task-meta" }, metaParts.join("  ·  "));
    const main = h("div", { class: "af-task-main" }, name, trigger, meta);

    const toggleBtn = h(
      "button",
      { type: "button", class: "af-ghost af-task-action" },
      t.enabled ? "Disable" : "Enable",
    );
    toggleBtn.addEventListener("click", () => this.actions.toggle(t));

    const editBtn = h("button", { type: "button", class: "af-ghost af-task-action" }, "Edit");
    editBtn.addEventListener("click", () => this.actions.edit(t));

    const actionEls: HTMLElement[] = [toggleBtn, editBtn];
    if (canTrigger(t)) {
      const triggerBtn = h("button", { type: "button", class: "af-ghost af-task-action" }, "Trigger");
      triggerBtn.addEventListener("click", () => this.actions.trigger(t));
      actionEls.push(triggerBtn);
    }
    const removeBtn = h("button", { type: "button", class: "af-danger af-task-action" }, "Remove");
    removeBtn.addEventListener("click", () => this.actions.remove(t));
    actionEls.push(removeBtn);

    const actions = h("div", { class: "af-task-actions" }, ...actionEls);
    return h("li", { class: "af-task-row" }, enabledDot, main, actions);
  }
}

/** Weekday toggles render Monday-first ("M T W T F S S", #2057) but map onto the
 *  Sunday-first weekday numbering the schedule model normalizes on — the same
 *  display order as the TUI picker's weekdayDisplayOrder. */
const WEEKDAY_DISPLAY: ReadonlyArray<{ weekday: number; letter: string; name: string }> = [
  { weekday: 1, letter: "M", name: "Monday" },
  { weekday: 2, letter: "T", name: "Tuesday" },
  { weekday: 3, letter: "W", name: "Wednesday" },
  { weekday: 4, letter: "T", name: "Thursday" },
  { weekday: 5, letter: "F", name: "Friday" },
  { weekday: 6, letter: "S", name: "Saturday" },
  { weekday: 0, letter: "S", name: "Sunday" },
];

/** A form row that is NOT a <label>: the picker's multi-control rows (a time, a
 *  weekday toggle group) hold several controls, and a <label> wrapping them forwards
 *  a click on its own text to the first control inside — which would silently toggle
 *  Monday when the user clicks the word "Days". Same class + look as modals' field(),
 *  which stays the right helper for a single-control row. */
function fieldGroup(label: string, control: HTMLElement): HTMLElement {
  return h("div", { class: "af-modal-field" }, h("span", { class: "af-modal-label" }, label), control);
}

/** Parses a numeric cell, clamping into [min,max] and falling back to `def` for
 *  anything unparseable — the browser twin of the TUI picker's atoiClamp. Clamping
 *  happens when the schedule is MATERIALIZED, not on every keystroke, so a mid-edit
 *  "7" on the way to "17" is never fought. */
function clampField(raw: string, min: number, max: number, def: number): number {
  const n = Number.parseInt(raw.trim(), 10);
  if (!Number.isFinite(n)) {
    return def;
  }
  return Math.min(Math.max(n, min), max);
}

/**
 * The friendly schedule picker (#2057 phase 2): the browser twin of the TUI's
 * ui/schedule_picker.go, and the cron field's replacement in the task form. A
 * schedule-type selector plus ONLY the inputs that type needs, a live plain-English
 * preview, and the generated cron shown read-only so users can see exactly what
 * gets saved.
 *
 * All model logic — cron generation, the preview text, and seeding an existing task
 * back into its matching preset — lives in the shared schedule module, which mirrors
 * the canonical Go package under a vectors-driven parity test. This class is only
 * the DOM surface over it, so the two pickers cannot drift apart in what a state
 * means, only in how it is drawn.
 *
 * Cron remains the stored/wire format: cron() is what the form submits, and Custom
 * hands back a raw expression verbatim (today's behavior, kept as the escape hatch).
 */
class SchedulePicker {
  /** The rows to append into the modal body, in visual order. */
  readonly rows: HTMLElement[];

  private readonly typeSelect: HTMLSelectElement;
  private readonly intervalInput: HTMLInputElement;
  private readonly intervalUnit: HTMLElement;
  private readonly hourInput: HTMLInputElement;
  private readonly minuteInput: HTMLInputElement;
  private readonly meridiemSelect: HTMLSelectElement;
  private readonly domInput: HTMLInputElement;
  private readonly rawInput: HTMLInputElement;
  private readonly humanLine: HTMLElement;
  private readonly cronOut: HTMLInputElement;

  private readonly intervalRow: HTMLElement;
  private readonly timeRow: HTMLElement;
  private readonly timeLabel: HTMLElement;
  private readonly hourGroup: HTMLElement;
  private readonly weekdayRow: HTMLElement;
  private readonly domRow: HTMLElement;
  private readonly rawRow: HTMLElement;
  private readonly previewRow: HTMLElement;

  private readonly weekdayButtons: HTMLButtonElement[] = [];
  private readonly weekdaysOn = new Set<number>();

  constructor() {
    this.typeSelect = h("select", { class: "af-input" });
    this.typeSelect.setAttribute("aria-label", "Schedule type");
    for (const opt of SCHEDULE_TYPE_OPTIONS) {
      this.typeSelect.append(h("option", { value: opt.type }, opt.label));
    }
    this.typeSelect.addEventListener("change", () => this.onTypeChange());

    this.intervalInput = this.numberInput("Interval", "1", "59");
    this.intervalUnit = h("span", { class: "af-schedule-unit" }, "minutes");
    this.intervalRow = fieldGroup(
      "Run every",
      h("div", { class: "af-schedule-row" }, this.intervalInput, this.intervalUnit),
    );

    this.hourInput = this.numberInput("Hour", "1", "12");
    this.minuteInput = this.numberInput("Minute", "0", "59");
    this.meridiemSelect = h("select", { class: "af-input af-schedule-meridiem" });
    this.meridiemSelect.setAttribute("aria-label", "AM/PM");
    this.meridiemSelect.append(h("option", { value: "AM" }, "AM"), h("option", { value: "PM" }, "PM"));
    this.meridiemSelect.addEventListener("change", () => this.sync());
    // Hour and AM/PM travel together: an hourly schedule keeps only the minute, so
    // the whole group hides and the row's label switches to name what is left.
    this.hourGroup = h(
      "span",
      { class: "af-schedule-clock" },
      this.hourInput,
      h("span", { class: "af-schedule-sep" }, ":"),
    );
    this.timeRow = fieldGroup("Time", h("div", { class: "af-schedule-row" }, this.hourGroup, this.minuteInput, this.meridiemSelect));
    this.timeLabel = this.timeRow.firstElementChild as HTMLElement;

    const weekdayGroup = h("div", { class: "af-weekdays" });
    weekdayGroup.setAttribute("role", "group");
    weekdayGroup.setAttribute("aria-label", "Days of the week");
    for (const day of WEEKDAY_DISPLAY) {
      // The letters repeat (T/T, S/S), so the accessible name is the full day name
      // and aria-pressed carries the state — never the glyph alone.
      const btn = h("button", { type: "button", class: "af-weekday" }, day.letter);
      btn.setAttribute("aria-label", day.name);
      btn.setAttribute("aria-pressed", "false");
      btn.addEventListener("click", () => this.toggleWeekday(day.weekday));
      this.weekdayButtons.push(btn);
      weekdayGroup.append(btn);
    }
    this.weekdayRow = fieldGroup("Days", weekdayGroup);

    this.domInput = this.numberInput("Day of month", "1", "31");
    this.domRow = field("Day of month", this.domInput);

    this.rawInput = h("input", { type: "text", class: "af-input", placeholder: "0 9 * * 1-5", autocomplete: "off" });
    this.rawInput.setAttribute("aria-label", "Cron expression");
    this.rawInput.addEventListener("input", () => this.sync());
    this.rawRow = field("Cron expression", this.rawInput);

    this.humanLine = h("p", { class: "af-schedule-human" });
    this.humanLine.setAttribute("aria-live", "polite");
    this.cronOut = h("input", { type: "text", class: "af-input af-schedule-cron", readOnly: true, tabIndex: -1 });
    this.cronOut.setAttribute("aria-label", "Generated cron");
    this.previewRow = h("div", { class: "af-schedule-preview" }, this.humanLine, this.cronOut);

    this.rows = [
      field("Schedule", this.typeSelect),
      this.intervalRow,
      this.timeRow,
      this.weekdayRow,
      this.domRow,
      this.rawRow,
      this.previewRow,
    ];

    this.reset();
  }

  private numberInput(label: string, min: string, max: string): HTMLInputElement {
    const el = h("input", { type: "number", class: "af-input af-schedule-num", min, max, autocomplete: "off" });
    el.setAttribute("aria-label", label);
    el.addEventListener("input", () => this.sync());
    return el;
  }

  /** Seeds a brand-new task: daily at 9:00 AM, with every other type's fields
   *  carrying valid defaults so switching type never lands on an empty input. The
   *  same defaults the TUI picker resets to. */
  private reset(): void {
    this.typeSelect.value = "daily";
    this.intervalInput.value = "15";
    this.hourInput.value = "9";
    this.minuteInput.value = "00";
    this.meridiemSelect.value = "AM";
    this.domInput.value = "1";
    this.rawInput.value = "";
    this.weekdaysOn.clear();
    this.weekdaysOn.add(1); // Monday
    this.sync();
  }

  /**
   * Seeds the picker from an existing task's cron: the matching preset if the
   * expression is one of the shapes the model emits, otherwise Custom holding the
   * original text verbatim. An empty expression (e.g. a watch task being switched to
   * a cron trigger) keeps the new-task defaults rather than dropping into an empty
   * Custom field.
   */
  seed(cronExpr: string): void {
    this.reset();
    if (cronExpr.trim() === "") {
      return;
    }
    const { schedule: s } = parseCron(cronExpr);
    this.typeSelect.value = s.type;
    switch (s.type) {
      case "everyNMinutes":
      case "everyNHours":
        if (s.interval !== undefined && s.interval > 0) {
          this.intervalInput.value = String(s.interval);
        }
        break;
      case "hourly":
        this.minuteInput.value = pad2(s.minute ?? 0);
        break;
      case "daily":
        this.setClock(s.hour ?? 0, s.minute ?? 0);
        break;
      case "weekly":
        this.setClock(s.hour ?? 0, s.minute ?? 0);
        this.weekdaysOn.clear();
        for (const d of s.weekdays ?? []) {
          this.weekdaysOn.add(d);
        }
        break;
      case "monthly":
        this.setClock(s.hour ?? 0, s.minute ?? 0);
        if (s.dayOfMonth !== undefined && s.dayOfMonth > 0) {
          this.domInput.value = String(s.dayOfMonth);
        }
        break;
      default: // custom — the raw expression, unchanged
        this.rawInput.value = s.raw ?? cronExpr;
        break;
    }
    this.sync();
  }

  /** Splits a 24-hour time into the 12-hour hour + AM/PM cells (to12Hour). */
  private setClock(hour24: number, minute: number): void {
    let h12 = hour24;
    let pm = false;
    if (hour24 === 0) {
      h12 = 12;
    } else if (hour24 === 12) {
      pm = true;
    } else if (hour24 > 12) {
      h12 = hour24 - 12;
      pm = true;
    }
    this.hourInput.value = String(h12);
    this.minuteInput.value = pad2(minute);
    this.meridiemSelect.value = pm ? "PM" : "AM";
  }

  private toggleWeekday(weekday: number): void {
    if (this.weekdaysOn.has(weekday)) {
      this.weekdaysOn.delete(weekday);
    } else {
      this.weekdaysOn.add(weekday);
    }
    this.sync();
  }

  private get type(): ScheduleType {
    return this.typeSelect.value as ScheduleType;
  }

  /** Switching INTO Custom prefills the raw field with the cron the previous preset
   *  generated (when it is empty), so the escape hatch starts from a working
   *  expression rather than blank — the TUI does the same. */
  private onTypeChange(): void {
    if (this.type === "custom" && this.rawInput.value.trim() === "") {
      this.rawInput.value = this.cronOut.value;
    }
    this.sync();
  }

  /** Materializes the current cell state into a canonical Schedule, clamping the
   *  numeric cells so the generated cron is always well-formed (Custom's raw text
   *  excepted — the daemon validates that). */
  schedule(): Schedule {
    switch (this.type) {
      case "everyNMinutes":
        return { type: "everyNMinutes", interval: clampField(this.intervalInput.value, 1, 59, 15) };
      case "everyNHours":
        return { type: "everyNHours", interval: clampField(this.intervalInput.value, 1, 23, 1) };
      case "hourly":
        return { type: "hourly", minute: clampField(this.minuteInput.value, 0, 59, 0) };
      case "daily":
        return { type: "daily", hour: this.hour24(), minute: clampField(this.minuteInput.value, 0, 59, 0) };
      case "weekly":
        return {
          type: "weekly",
          hour: this.hour24(),
          minute: clampField(this.minuteInput.value, 0, 59, 0),
          weekdays: [...this.weekdaysOn],
        };
      case "monthly":
        return {
          type: "monthly",
          hour: this.hour24(),
          minute: clampField(this.minuteInput.value, 0, 59, 0),
          dayOfMonth: clampField(this.domInput.value, 1, 31, 1),
        };
      default: // custom
        return { type: "custom", raw: this.rawInput.value.trim() };
    }
  }

  private hour24(): number {
    const h = clampField(this.hourInput.value, 1, 12, 12);
    const pm = this.meridiemSelect.value === "PM";
    if (pm) {
      return h === 12 ? 12 : h + 12;
    }
    return h === 12 ? 0 : h; // 12 AM = midnight
  }

  /** The cron expression the form submits — always live off the current state. */
  cron(): string {
    return scheduleCron(this.schedule());
  }

  /** Re-applies the per-type row visibility and preview. The task form calls this
   *  after showing the picker again (switching the trigger back to cron), which
   *  unhides every row wholesale. */
  refresh(): void {
    this.sync();
  }

  /** A user-facing message for an unsavable schedule, or null when it is good to
   *  save. The daemon re-validates the expression itself (it always has); these are
   *  the picker-level constraints that would otherwise generate a nonsense cron. */
  validate(): string | null {
    if (this.type === "custom" && this.rawInput.value.trim() === "") {
      return "A cron expression is required for a cron task.";
    }
    if (this.type === "weekly" && this.weekdaysOn.size === 0) {
      return "Select at least one day of the week.";
    }
    return null;
  }

  /** Shows only the cells the selected type needs, and refreshes the preview + the
   *  read-only generated cron. Runs on every edit, so what the user reads is always
   *  what a submit would store. */
  private sync(): void {
    const type = this.type;
    const isClock = type === "daily" || type === "weekly" || type === "monthly";
    this.intervalRow.hidden = type !== "everyNMinutes" && type !== "everyNHours";
    this.intervalUnit.textContent = type === "everyNHours" ? "hours" : "minutes";
    this.intervalInput.max = type === "everyNHours" ? "23" : "59";
    this.timeRow.hidden = !isClock && type !== "hourly";
    this.hourGroup.hidden = !isClock;
    this.meridiemSelect.hidden = !isClock;
    this.timeLabel.textContent = isClock ? "Time" : "Minute past the hour";
    this.weekdayRow.hidden = type !== "weekly";
    this.domRow.hidden = type !== "monthly";
    this.rawRow.hidden = type !== "custom";

    for (let i = 0; i < this.weekdayButtons.length; i++) {
      const on = this.weekdaysOn.has(WEEKDAY_DISPLAY[i].weekday);
      this.weekdayButtons[i].setAttribute("aria-pressed", on ? "true" : "false");
      this.weekdayButtons[i].classList.toggle("af-weekday-on", on);
    }

    const s = this.schedule();
    this.humanLine.textContent = scheduleDescribe(s);
    this.cronOut.value = scheduleCron(s);
  }
}

function pad2(n: number): string {
  return String(n).padStart(2, "0");
}

/**
 * The task form modal (mirrors the TUI's task editor + `af tasks add`/`update`):
 * name, project, a cron/watch trigger toggle with its value — the schedule PICKER
 * for cron (#2057), a command for watch — a prompt, an optional target session, and
 * the agent program. Shared by the ADD and EDIT flows — the only
 * difference is the chrome (title/confirm label) and, in edit mode, that every field
 * is SEEDED from the existing task (#1935). onSubmit fires with the collected input;
 * the caller turns it into an AddTask (buildTask) or a field-level UpdateTask patch.
 * A cron task requires a prompt (the daemon rejects an empty one — there is no event
 * line to fall back to); a watch task requires its command and may omit the prompt.
 */
function taskFormModal(opts: {
  title: string;
  confirmLabel: string;
  projects: string[];
  /** The project the picker defaults to (add) or is seeded to (edit). */
  defaultProject: string | null;
  /** Present only in edit mode: the task whose current values seed the form. */
  seed?: TaskData;
  /** Fetches the agent catalog for a project (#1970). Passed in rather than
   *  imported so this module stays free of the API/token layer, and re-run on every
   *  project change: the program a repo defaults to is a per-repo fact. */
  loadPrograms: (repoPath: string) => Promise<ProgramCatalog>;
  onSubmit: (input: AddTaskInput) => void;
  onCancel: () => void;
}): ModalHandle {
  const { handle, body, confirmBtn } = modalChrome({
    title: opts.title,
    confirmLabel: opts.confirmLabel,
    confirmClass: "af-primary",
    onCancel: opts.onCancel,
  });

  const nameInput = h("input", { type: "text", class: "af-input", placeholder: "Task name", autocomplete: "off" });
  nameInput.setAttribute("aria-label", "Task name");

  const projectSelect = h("select", { class: "af-input" });
  projectSelect.setAttribute("aria-label", "Project");
  if (opts.projects.length === 0) {
    const opt = h("option", { value: "" }, "No projects yet — create a session first");
    opt.disabled = true;
    opt.selected = true;
    projectSelect.append(opt);
    confirmBtn.disabled = true;
  } else {
    for (const p of opts.projects) {
      projectSelect.append(h("option", { value: p }, projectLabel(p)));
    }
    // Default the picker to the currently-scoped project (redesign PR2), so adding a
    // task from within a project lands it in that project without extra clicks. In
    // edit mode this is the task's own project (its seed), so it starts on it.
    if (opts.defaultProject && opts.projects.includes(opts.defaultProject)) {
      projectSelect.value = opts.defaultProject;
    }
  }

  const triggerSelect = h("select", { class: "af-input" });
  triggerSelect.setAttribute("aria-label", "Trigger type");
  triggerSelect.append(h("option", { value: "cron" }, "Cron schedule"));
  triggerSelect.append(h("option", { value: "watch" }, "Watch command"));

  // The friendly schedule picker replaces the raw-cron text field (#2057). It still
  // yields a cron expression — the stored/wire format is unchanged — and keeps the
  // raw field available under its Custom type.
  const picker = new SchedulePicker();

  const watchInput = h("input", { type: "text", class: "af-input", placeholder: "tail -F events.log", autocomplete: "off" });
  watchInput.setAttribute("aria-label", "Watch command");
  const watchField = field("Watch command", watchInput);
  watchField.hidden = true;

  // Show only the field(s) for the selected trigger; the other is hidden (its value
  // is ignored on submit, and the daemon rejects a task with both set).
  const syncTriggerFields = (): void => {
    const isWatch = triggerSelect.value === "watch";
    for (const row of picker.rows) {
      row.hidden = isWatch;
    }
    watchField.hidden = !isWatch;
    if (!isWatch) {
      // Re-assert the picker's own per-type row visibility, which the blanket
      // unhide above just cleared.
      picker.refresh();
    }
  };
  triggerSelect.addEventListener("change", syncTriggerFields);

  const promptArea = h("textarea", { class: "af-input af-textarea", placeholder: "Prompt to deliver ({{line}} for the watch line)", rows: 3 });
  promptArea.setAttribute("aria-label", "Prompt");

  const targetInput = h("input", { type: "text", class: "af-input", placeholder: "Target session (optional)", autocomplete: "off" });
  targetInput.setAttribute("aria-label", "Target session");

  // The program field (#1970). Its options come from the daemon, never from a list
  // here, so an agent added server-side shows up with no change to the web.
  const programSelect = h("select", { class: "af-input" });
  programSelect.setAttribute("aria-label", "Program");

  // A program the catalog doesn't list (e.g. one set via the CLI, or one retired
  // since the task was written) is carried through as a choice so an unrelated edit
  // never silently resets it to the repo default — programChoices appends it last.
  const keepProgram = opts.seed?.program ?? "";
  let programs: ProgramChoice[] = programChoices(null, keepProgram);

  const renderPrograms = (): void => {
    const previous = programSelect.value;
    programSelect.replaceChildren();
    for (const choice of programs) {
      programSelect.append(h("option", { value: choice.value }, choice.label));
    }
    programSelect.value = programs.some((c) => c.value === previous) ? previous : PROGRAM_REPO_DEFAULT;
  };
  renderPrograms();

  // A per-load token: a slow catalog for the project the user just left must not
  // overwrite the choices for the one they just picked.
  let programSeq = 0;
  const loadProgramsFor = (repoPath: string): void => {
    const seq = ++programSeq;
    void opts
      .loadPrograms(repoPath)
      .then((catalog) => {
        if (seq !== programSeq) {
          return;
        }
        programs = programChoices(catalog, keepProgram);
        renderPrograms();
      })
      .catch(() => {
        if (seq !== programSeq) {
          return;
        }
        // Degrade to "repo default" (plus any seeded program) — an unreachable
        // catalog costs the user the choice, never the task.
        programs = programChoices(null, keepProgram);
        renderPrograms();
      });
  };
  projectSelect.addEventListener("change", () => loadProgramsFor(projectSelect.value));

  // Seed every field from the existing task (edit mode). The trigger picker follows
  // whichever of watch_cmd / cron_expr the task carries, and its field visibility is
  // synced to match.
  if (opts.seed) {
    const s = opts.seed;
    nameInput.value = s.name ?? "";
    const isWatch = !!(s.watch_cmd && s.watch_cmd.trim() !== "");
    triggerSelect.value = isWatch ? "watch" : "cron";
    // The picker re-opens as the preset the stored cron maps to, or as Custom
    // holding the original expression verbatim (#2057).
    picker.seed(s.cron_expr ?? "");
    watchInput.value = s.watch_cmd ?? "";
    syncTriggerFields();
    promptArea.value = s.prompt ?? "";
    targetInput.value = s.target_session ?? "";
    programSelect.value = s.program ?? "";
  }

  // Kicked off after the seed so the catalog's re-render preserves the seeded
  // program as the current selection rather than clobbering it with the default.
  loadProgramsFor(projectSelect.value);

  body.append(
    field("Name", nameInput),
    field("Project", projectSelect),
    field("Trigger", triggerSelect),
    ...picker.rows,
    watchField,
    field("Prompt", promptArea),
    field("Target session", targetInput),
    field("Program", programSelect),
  );

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    const trigger = triggerSelect.value === "watch" ? "watch" : "cron";
    const name = nameInput.value.trim();
    const watchCmd = watchInput.value.trim();
    const prompt = promptArea.value.trim();
    if (name === "" || projectSelect.value === "") {
      handle.setError("A name and a project are required.");
      return;
    }
    // The picker always yields a well-formed expression for a preset; validate()
    // only catches the states that cannot generate one (an empty Custom cron, a
    // weekly with no days). The daemon re-validates whatever we send, as before.
    const scheduleErr = trigger === "cron" ? picker.validate() : null;
    if (scheduleErr !== null) {
      handle.setError(scheduleErr);
      return;
    }
    const cron = trigger === "cron" ? picker.cron() : "";
    if (trigger === "cron" && prompt === "") {
      handle.setError("A prompt is required for a cron task.");
      return;
    }
    if (trigger === "watch" && watchCmd === "") {
      handle.setError("A watch command is required for a watch task.");
      return;
    }
    handle.setError(null);
    opts.onSubmit({
      name,
      projectPath: projectSelect.value,
      trigger,
      cron,
      watchCmd,
      prompt: promptArea.value,
      targetSession: targetInput.value.trim(),
      program: programSelect.value,
    });
  });

  queueMicrotask(() => nameInput.focus());
  return handle;
}

/** The add-task modal: the shared task form with add chrome. The caller builds the
 *  task (buildTask) and POSTs AddTask. */
export function addTaskModal(
  projects: string[],
  defaultProject: string | null,
  callbacks: {
    onSubmit: (input: AddTaskInput) => void;
    onCancel: () => void;
    loadPrograms: (repoPath: string) => Promise<ProgramCatalog>;
  },
): ModalHandle {
  return taskFormModal({
    title: "Add task",
    confirmLabel: "Add",
    projects,
    defaultProject,
    loadPrograms: callbacks.loadPrograms,
    onSubmit: callbacks.onSubmit,
    onCancel: callbacks.onCancel,
  });
}

/** The edit-task modal (#1935): the same form seeded from `task`'s current values.
 *  The caller submits the collected fields as a field-level UpdateTask patch. The
 *  project picker starts on the task's own project and can move it to another. */
export function editTaskModal(
  projects: string[],
  task: TaskData,
  callbacks: {
    onSubmit: (input: AddTaskInput) => void;
    onCancel: () => void;
    loadPrograms: (repoPath: string) => Promise<ProgramCatalog>;
  },
): ModalHandle {
  return taskFormModal({
    title: "Edit task",
    confirmLabel: "Save",
    projects,
    defaultProject: task.project_path,
    seed: task,
    loadPrograms: callbacks.loadPrograms,
    onSubmit: callbacks.onSubmit,
    onCancel: callbacks.onCancel,
  });
}
