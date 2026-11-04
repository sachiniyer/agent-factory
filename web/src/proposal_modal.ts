// The proposal modal (proposal.agent-workflow): the web's session-scoped entry
// to `af proposal start|attach|detach`. One dialog, three verbs — the footer's
// primary button starts a new proposal from the slug field, each existing
// proposal row attaches on click, and an attached session's line carries
// Detach. It rides the same daemon routes the CLI verbs call, so the surfaces
// cannot disagree about what starting or attaching does.
//
// Built once on open like every modal here: index.ts drives the RPC around the
// submit and patches busy/error in place through the ModalHandle, so typed input
// survives a refused call (a taken slug, an off-box refusal) for a retry.

import { errorText } from "./api.js";
import { h } from "./dom.js";
import { field, modalChrome, type ModalHandle } from "./components.js";
import { asForm } from "./modals.js";
import type { ProposalSummary } from "./proposal_api.js";

export function proposalModal(opts: {
  sessionTitle: string;
  sessionId: string;
  /** Optimistic attachment set from the session's Docs tabs. The catalog's
   *  attached_sessions replaces it once loaded — the store knows attachments
   *  whose tab closed or never opened, and rejects tabs opened without one. */
  attached: string[];
  loadProposals: () => Promise<ProposalSummary[]>;
  onStart: (slug: string) => void;
  onAttach: (slug: string) => void;
  onDetach: () => void;
  onCancel: () => void;
}): ModalHandle {
  const { handle, body, confirmBtn } = modalChrome({
    title: `Proposal · ${opts.sessionTitle}`,
    confirmLabel: "Start",
    confirmClass: "af-primary",
    onCancel: opts.onCancel,
  });

  let busy = false;
  // Buttons the modal carries besides the footer's: the attach rows and Detach.
  // modalChrome's setBusy only knows the footer pair, so they share this list —
  // a busy submit must freeze every action, not just the one that fired.
  const rowButtons: HTMLButtonElement[] = [];
  let detachBtn: HTMLButtonElement | null = null;

  const slugInput = h("input", {
    type: "text",
    class: "af-input",
    placeholder: "auth-rework",
    autocomplete: "off",
  });
  slugInput.setAttribute("aria-label", "New proposal name");

  const syncSubmit = (): void => {
    confirmBtn.disabled = busy || slugInput.value.trim() === "";
    if (detachBtn) {
      detachBtn.disabled = busy;
    }
    for (const btn of rowButtons) {
      btn.disabled = busy || btn.dataset.current === "true";
    }
  };
  const chromeSetBusy = handle.setBusy.bind(handle);
  handle.setBusy = (b: boolean): void => {
    busy = b;
    chromeSetBusy(b);
    syncSubmit();
  };
  slugInput.addEventListener("input", syncSubmit);

  // The session's attachments, seeded from its Docs tabs and reconciled
  // against attached_sessions once the catalog loads — the store projection is
  // authoritative where the roster is not: a tab the user closed, or a
  // tab_error that left the attach committed without one, still counts; a Docs
  // tab opened on a proposal the session never attached to does not.
  // Starting or attaching elsewhere moves the attachment (the daemon closes
  // the old Docs tabs first), which the hint states rather than re-asking.
  let attached = [...opts.attached];
  const attachedSlot = h("div");
  const renderAttached = (): void => {
    detachBtn = null;
    attachedSlot.replaceChildren();
    if (attached.length === 0) {
      return;
    }
    detachBtn = h("button", { type: "button", class: "af-ghost af-prop-detach" }, "Detach");
    detachBtn.title = "Remove this session's proposal attachment and close its Proposal tab";
    detachBtn.addEventListener("click", () => opts.onDetach());
    attachedSlot.append(
      h("p", { class: "af-modal-text af-prop-attached" },
        `Attached to ${attached.map((s) => `“${s}”`).join(", ")} · starting or attaching another moves the session `,
        detachBtn),
    );
    syncSubmit();
  };

  const list = h("div", { class: "af-prop-list", role: "listbox" });
  list.setAttribute("aria-label", "Existing proposals");
  const listNote = h("p", { class: "af-prop-note", role: "status" }, "Loading…");

  const renderRows = (rows: ProposalSummary[]): void => {
    // Reconcile against the store: the daemon names attached sessions by
    // title, falling back to the id itself for one it no longer knows.
    attached = rows
      .filter((r) => r.attachedSessions.includes(opts.sessionTitle) || r.attachedSessions.includes(opts.sessionId))
      .map((r) => r.slug);
    renderAttached();
    rowButtons.length = 0;
    list.replaceChildren();
    for (const row of rows) {
      const current = attached.includes(row.slug);
      const item = h("button", { type: "button", class: "af-prop-item", role: "option" },
        h("span", { class: "af-prop-name" },
          h("span", { class: "af-prop-text" }, row.slug)),
        h("span", { class: "af-prop-meta" },
          `${row.docs.length} doc${row.docs.length === 1 ? "" : "s"}` +
          (row.attachedSessions.length > 0 ? ` · ${row.attachedSessions.join(", ")}` : "") +
          (current ? " · attached" : "")));
      item.dataset.current = String(current);
      item.setAttribute("aria-selected", String(current));
      item.addEventListener("click", () => opts.onAttach(row.slug));
      rowButtons.push(item);
      list.append(h("div", { class: "af-prop-row" }, item));
    }
    listNote.textContent = rows.length === 0 ? "No proposals yet — start one above." : "";
    syncSubmit();
  };

  void opts.loadProposals().then(renderRows).catch((e: unknown) => {
    // The attach picker degrades, the start form does not: a failed catalog
    // leaves "Start" working, which is the only action that needs no list.
    listNote.textContent = `Could not load proposals: ${errorText(e)}`;
  });

  body.append(
    attachedSlot,
    field("New proposal", slugInput),
    h("p", { class: "af-modal-hint" }, "Creates the proposal from the doc templates and attaches the session — its docs open beside the agent."),
    h("span", { class: "af-modal-label" }, "Attach an existing proposal"),
    list,
    listNote,
  );
  renderAttached();

  asForm(handle.el.firstElementChild as HTMLElement, () => {
    const slug = slugInput.value.trim();
    if (slug === "") {
      handle.setError("Name the proposal.");
      return;
    }
    handle.setError(null);
    opts.onStart(slug);
  });

  syncSubmit();
  queueMicrotask(() => slugInput.focus());
  return handle;
}
