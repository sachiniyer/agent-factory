// The tabs of a doc that were closed and may still be writing (a final save in flight, a
// retry, a read whose draft it will restore). A tab opened on the doc waits for them on its
// first read, and takes them over once it has loaded. The pane and the model test share this
// code, so the test drives the bookkeeping itself rather than a copy of it.

export interface ClosingTab {
  whenQuiet(): Promise<void>;
  abandon(keepDraft: boolean): void;
}

export class ClosingTabs<T extends ClosingTab> extends Map<string, Set<T>> {
  /** A tab was closed: it is tracked until it is quiet. */
  track(key: string, tab: T): void {
    const set = this.get(key) ?? new Set<T>();
    set.add(tab);
    this.set(key, set);
    void tab.whenQuiet().then(() => {
      set.delete(tab);
      if (set.size === 0 && this.get(key) === set) this.delete(key);
    });
  }

  /** The tabs a newly opened tab's first read must wait for. */
  priors(key: string, loader: T): T[] {
    return [...(this.get(key) ?? [])].filter((p) => p !== loader);
  }

  /** `loader` has loaded and restored the doc's stored draft: the tabs that closed on the
   *  doc are redundant writers, and are abandoned (the draft is kept). A loader that was
   *  itself closed meanwhile is tracked like any closed tab, and takes nothing over. */
  takeOver(key: string, loader: T, loaderIsOpen: boolean): void {
    if (!loaderIsOpen) return;
    for (const old of this.get(key) ?? []) if (old !== loader) old.abandon(true);
    this.delete(key);
  }
}
