import { h } from "./dom.js";
import { asForm, modalChrome, type ModalHandle } from "./modals.js";
import { TabKind } from "./types.js";

/** Confirmation for persistent tab removal, shared by the tab × and rail-mode w. */
export function confirmDeleteTabModal(opts: {
  sessionTitle: string;
  tabName: string;
  kind: number;
  onConfirm: () => void;
  onCancel: () => void;
}): ModalHandle {
  const { handle, body, cancelBtn } = modalChrome({
    title: `Delete tab “${opts.tabName}” from session “${opts.sessionTitle}”?`,
    confirmLabel: "Delete tab",
    confirmClass: "af-danger",
    onCancel: opts.onCancel,
  });
  body.append(h("p", { class: "af-modal-text" }, opts.kind === TabKind.Web
    ? "This removes the web tab, not the service it displays. Hiding a pane leaves the tab available."
    : "This removes the tab and requests cleanup of its runtime. Hiding a pane leaves the tab available."));
  asForm(handle.el.firstElementChild as HTMLElement, opts.onConfirm);
  queueMicrotask(() => cancelBtn.focus());
  return handle;
}
