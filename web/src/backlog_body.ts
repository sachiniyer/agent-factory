// An item body editor with debounced autosave. The editor comes through a loader
// seam: today a plain textarea, and the CodeMirror live preview from the lazy Docs
// bundle can replace it by swapping bodyEditorLoader once that bundle is on master.

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
  private draft: string | null = null;
  private saving = false;
  private queued = false;
  private disposed = false;

  /** `save` may return a promise that settles true once the write AND the refresh
   *  after it have landed; a plain void return acknowledges at once. */
  constructor(private readonly save: (text: string) => Promise<boolean> | void, label: string) {
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

  /** Shows the stored body. An edit that is typed or not yet acknowledged is kept. */
  setText(text: string): void {
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
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = setTimeout(() => this.flush(), SAVE_DELAY_MS);
  }

  flush(): void {
    if (this.timer === null) return;
    clearTimeout(this.timer);
    this.timer = null;
    this.send();
  }

  /** One save at a time: an older request must not commit after a newer one and
   *  overwrite it, so an edit made while a save is in flight waits for it and
   *  then goes out as the latest text. */
  private send(): void {
    if (this.saving) {
      this.queued = true;
      return;
    }
    const text = this.text;
    const done = this.save(text);
    const acknowledge = (ok: boolean): void => {
      // Newer typing has replaced the draft; only the edit this save carried is covered.
      if (ok && this.draft === text) this.draft = null;
    };
    if (!done) {
      acknowledge(true);
      return;
    }
    this.saving = true;
    const finish = (ok: boolean): void => {
      this.saving = false;
      acknowledge(ok);
      if (!this.queued) return;
      this.queued = false;
      if (this.draft !== null && this.draft !== text) this.send();
    };
    done.then(finish, () => finish(false));
  }

  dispose(): void {
    this.flush();
    this.disposed = true;
    this.editor?.destroy();
  }
}
