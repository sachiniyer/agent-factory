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

import { asForm, field, h, modalChrome, type ModalHandle, projectLabel } from "./modals.js";
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
 *  handler has its stable id AND its current fields (UpdateTask needs the full task). */
export interface TaskActions {
  /** Opens the add-task modal. */
  add(): void;
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

  constructor(private readonly actions: TaskActions) {
    this.el = h("section", { class: "af-tasks" });
    this.el.setAttribute("aria-label", "Tasks");
  }

  update(tasks: TaskData[]): void {
    if (this.lastTasks === tasks) {
      return;
    }
    this.lastTasks = tasks;
    this.render(tasks);
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

    const actionEls: HTMLElement[] = [toggleBtn];
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

/**
 * The add-task modal (mirrors the TUI's task editor + `af tasks add`): name,
 * project, a cron/watch trigger toggle with its value, a prompt, an optional target
 * session, and the agent program. onSubmit fires with the collected input; the caller
 * builds the task (buildTask) and POSTs AddTask. A cron task requires a prompt (the
 * daemon rejects an empty one — there is no event line to fall back to); a watch task
 * requires its command and may omit the prompt.
 */
export function addTaskModal(
  projects: string[],
  callbacks: { onSubmit: (input: AddTaskInput) => void; onCancel: () => void },
): ModalHandle {
  const { handle, body, confirmBtn } = modalChrome({
    title: "Add task",
    confirmLabel: "Add",
    confirmClass: "af-primary",
    onCancel: callbacks.onCancel,
  });

  const nameInput = h("input", { type: "text", class: "af-input", placeholder: "Task name", autocomplete: "off" });
  nameInput.setAttribute("aria-label", "Task name");

  const projectSelect = h("select", { class: "af-input" });
  projectSelect.setAttribute("aria-label", "Project");
  if (projects.length === 0) {
    const opt = h("option", { value: "" }, "No projects yet — create a session first");
    opt.disabled = true;
    opt.selected = true;
    projectSelect.append(opt);
    confirmBtn.disabled = true;
  } else {
    for (const p of projects) {
      projectSelect.append(h("option", { value: p }, projectLabel(p)));
    }
  }

  const triggerSelect = h("select", { class: "af-input" });
  triggerSelect.setAttribute("aria-label", "Trigger type");
  triggerSelect.append(h("option", { value: "cron" }, "Cron schedule"));
  triggerSelect.append(h("option", { value: "watch" }, "Watch command"));

  const cronInput = h("input", { type: "text", class: "af-input", placeholder: "0 9 * * *", autocomplete: "off" });
  cronInput.setAttribute("aria-label", "Cron expression");
  const cronField = field("Cron expression", cronInput);

  const watchInput = h("input", { type: "text", class: "af-input", placeholder: "tail -F events.log", autocomplete: "off" });
  watchInput.setAttribute("aria-label", "Watch command");
  const watchField = field("Watch command", watchInput);
  watchField.hidden = true;

  // Show only the field for the selected trigger; the other is hidden (its value is
  // ignored by buildTask, and the daemon rejects a task with both set).
  triggerSelect.addEventListener("change", () => {
    const isWatch = triggerSelect.value === "watch";
    cronField.hidden = isWatch;
    watchField.hidden = !isWatch;
  });

  const promptArea = h("textarea", { class: "af-input af-textarea", placeholder: "Prompt to deliver ({{line}} for the watch line)", rows: 3 });
  promptArea.setAttribute("aria-label", "Prompt");

  const targetInput = h("input", { type: "text", class: "af-input", placeholder: "Target session (optional)", autocomplete: "off" });
  targetInput.setAttribute("aria-label", "Target session");

  const programSelect = h("select", { class: "af-input" });
  programSelect.setAttribute("aria-label", "Program");
  programSelect.append(h("option", { value: "" }, "Repo default"));
  for (const prog of ["claude", "codex", "aider", "gemini", "amp"]) {
    programSelect.append(h("option", { value: prog }, prog));
  }

  body.append(
    field("Name", nameInput),
    field("Project", projectSelect),
    field("Trigger", triggerSelect),
    cronField,
    watchField,
    field("Prompt", promptArea),
    field("Target session", targetInput),
    field("Program", programSelect),
  );

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    const trigger = triggerSelect.value === "watch" ? "watch" : "cron";
    const name = nameInput.value.trim();
    const cron = cronInput.value.trim();
    const watchCmd = watchInput.value.trim();
    const prompt = promptArea.value.trim();
    if (name === "" || projectSelect.value === "") {
      handle.setError("A name and a project are required.");
      return;
    }
    if (trigger === "cron" && cron === "") {
      handle.setError("A cron expression is required for a cron task.");
      return;
    }
    if (trigger === "cron" && prompt === "") {
      handle.setError("A prompt is required for a cron task.");
      return;
    }
    if (trigger === "watch" && watchCmd === "") {
      handle.setError("A watch command is required for a watch task.");
      return;
    }
    handle.setError(null);
    callbacks.onSubmit({
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
