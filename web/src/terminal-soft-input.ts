interface CompositionRange {
  start?: number;
  text: string;
  frozenText?: string;
  trailing?: string;
  release?: ReturnType<typeof setTimeout>;
}

/** Capture native phone input before xterm's stale-keydown fallback can drop it. */
export class TerminalSoftInput {
  private active: CompositionRange | undefined;
  private readonly pending: CompositionRange[] = [];
  private readonly onCompositionStart = (): void => {
    // Xterm retains a finalized range when another composition starts before
    // its delayed send (CompositionHelper.ts:137-163). Freeze that boundary,
    // but never cancel the old entry or its independently scoped release.
    const value = this.textarea?.value;
    for (const range of this.pending) {
      if (range.frozenText === undefined && range.start !== undefined && value !== undefined) {
        range.frozenText = value.substring(range.start);
      }
    }
    this.active = { start: value?.length, text: "" };
  };
  private readonly onCompositionUpdate = (event: Event): void => {
    const data = (event as CompositionEvent).data;
    if (this.active && typeof data === "string") this.active.text = data;
  };
  private readonly onCompositionEnd = (event: Event): void => {
    this.active ??= { text: "" };
    this.onCompositionUpdate(event);
    const range = this.active;
    this.active = undefined;
    this.pending.push(range);
    // Bound on the textarea AFTER xterm: its commit timer runs before this
    // release. Pending membership is the IME mutation window: every native
    // mutation in this turn belongs to the commit, even an append-shaped one.
    // Removing the range closes that window before the next ordinary key.
    // A microtask would expire ownership before the final mutation.
    range.release = setTimeout(() => this.remove(range), 0);
  };
  private readonly onInput = (event: Event): void => {
    const input = event as InputEvent;
    if (!this.enabled()) return;
    if (this.active || this.pending.length) {
      const range = this.pending.at(-1);
      if (range && input.inputType === "insertText" && input.type === "input" &&
        input.isComposing === false && input.data && input.data !== range.text)
        range.trailing = (range.trailing ?? "") + input.data;
      // Preserve beforeinput's native mutation; CompositionHelper owns sending.
      if (input.type === "input" && input.inputType === "insertText") input.stopImmediatePropagation();
      return;
    }
    if (this.physicalInput() || input.isComposing || !this.hasArmedModifier() ||
      input.inputType !== "insertText" || !input.data) return;
    if (input.type === "beforeinput" && !input.cancelable) return;
    input.preventDefault();
    input.stopImmediatePropagation();
    this.send(input.data);
  };

  constructor(private readonly host: EventTarget, private readonly textarea: (EventTarget & { value?: string }) | null,
    private readonly enabled: () => boolean, private readonly physicalInput: () => boolean,
    private readonly send: (text: string) => void,
    private readonly hasArmedModifier: () => boolean = () => true) {
    textarea?.addEventListener("compositionstart", this.onCompositionStart);
    textarea?.addEventListener("compositionupdate", this.onCompositionUpdate);
    textarea?.addEventListener("compositionend", this.onCompositionEnd);
    host.addEventListener("beforeinput", this.onInput, true);
    host.addEventListener("input", this.onInput, true);
  }

  transform(text: string, applyModifiers: (text: string) => string): string {
    // Oldest finalized commit first; the active composition has separate state.
    const ranges = this.active ? [...this.pending, this.active] : [...this.pending];
    let rest = text, prefix = "";
    for (const range of ranges) {
      const value = this.textarea?.value;
      const committed = range.frozenText ?? (range.start !== undefined && value !== undefined
        ? value.substring(range.start) : range.text);
      const boundary = Math.max(0, committed.length - (range.trailing?.length ?? 0));
      // Xterm may send just the old range or include ordinary trailing input.
      // Match the live substring, never a provisional compositionend length.
      let length = 0;
      for (const character of committed.slice(0, boundary)) {
        if (!rest.startsWith(character, length)) break;
        length += character.length;
      }
      if (!length) continue;
      this.remove(range);
      prefix += rest.slice(0, length);
      rest = rest.slice(length);
      if (!rest) break;
    }
    return prefix + (rest ? applyModifiers(rest) : "");
  }

  private remove(range: CompositionRange): void {
    if (range.release !== undefined) clearTimeout(range.release);
    const index = this.pending.indexOf(range);
    if (index !== -1) this.pending.splice(index, 1);
    if (this.active === range) this.active = undefined;
  }
  reset(): void {
    for (const range of this.pending) if (range.release !== undefined) clearTimeout(range.release);
    this.pending.length = 0;
    this.active = undefined;
  }
  dispose(): void {
    this.reset();
    this.textarea?.removeEventListener("compositionstart", this.onCompositionStart);
    this.textarea?.removeEventListener("compositionupdate", this.onCompositionUpdate);
    this.textarea?.removeEventListener("compositionend", this.onCompositionEnd);
    this.host.removeEventListener("beforeinput", this.onInput, true);
    this.host.removeEventListener("input", this.onInput, true);
  }
}
