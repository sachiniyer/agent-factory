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

/** The new-session modal: title, project picker, program, initial prompt, auto-yes.
 *  Projects are the distinct repo roots derived from the current sessions (like the
 *  TUI's zero-config picker). onSubmit fires with the collected form values. */
export function newSessionModal(
  projects: string[],
  callbacks: { onSubmit: (values: CreateSessionInput) => void; onCancel: () => void },
): ModalHandle {
  const { handle, body, confirmBtn } = modalChrome({
    title: "New session",
    confirmLabel: "Create",
    confirmClass: "af-primary",
    onCancel: callbacks.onCancel,
  });

  const titleInput = h("input", { type: "text", class: "af-input", placeholder: "Session title", autocomplete: "off" });
  titleInput.setAttribute("aria-label", "Session title");

  const projectSelect = h("select", { class: "af-input" });
  projectSelect.setAttribute("aria-label", "Project");
  if (projects.length === 0) {
    const opt = h("option", { value: "" }, "No projects yet — create a session in the TUI first");
    opt.disabled = true;
    opt.selected = true;
    projectSelect.append(opt);
    confirmBtn.disabled = true;
  } else {
    for (const p of projects) {
      projectSelect.append(h("option", { value: p }, projectLabel(p)));
    }
  }

  const programSelect = h("select", { class: "af-input" });
  programSelect.setAttribute("aria-label", "Program");
  programSelect.append(h("option", { value: "" }, "Repo default"));
  for (const prog of ["claude", "codex", "aider", "gemini", "amp"]) {
    programSelect.append(h("option", { value: prog }, prog));
  }

  const promptArea = h("textarea", { class: "af-input af-textarea", placeholder: "Initial prompt (optional)", rows: 3 });
  promptArea.setAttribute("aria-label", "Initial prompt");

  const autoYes = h("input", { type: "checkbox", id: "af-autoyes" });
  const autoYesRow = h("label", { class: "af-check-row", htmlFor: "af-autoyes" }, autoYes, "Auto-yes (accept agent prompts automatically)");

  body.append(
    field("Title", titleInput),
    field("Project", projectSelect),
    field("Program", programSelect),
    field("Prompt", promptArea),
    autoYesRow,
  );

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    const title = titleInput.value.trim();
    if (title === "" || projectSelect.value === "") {
      handle.setError("A title and a project are required.");
      return;
    }
    handle.setError(null);
    callbacks.onSubmit({
      title,
      repoPath: projectSelect.value,
      program: programSelect.value,
      prompt: promptArea.value,
      autoYes: autoYes.checked,
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

/** A destructive-action confirm modal (kill or archive). */
export function confirmModal(
  opts: { action: "kill" | "archive"; sessionTitle: string; onConfirm: () => void; onCancel: () => void },
): ModalHandle {
  const isKill = opts.action === "kill";
  const { handle, body } = modalChrome({
    title: isKill ? `Kill ${opts.sessionTitle}?` : `Archive ${opts.sessionTitle}?`,
    confirmLabel: isKill ? "Kill" : "Archive",
    confirmClass: isKill ? "af-danger" : "af-primary",
    onCancel: opts.onCancel,
  });

  body.append(
    h(
      "p",
      { class: "af-modal-text" },
      isKill
        ? "This permanently destroys the session and prunes its branch. This can't be undone."
        : "This tears down the session's terminal and moves its worktree to the archive. You can restore it later.",
    ),
  );

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    handle.setError(null);
    opts.onConfirm();
  });

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
