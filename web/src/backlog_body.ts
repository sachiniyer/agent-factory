// An item body editor with debounced autosave. The editor comes through a loader
// seam: today a plain textarea, and the CodeMirror live preview from the lazy Docs
// bundle can replace it by swapping bodyEditorLoader once that bundle is on master.

import { SerialQueue, getDraft, queueFor, setDraft } from "./backlog_serial.js";

export const SAVE_DELAY_MS = 800;

/** The slice of an editor the body needs; the Docs editor satisfies it too. */
export interface BodyEditorHandle {
  getText(): string;
  setText(text: string): void;
  destroy(): void;
}

export interface BodyEditorOptions {
  text: string;
  label: string;
  onChange(text: string): void;
}

export interface BodyEditorModule {
  createDocsEditor(parent: HTMLElement, opts: BodyEditorOptions): BodyEditorHandle;
}

/** A textarea editor: the editor the body uses until the Docs editor is on master. */
function createTextareaEditor(parent: HTMLElement, opts: BodyEditorOptions): BodyEditorHandle {
  const area = document.createElement("textarea");
  area.className = "af-backlog-body-input";
  area.value = opts.text;
  area.rows = 6;
  area.setAttribute("aria-label", opts.label);
  area.addEventListener("input", () => opts.onChange(area.value));
  parent.append(area);
  return {
    getText: () => area.value,
    setText: (t) => { area.value = t; },
    destroy: () => area.remove(),
  };
}

/** Where the editor comes from; tests swap in a fake. */
export const bodyEditorLoader: { load(): Promise<BodyEditorModule> } = {
  load: async () => ({ createDocsEditor: createTextareaEditor }),
};

export class BodyEditor {
  readonly el: HTMLElement;
  private editor: BodyEditorHandle | null = null;
  private text = "";
  private timer: ReturnType<typeof setTimeout> | null = null;
  // The edit the user made that no confirming response has covered yet: typed (the
  // debounce is running) or saved and still in flight. Stored text arriving while it
  // is set is older than the edit and must not replace it. It survives a failed save,
  // so the edit is kept (and the next change saves it again).
  private localDraft: string | null = null;
  private readonly draftKey: string | undefined;
  private get draft(): string | null { return this.draftKey === undefined ? this.localDraft : getDraft<string>(this.draftKey); }
  private set draft(v: string | null) { if (this.draftKey === undefined) this.localDraft = v; else setDraft(this.draftKey, v); }
  private disposed = false;
  // Counts the item's edits, shared with every editor of it like the draft: a save's
  // acknowledgement covers the edit it carried only if no edit has been made since,
  // whatever text that edit left (typing A, B, A again is three edits, not none).
  private get rev(): number { return this.draftKey === undefined ? this.localRev : getDraft<number>(`${this.draftKey}\u0000rev`) ?? 0; }
  private set rev(v: number) { if (this.draftKey === undefined) this.localRev = v; else setDraft(`${this.draftKey}\u0000rev`, v); }
  private localRev = 0;

  /** `save` may return a promise that settles true once the write AND the refresh
   *  after it have landed; a plain void return acknowledges at once. */
  private readonly queue: SerialQueue;

  /** `queueKey` names the item's body: editors for the same body share one save
   *  queue, so a save held by a closed panel cannot land after a reopened one's. */
  constructor(private readonly save: (text: string) => Promise<boolean> | void, label: string, queueKey?: string) {
    this.queue = queueKey === undefined ? new SerialQueue() : queueFor(queueKey);
    this.draftKey = queueKey;
    // A save from a panel that was closed is still unconfirmed: show it, not the stored text.
    if (this.draft !== null) this.text = this.draft;
    this.el = document.createElement("div");
    this.el.className = "af-backlog-body";
    const fallback = document.createElement("pre");
    fallback.className = "af-backlog-body-text";
    this.el.append(fallback);
    bodyEditorLoader.load().then((mod) => {
      if (this.disposed) return;
      this.el.replaceChildren();
      this.editor = mod.createDocsEditor(this.el, { text: this.text, label, onChange: (t) => this.changed(t) });
    }, () => {
      fallback.textContent = this.text;
    });
  }

  /** Shows the stored body. An edit that is typed or not yet acknowledged is kept. Once
   *  every write has settled and nothing is waiting to be saved, a stored body that is
   *  exactly the draft releases it (a save whose confirming read failed is acknowledged
   *  by a later one); while a write is in flight or the debounce is running, a matching
   *  snapshot may be stale and proves nothing. */
  setText(text: string): void {
    if (this.draft !== null && this.draft === text && this.timer === null && !this.queue.busy()) this.draft = null;
    if (this.draft !== null || text === this.text) return;
    this.text = text;
    if (this.editor) {
      if (this.editor.getText() !== text) this.editor.setText(text);
    } else {
      const pre = this.el.querySelector("pre");
      if (pre) pre.textContent = text;
    }
  }

  private changed(text: string): void {
    this.text = text;
    this.draft = text;
    this.rev++;
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = setTimeout(() => this.flush(), SAVE_DELAY_MS);
  }

  flush(): void {
    if (this.timer === null) return;
    clearTimeout(this.timer);
    this.timer = null;
    const text = this.text;
    const rev = this.rev;
    this.queue.submit(() => this.save(text), (ok) => {
      // Later typing has made a newer edit; only the edit this save carried is covered.
      if (ok && this.rev === rev && this.draft === text) this.draft = null;
    });
  }

  dispose(): void {
    this.flush();
    this.disposed = true;
    this.editor?.destroy();
  }
}
