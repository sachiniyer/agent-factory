// The web client's modal overlays (#1592 Phase 5 PR5): the new-session form, the
// send-prompt box, and the kill/archive confirms — the write surface that
// completes the v1 loop (list → attach → type → create/kill). They mirror the
// TUI's overlays (ui/overlay/textOverlay.go, projectPickerOverlay.go) as small
// additive views.
//
// Each modal is built ONCE when opened and returns a ModalHandle: index.ts mounts
// it into the shell's persistent modal host, drives the async API call the
// submit fires, and patches busy/error IN PLACE via the handle — never rebuilding
// the DOM, so typed input survives a failed submit (e.g. "title taken") for a
// retry. This is the same build-once/patch-in-place model the terminal and rail
// header use, and it keeps the store a pure read-model: modals are ephemeral UI
// managed imperatively, not store state.
//
// CSP-safe like the rest of the client: createElement + addEventListener only, no
// innerHTML with markup and no inline handlers, so the daemon's default-src 'self'
// policy holds.

import type { CreateSessionInput } from "./api.js";
import { type BackendCatalog, type BackendChoice, REPO_DEFAULT, backendChoices, backendNotice, backendSelectable } from "./backends.js";
import { PROGRAM_REPO_DEFAULT, type ProgramCatalog, type ProgramChoice, handoffAgentChoices, programChoices } from "./programs.js";

/** A live modal: its root element plus in-place patch controls index.ts drives
 *  around the async submit. close() removes it from the DOM. */
export interface ModalHandle {
  el: HTMLElement;
  setBusy(busy: boolean): void;
  setError(msg: string | null): void;
  close(): void;
}

/** Minimal hyperscript (shared by the modal builders and the projects/tasks panes,
 *  #1592 Phase 5 PR8): create an element, apply props, append children — CSP-safe
 *  createElement, no innerHTML. */
export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: Partial<HTMLElementTagNameMap[K]> & { class?: string } = {},
  ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === "class") {
      el.className = value as string;
    } else {
      (el as unknown as Record<string, unknown>)[key] = value;
    }
  }
  for (const child of children) {
    el.append(child);
  }
  return el;
}

/** Builds the shared modal chrome: a backdrop, a titled card, a body slot, an
 *  error line, and a footer with a cancel + a primary action button. Returns the
 *  pieces the specific modals wire their behavior onto. Clicking the backdrop or
 *  pressing Escape cancels; Enter is left to the form's own submit. */
export function modalChrome(opts: {
  title: string;
  confirmLabel: string;
  confirmClass: string;
  onCancel: () => void;
}): {
  handle: ModalHandle;
  body: HTMLElement;
  confirmBtn: HTMLButtonElement;
  cancelBtn: HTMLButtonElement;
  errorLine: HTMLElement;
} {
  const body = h("div", { class: "af-modal-body" });
  const errorLine = h("p", { class: "af-modal-error", role: "alert" });
  errorLine.hidden = true;

  const cancelBtn = h("button", { type: "button", class: "af-ghost" }, "Cancel");
  const confirmBtn = h("button", { type: "submit", class: opts.confirmClass }, opts.confirmLabel);
  const footer = h("div", { class: "af-modal-foot" }, cancelBtn, confirmBtn);

  const card = h(
    "div",
    { class: "af-modal-card", role: "dialog" },
    h("h2", { class: "af-modal-title" }, opts.title),
    body,
    errorLine,
    footer,
  );
  card.setAttribute("aria-modal", "true");
  card.setAttribute("aria-label", opts.title);
  // Stop a click inside the card from bubbling to the backdrop's cancel handler.
  card.addEventListener("click", (e) => e.stopPropagation());

  const backdrop = h("div", { class: "af-modal-backdrop" }, card);
  backdrop.addEventListener("click", () => opts.onCancel());

  cancelBtn.addEventListener("click", () => opts.onCancel());

  const handle: ModalHandle = {
    el: backdrop,
    setBusy(busy: boolean) {
      confirmBtn.disabled = busy;
      cancelBtn.disabled = busy;
      card.classList.toggle("af-modal-busy", busy);
    },
    setError(msg: string | null) {
      if (msg) {
        errorLine.textContent = msg;
        errorLine.hidden = false;
      } else {
        errorLine.textContent = "";
        errorLine.hidden = true;
      }
    },
    close() {
      backdrop.remove();
    },
  };
  return { handle, body, confirmBtn, cancelBtn, errorLine };
}

/** Wraps the card's content in a <form> so Enter submits and the browser handles
 *  focus, calling onSubmit with preventDefault already applied. */
export function asForm(card: HTMLElement, onSubmit: () => void): void {
  // The card children were appended directly; re-parent them under a form so a
  // native submit (Enter / the primary button) is captured once.
  const form = h("form", { class: "af-modal-form" });
  while (card.firstChild) {
    form.append(card.firstChild);
  }
  card.append(form);
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    onSubmit();
  });
}

/** The new-session modal: title, project picker, program, backend, and initial
 *  prompt. Projects are the distinct repo roots derived from the current sessions
 *  (like the TUI's zero-config picker). onSubmit fires with the collected form
 *  values.
 *
 *  loadBackends fetches the picked project's backend catalog (#1933), and
 *  loadPrograms the agent catalog (#1970). Both are passed in rather than imported
 *  so this module stays free of the API/token layer, and both are re-run whenever
 *  the project changes: each catalog's default is a per-repo fact, so the choices
 *  must follow the project picker. If either rejects, its field degrades to "repo
 *  default" alone — the exact behavior before these fields existed, so a catalog
 *  failure can never block a create. */
export function newSessionModal(
  projects: string[],
  defaultProject: string | null,
  callbacks: {
    onSubmit: (values: CreateSessionInput) => void;
    onCancel: () => void;
    loadBackends: (repoPath: string) => Promise<BackendCatalog>;
    loadPrograms: (repoPath: string) => Promise<ProgramCatalog>;
    // #2470: fetch a random, readable session name from the daemon (the wordlist
    // is Go-only) to show as shadow text; an empty submit adopts it.
    suggestName: () => Promise<string>;
  },
): ModalHandle {
  const { handle, body, confirmBtn } = modalChrome({
    title: "New session",
    confirmLabel: "Create",
    confirmClass: "af-primary",
    onCancel: callbacks.onCancel,
  });

  const titleInput = h("input", { type: "text", class: "af-input", placeholder: "Session title", autocomplete: "off" });
  titleInput.setAttribute("aria-label", "Session title");
  // #2470: the daemon-suggested autocreate name. Shown as the title placeholder
  // (shadow text that clears the instant the user types) and, when the field is
  // submitted empty, used as the session name. "" until the fetch lands or if it
  // fails — in which case the field falls back to requiring a typed title.
  let suggestedName = "";

  const projectSelect = h("select", { class: "af-input" });
  projectSelect.setAttribute("aria-label", "Project");
  if (projects.length === 0) {
    // Post-#2456 the coherent zero-projects action is the switcher's "+ Add project",
    // not the TUI: once a repo is registered it appears here (pickerProjects ∪ the
    // registry), so point the user at it rather than at a different surface.
    const opt = h("option", { value: "" }, "No projects yet — add one from the project switcher first");
    opt.disabled = true;
    opt.selected = true;
    projectSelect.append(opt);
    confirmBtn.disabled = true;
  } else {
    for (const p of projects) {
      projectSelect.append(h("option", { value: p }, projectLabel(p)));
    }
    // Default to the currently-scoped project (redesign PR2): a new session created
    // from within a project lands in that project by default.
    if (defaultProject && projects.includes(defaultProject)) {
      projectSelect.value = defaultProject;
    }
  }

  // The program field (#1970). Like the backend field below, its options come from
  // the daemon, never from a list here, so an agent added server-side shows up with
  // no change to the web.
  const programSelect = h("select", { class: "af-input" });
  programSelect.setAttribute("aria-label", "Program");
  let programs: ProgramChoice[] = programChoices(null);

  // The backend field (#1933). Its options come from the daemon, never from a list
  // here, so a backend added server-side shows up with no change to the web.
  const backendSelect = h("select", { class: "af-input" });
  backendSelect.setAttribute("aria-label", "Backend");
  const backendHint = h("p", { class: "af-modal-hint" });
  // Announce the notice when it changes: the reason a choice is unusable must
  // reach a screen reader, not only sighted users scanning under the select.
  backendHint.setAttribute("role", "status");

  let choices: BackendChoice[] = backendChoices(null);
  // Mirrors the chrome's busy flag. An async availability refresh can land at ANY
  // time — including mid-submit — and it must never be the thing that decides
  // whether Create is clickable.
  let busy = false;

  // The SINGLE writer of confirmBtn.disabled, so no update can clobber another's
  // reason for disabling it. Every input that gates Create is OR-ed here rather
  // than assigned from its own call site:
  //   busy      — a submit is in flight; re-enabling would allow a double-create.
  //   projects  — nothing to create in (set once, before any catalog lands).
  //   backend   — the selection is unusable or unverified.
  // A bare `confirmBtn.disabled = …` anywhere else silently drops the other two.
  const syncSubmitState = (): void => {
    backendHint.textContent = backendNotice(choices, backendSelect.value);
    confirmBtn.disabled = busy || projects.length === 0 || !backendSelectable(choices, backendSelect.value);
  };

  // Route the chrome's setBusy through the same writer. index.ts drives busy around
  // the create call; without this, a catalog response arriving mid-create would
  // re-enable the button underneath it.
  const chromeSetBusy = handle.setBusy.bind(handle);
  handle.setBusy = (b: boolean): void => {
    busy = b;
    chromeSetBusy(b);
    syncSubmitState();
  };

  const renderChoices = (): void => {
    const previous = backendSelect.value;
    backendSelect.replaceChildren();
    for (const choice of choices) {
      backendSelect.append(h("option", { value: choice.value }, choice.label));
    }
    // Re-selecting the prior value keeps a user's pick across a project switch when
    // it is still offered; otherwise fall back to the repo default, which always is.
    backendSelect.value = choices.some((c) => c.value === previous) ? previous : REPO_DEFAULT;
    syncSubmitState();
  };

  backendSelect.addEventListener("change", syncSubmitState);

  const renderPrograms = (): void => {
    const previous = programSelect.value;
    programSelect.replaceChildren();
    for (const choice of programs) {
      programSelect.append(h("option", { value: choice.value }, choice.label));
    }
    // Same rule as the backend picker: keep the user's pick across a project switch
    // when it is still offered, else fall back to the repo default, which always is.
    programSelect.value = programs.some((c) => c.value === previous) ? previous : PROGRAM_REPO_DEFAULT;
  };

  // A per-load token: a slow catalog for the project the user just left must not
  // overwrite the choices for the one they just picked. Shared by both catalogs —
  // they are fetched together and are both per-project facts, so one token orders
  // them consistently.
  let loadSeq = 0;
  const loadCatalogsFor = (repoPath: string): void => {
    const seq = ++loadSeq;

    // The program enum is global, so this is asked even with no project picked —
    // repoPath only sharpens which agent "repo default" resolves to.
    void callbacks
      .loadPrograms(repoPath)
      .then((catalog) => {
        if (seq !== loadSeq) {
          return;
        }
        programs = programChoices(catalog);
        renderPrograms();
      })
      .catch(() => {
        if (seq !== loadSeq) {
          return;
        }
        // Degrade to "repo default" only — the same contract as the backend field:
        // an unreachable catalog costs the user the choice, never the session, since
        // sending no program is exactly what "repo default" means on the wire.
        programs = programChoices(null);
        renderPrograms();
      });

    if (repoPath === "") {
      choices = backendChoices(null);
      renderChoices();
      return;
    }
    void callbacks
      .loadBackends(repoPath)
      .then((catalog) => {
        if (seq !== loadSeq) {
          return;
        }
        choices = backendChoices(catalog);
        renderChoices();
      })
      .catch(() => {
        if (seq !== loadSeq) {
          return;
        }
        // Degrade to "repo default" only. The create path is unchanged by an
        // unknown catalog, so this costs the user the choice, not the session.
        choices = backendChoices(null);
        renderChoices();
      });
  };

  projectSelect.addEventListener("change", () => loadCatalogsFor(projectSelect.value));

  const promptArea = h("textarea", { class: "af-input af-textarea", placeholder: "Initial prompt (optional)", rows: 3 });
  promptArea.setAttribute("aria-label", "Initial prompt");

  body.append(
    field("Title", titleInput),
    field("Project", projectSelect),
    field("Program", programSelect),
    field("Backend", backendSelect),
    backendHint,
    field("Prompt", promptArea),
  );

  renderPrograms();
  renderChoices();
  loadCatalogsFor(projectSelect.value);

  // Ask for the autocreate name once, on open. Repo-agnostic (the daemon avoids
  // every live title), so it needs no re-fetch on a project change. A failure is
  // silent: the field keeps its static placeholder and simply requires a typed
  // title, exactly as before this feature.
  void callbacks
    .suggestName()
    .then((name) => {
      if (name !== "") {
        suggestedName = name;
        titleInput.placeholder = name;
      }
    })
    .catch(() => {
      /* no suggestion — leave the static placeholder and the typed-title requirement */
    });

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    // #2470: an empty field adopts the suggested name (the shadow text). The
    // placeholder already equals suggestedName, so submitting untouched creates
    // exactly the name the user saw.
    const typed = titleInput.value.trim();
    const title = typed !== "" ? typed : suggestedName;
    if (projectSelect.value === "") {
      handle.setError("A project is required.");
      return;
    }
    if (title === "") {
      handle.setError("A title is required.");
      return;
    }
    handle.setError(null);
    callbacks.onSubmit({
      title,
      repoPath: projectSelect.value,
      program: programSelect.value,
      prompt: promptArea.value,
      // REPO_DEFAULT ("") when the user did not choose — createSession then omits
      // `backend` entirely and the repo's config decides (#1933).
      backend: backendSelect.value,
    });
  });

  queueMicrotask(() => titleInput.focus());
  return handle;
}

/** The send-prompt modal: a textarea whose text is sent to the named session. */
export function promptModal(
  sessionTitle: string,
  callbacks: { onSubmit: (text: string) => void; onCancel: () => void },
): ModalHandle {
  const { handle, body } = modalChrome({
    title: `Send prompt to ${sessionTitle}`,
    confirmLabel: "Send",
    confirmClass: "af-primary",
    onCancel: callbacks.onCancel,
  });

  const area = h("textarea", { class: "af-input af-textarea", placeholder: "Prompt", rows: 4 });
  area.setAttribute("aria-label", "Prompt");
  body.append(area);

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    const text = area.value.trim();
    if (text === "") {
      handle.setError("Enter a prompt to send.");
      return;
    }
    handle.setError(null);
    callbacks.onSubmit(text);
  });

  queueMicrotask(() => area.focus());
  return handle;
}

/** The handoff modal (#2013): pick the agent to continue the session under — the
 *  web half of the TUI's `F`. It collapses the TUI's pick-then-confirm into one
 *  dialog: a web modal already gates the swap behind an explicit submit, and the
 *  explanatory line IS the confirmation the TUI shows after the picker.
 *
 *  The agent list comes from the daemon (loadPrograms), never a list here, and
 *  excludes the running agent — the daemon's same-agent guard would reject it. If
 *  the catalog can't be reached or offers no other agent, Hand off stays disabled
 *  with an explanation, so the modal degrades to "you can't from here" rather than
 *  to a submit that always errors. */
export function handoffModal(
  sessionTitle: string,
  currentAgent: string,
  callbacks: {
    onSubmit: (target: string) => void;
    onCancel: () => void;
    loadPrograms: () => Promise<ProgramCatalog>;
  },
): ModalHandle {
  const { handle, body, confirmBtn } = modalChrome({
    title: `Hand off ${sessionTitle}`,
    confirmLabel: "Hand off",
    confirmClass: "af-primary",
    onCancel: callbacks.onCancel,
  });

  const agentSelect = h("select", { class: "af-input" });
  agentSelect.setAttribute("aria-label", "New agent");
  // Nothing to pick until the catalog lands; Hand off is disabled until it does.
  confirmBtn.disabled = true;

  const renderChoices = (choices: ProgramChoice[]): void => {
    agentSelect.replaceChildren();
    for (const choice of choices) {
      agentSelect.append(h("option", { value: choice.value }, choice.label));
    }
    confirmBtn.disabled = choices.length === 0;
  };

  body.append(
    field("New agent", agentSelect),
    h(
      "p",
      { class: "af-modal-text" },
      "The new agent starts fresh with a summary of the work so far. Same worktree and branch — nothing is discarded.",
    ),
  );

  void callbacks
    .loadPrograms()
    .then((catalog) => {
      const choices = handoffAgentChoices(catalog, currentAgent);
      renderChoices(choices);
      if (choices.length === 0) {
        handle.setError("No other agent is available to hand off to.");
      }
    })
    .catch(() => {
      // A handoff needs a concrete target and the web cannot read the enum itself —
      // that is the whole reason ListPrograms is served — so an unreachable catalog
      // means the picker can't be offered. Say so rather than submit an empty `to`.
      renderChoices([]);
      handle.setError("Could not load the agent list. Try again.");
    });

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    const target = agentSelect.value;
    if (target === "") {
      handle.setError("Pick an agent to hand off to.");
      return;
    }
    handle.setError(null);
    callbacks.onSubmit(target);
  });

  queueMicrotask(() => agentSelect.focus());
  return handle;
}

/** A session-lifecycle confirm modal (kill, archive, or restore). Kill is
 *  destructive; archive/restore are the reversible pair (#1932). */
export function confirmModal(
  opts: { action: "kill" | "archive" | "restore"; sessionTitle: string; onConfirm: () => void; onCancel: () => void },
): ModalHandle {
  // Restore is the reverse of archive (#1932): non-destructive, so it reads as a
  // primary (not danger) confirm, mirroring archive's own class. The web routes it
  // through this same confirm chrome as kill/archive so it inherits their busy +
  // error surface; the copy makes true the "you can restore it later" promise the
  // archive confirm already prints.
  const copy = {
    kill: {
      title: `Kill ${opts.sessionTitle}?`,
      confirmLabel: "Kill",
      confirmClass: "af-danger",
      body: "This permanently destroys the session and prunes its branch. This can't be undone.",
    },
    archive: {
      title: `Archive ${opts.sessionTitle}?`,
      confirmLabel: "Archive",
      confirmClass: "af-primary",
      body: "This tears down the session's terminal and moves its worktree to the archive. You can restore it later.",
    },
    restore: {
      title: `Restore ${opts.sessionTitle}?`,
      confirmLabel: "Restore",
      confirmClass: "af-primary",
      body: "This moves the session's worktree back next to its repo and re-spawns the agent, returning it to the live rail.",
    },
  }[opts.action];

  const { handle, body } = modalChrome({
    title: copy.title,
    confirmLabel: copy.confirmLabel,
    confirmClass: copy.confirmClass,
    onCancel: opts.onCancel,
  });

  body.append(h("p", { class: "af-modal-text" }, copy.body));

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    handle.setError(null);
    opts.onConfirm();
  });

  return handle;
}

/** Delete-project confirmation (#1735): archived sessions remain restorable and
 *  the real git repo is untouched, while the durable project registration is
 *  removed. */
export function confirmDeleteProjectModal(
  opts: { projectLabel: string; sessionCount: number; onConfirm: () => void; onCancel: () => void },
): ModalHandle {
  const word = opts.sessionCount === 1 ? "session" : "sessions";
  const { handle, body } = modalChrome({
    title: `Delete project ${opts.projectLabel}?`,
    confirmLabel: "Delete project",
    confirmClass: "af-danger",
    onCancel: opts.onCancel,
  });

  // A registered project with no live sessions (#2456) has nothing to archive — the
  // delete just drops its registry record. Say so, rather than "Archive 0 sessions".
  const message =
    opts.sessionCount === 0
      ? "Remove this project from the list. It has no sessions to archive, and your real git repo is untouched — you can add it again anytime."
      : `Archive ${opts.sessionCount} ${word} and remove this project. Archived sessions stay restorable and your real git repo is untouched — restore any of them to bring the project back.`;
  body.append(h("p", { class: "af-modal-text" }, message));

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    handle.setError(null);
    opts.onConfirm();
  });

  return handle;
}

/** The add-project modal (#2456): a single path input that registers a git
 *  checkout as a durable, sessionless project through the RegisterProject RPC.
 *  The path names a directory ON THE DAEMON HOST (absolute, or ~-prefixed which
 *  the daemon expands) — a browser has no shared cwd to resolve a relative path
 *  against, so the daemon owns resolution and validation. A non-git or unreadable
 *  path comes back as the daemon's actionable error, shown inline; on success the
 *  modal closes and the repo appears in the switcher immediately (the #2456 union:
 *  the derived project list ∪ the daemon's registry), no session required.
 *
 *  onSubmit is async in index.ts, which drives setBusy/setError around it. */
export function addProjectModal(callbacks: {
  onSubmit: (path: string) => void;
  onCancel: () => void;
}): ModalHandle {
  const { handle, body } = modalChrome({
    title: "Add project",
    confirmLabel: "Add project",
    confirmClass: "af-primary",
    onCancel: callbacks.onCancel,
  });

  const pathInput = h("input", {
    type: "text",
    class: "af-input",
    placeholder: "/path/to/repo  or  ~/repo",
    autocomplete: "off",
  });
  pathInput.setAttribute("aria-label", "Repository path");

  body.append(
    field("Repository path", pathInput),
    h(
      "p",
      { class: "af-modal-hint" },
      "An absolute path to a git checkout on the daemon host (~ is expanded there). It becomes an empty project you can create sessions into.",
    ),
  );

  // Clear a stale validation/daemon error as the user edits, so the inline
  // message always reflects the CURRENT input rather than the last rejected path.
  pathInput.addEventListener("input", () => handle.setError(null));

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    const path = pathInput.value.trim();
    if (path === "") {
      handle.setError("Enter a repository path.");
      return;
    }
    handle.setError(null);
    callbacks.onSubmit(path);
  });

  // Focus the sole input so the user can type immediately, matching every other
  // input modal (add-task, create-session, …) — no click required.
  queueMicrotask(() => pathInput.focus());
  return handle;
}

/** A labeled field row: a caption above its control. */
export function field(label: string, control: HTMLElement): HTMLElement {
  return h("label", { class: "af-modal-field" }, h("span", { class: "af-modal-label" }, label), control);
}

/** A friendly project label: the repo's basename with its parent for context. */
export function projectLabel(root: string): string {
  const parts = root.replace(/\/+$/, "").split("/");
  const base = parts[parts.length - 1] || root;
  const parent = parts.length >= 2 ? parts[parts.length - 2] : "";
  return parent ? `${base}  (${parent}/${base})` : base;
}
