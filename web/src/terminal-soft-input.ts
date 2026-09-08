interface CompositionRange {
  start?: number;
  text: string;
  frozenText?: string;
  commitLength?: number;
  trailingLength?: number;
  keydownAfterEnd?: boolean;
  release?: ReturnType<typeof setTimeout>;
}

/** Capture native phone input before xterm's stale-keydown fallback can drop it. */
export class TerminalSoftInput {
  private active: CompositionRange | undefined;
  private readonly pending: CompositionRange[] = [];
  private keyDownSeen = false;
  private staleKeydown = false;
  private readonly onKeyDown = (event: Event): void => {
    this.keyDownSeen = true;
    this.staleKeydown = false;
    const range = this.pending.at(-1);
    // Xterm treats 229 as IME input, not an ordinary post-composition key.
    if (range && (event as KeyboardEvent).keyCode !== 229) range.keydownAfterEnd = true;
  };
  private readonly onKeyUp = (): void => { this.keyDownSeen = false; this.staleKeydown = false; };
  private readonly onBlur = (): void => { if (this.keyDownSeen) this.staleKeydown = true; };
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
    const value = this.textarea?.value;
    // Chrome mutates the textarea and emits its composing insertText before
    // compositionend. Freeze that browser-order boundary now so a later soft
    // character without keydown cannot be mistaken for the commit. Safari
    // reaches compositionend before mutating, so its first later insertText
    // establishes the boundary in onInput instead.
    if (range.start !== undefined && value !== undefined && value.length > range.start)
      range.commitLength = value.length - range.start;
    else if (!range.text) range.commitLength = 0;
    this.pending.push(range);
    // Bound on the textarea AFTER xterm: its commit timer runs before this
    // release. Pending membership keeps that delayed send owned while Safari
    // establishes its commit boundary or Chrome receives trailing input.
    // A microtask would expire ownership before Safari's final mutation.
    range.release = setTimeout(() => this.remove(range), 0);
  };
  private readonly onInput = (event: Event): void => {
    const input = event as InputEvent;
    if (!this.enabled()) return;
    if (this.active || this.pending.length) {
      const range = this.pending.at(-1);
      if (range && input.inputType === "insertText" && input.type === "input" &&
        input.isComposing === false) {
        const value = this.textarea?.value;
        const mutationLength = value !== undefined && range.start !== undefined && value.length > range.start
          ? value.length - range.start : undefined;
        if (range.keydownAfterEnd) {
          range.trailingLength = mutationLength !== undefined
            ? Math.max(range.trailingLength ?? 0, mutationLength - (range.commitLength ?? 0))
            : (range.trailingLength ?? 0) + (input.data?.length ?? 0);
        } else if (range.commitLength === undefined) {
          // The textarea mutation is the commit boundary. InputEvent.data is
          // nullable on real IMEs and is only a fallback when no value is exposed.
          range.commitLength = mutationLength ?? input.data?.length;
        } else {
          range.trailingLength = mutationLength !== undefined
            ? Math.max(range.trailingLength ?? 0, mutationLength - range.commitLength)
            : (range.trailingLength ?? 0) + (input.data?.length ?? 0);
        }
      }
      // Preserve beforeinput's native mutation; CompositionHelper owns sending.
      if (input.type === "input" && input.inputType === "insertText") input.stopImmediatePropagation();
      return;
    }
    if (this.physicalInput() || input.inputType !== "insertText" || !input.data) return;
    if (this.staleKeydown) {
      // Xterm drops the composed input while its keydown flag is stale. Send
      // beforeinput ourselves, but preserve the native textarea mutation so a
      // later 229 Backspace can diff it; intercept input to avoid sending twice.
      input.stopImmediatePropagation();
      if (input.type === "beforeinput") this.send(input.data);
      return;
    }
    if (!this.hasArmedModifier()) return;
    // Armed input differs: xterm would send the unmodified character, so cancel
    // the native event before sending the transformed bytes ourselves.
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
    textarea?.addEventListener("keydown", this.onKeyDown, true);
    textarea?.addEventListener("keyup", this.onKeyUp, true);
    textarea?.addEventListener("blur", this.onBlur, true);
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
      const boundary = Math.min(committed.length,
        range.commitLength ?? (committed.length - (range.trailingLength ?? 0)));
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
    this.textarea?.removeEventListener("keydown", this.onKeyDown, true);
    this.textarea?.removeEventListener("keyup", this.onKeyUp, true);
    this.textarea?.removeEventListener("blur", this.onBlur, true);
    this.host.removeEventListener("beforeinput", this.onInput, true);
    this.host.removeEventListener("input", this.onInput, true);
  }
}
