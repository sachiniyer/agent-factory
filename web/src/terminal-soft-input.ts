interface CompositionRange {
  start?: number;
  initialValue?: string;
  text: string;
  updateText?: string;
  sawUpdate?: boolean;
  textareaChanged?: boolean;
  provisionalAtEnd?: boolean;
  postEndInputSeen?: boolean;
  frozenText?: string;
  commitLength?: number;
  trailingLength?: number;
  keydownAfterEnd?: boolean;
  trailingFlush?: TrailingFlush;
  release?: ReturnType<typeof setTimeout>;
}

interface TrailingFlush {
  text: string;
  release?: ReturnType<typeof setTimeout>;
}

function insertedTextareaText(before: string, after: string): string {
  let prefix = 0;
  while (prefix < before.length && prefix < after.length && before[prefix] === after[prefix]) prefix += 1;
  let suffix = 0;
  while (suffix < before.length - prefix && suffix < after.length - prefix &&
    before[before.length - suffix - 1] === after[after.length - suffix - 1]) suffix += 1;
  return after.slice(prefix, after.length - suffix);
}

/** Capture native phone input before xterm's stale-keydown fallback can drop it. */
export class TerminalSoftInput {
  private active: CompositionRange | undefined;
  private readonly pending: CompositionRange[] = [];
  private readonly trailingFlushes = new Set<TrailingFlush>();
  private forwardingTrailing: string | undefined;
  private keyDownSeen = false;
  private staleKeydown = false;
  private staleBeforeInputSent = false;
  private staleBeforeValue: string | undefined;
  private readonly onKeyDown = (event: Event): void => {
    this.keyDownSeen = true;
    this.staleKeydown = false;
    this.staleBeforeInputSent = false;
    this.staleBeforeValue = undefined;
    const range = this.pending.at(-1);
    const keyCode = (event as KeyboardEvent).keyCode;
    // Only a key that xterm accepted past its custom handler can make
    // CompositionHelper finalize the pending commit before handling that key.
    if (range && this.keydownReachesCompositionHelper(event) &&
      ![16, 17, 18, 229].includes(keyCode)) range.keydownAfterEnd = true;
  };
  private readonly onKeyUp = (): void => {
    this.keyDownSeen = false;
    this.staleKeydown = false;
    this.staleBeforeInputSent = false;
    this.staleBeforeValue = undefined;
  };
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
      this.queueTrailingFlush(range);
    }
    this.active = { start: value?.length, initialValue: value, text: "" };
  };
  private readonly onCompositionUpdate = (event: Event): void => {
    const data = (event as CompositionEvent).data;
    if (this.active) {
      this.observeCompositionValue(this.active);
      this.active.sawUpdate = true;
      if (typeof data === "string") this.active.text = this.active.updateText = data;
    }
  };
  private readonly onCompositionEnd = (event: Event): void => {
    this.active ??= { text: "" };
    const range = this.active;
    this.observeCompositionValue(range);
    const data = (event as CompositionEvent).data;
    if (typeof data === "string") range.text = data;
    this.active = undefined;
    const value = this.textarea?.value;
    const endText = range.start !== undefined && value !== undefined ? value.substring(range.start) : undefined;
    // Only a prior composition update plus a strict live textarea prefix proves
    // that the boundary is provisional. compositionend.data alone may be stale
    // or differently normalized, so it cannot claim the next ordinary input.
    range.provisionalAtEnd = endText !== undefined && endText.length > 0 &&
      range.updateText !== undefined && range.updateText.length > endText.length &&
      range.updateText.startsWith(endText);
    // Chrome mutates the textarea and emits its composing insertText before
    // compositionend. Freeze that browser-order boundary now so a later soft
    // character without keydown cannot be mistaken for the commit. Safari
    // reaches compositionend before mutating, so its first later insertText
    // establishes the boundary in onInput instead.
    if (range.start !== undefined && value !== undefined && value.length > range.start)
      range.commitLength = value.length - range.start;
    // An empty composition that changed and returned to its starting textarea
    // value is a rollback even if it emitted updates. Safari's empty pre-mutation
    // lifecycle remains pending; the no-update case is the explicit cancel form.
    else if (!range.text && ((range.textareaChanged && value === range.initialValue) || !range.sawUpdate))
      range.commitLength = 0;
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
      if (this.active && input.type === "input")
        this.observeCompositionValue(this.active);
      const range = this.pending.at(-1);
      if (range && input.inputType === "insertText" && input.type === "input" &&
        input.isComposing === false) {
        const value = this.textarea?.value;
        const mutationLength = value !== undefined && range.start !== undefined && value.length > range.start
          ? value.length - range.start : undefined;
        const fallbackLength = input.data?.length;
        const firstCommitGrowth = range.provisionalAtEnd && !range.postEndInputSeen && !range.keydownAfterEnd &&
          range.commitLength !== undefined && (mutationLength ?? fallbackLength ?? 0) > range.commitLength;
        if (range.keydownAfterEnd) {
          range.trailingLength = mutationLength !== undefined
            ? Math.max(range.trailingLength ?? 0, mutationLength - (range.commitLength ?? 0))
            : (range.trailingLength ?? 0) + (input.data?.length ?? 0);
        } else if (range.commitLength === undefined) {
          // The textarea mutation is the commit boundary. InputEvent.data is
          // nullable on real IMEs and is only a fallback when no value is exposed.
          range.commitLength = mutationLength ?? input.data?.length;
        } else if (firstCommitGrowth) {
          // The first post-end mutation completed a provisional commit. Prefer
          // the live textarea boundary; nullable event data is only a fallback.
          range.commitLength = mutationLength ?? fallbackLength;
        } else {
          range.trailingLength = mutationLength !== undefined
            ? Math.max(range.trailingLength ?? 0, mutationLength - range.commitLength)
            : (range.trailingLength ?? 0) + (input.data?.length ?? 0);
        }
        range.postEndInputSeen = true;
      }
      // Preserve beforeinput's native mutation; CompositionHelper owns sending.
      if (input.type === "input" && input.inputType === "insertText") input.stopImmediatePropagation();
      return;
    }
    if (this.physicalInput() || input.inputType !== "insertText") return;
    if (this.staleKeydown) {
      // Xterm drops the composed input while its keydown flag is stale. Send
      // beforeinput ourselves, but preserve the native textarea mutation so a
      // later 229 Backspace can diff it. Some IMEs expose data only on input,
      // so remember whether its paired beforeinput was already forwarded.
      if (input.type === "beforeinput") {
        this.staleBeforeInputSent = false;
        this.staleBeforeValue = this.textarea?.value;
        if (!input.data) return;
        input.stopImmediatePropagation();
        this.staleBeforeInputSent = true;
        this.send(input.data);
      } else if (input.type === "input") {
        input.stopImmediatePropagation();
        const value = this.textarea?.value;
        const observed = value !== undefined && this.staleBeforeValue !== undefined
          ? insertedTextareaText(this.staleBeforeValue, value) : "";
        const recovered = observed || input.data;
        if (recovered && !this.staleBeforeInputSent) this.send(recovered);
        this.staleBeforeInputSent = false;
        this.staleBeforeValue = undefined;
      }
      return;
    }
    if (!input.data) return;
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
    private readonly hasArmedModifier: () => boolean = () => true,
    private readonly keydownReachesCompositionHelper: (event: Event) => boolean = () => true) {
    textarea?.addEventListener("compositionstart", this.onCompositionStart);
    textarea?.addEventListener("compositionupdate", this.onCompositionUpdate);
    textarea?.addEventListener("compositionend", this.onCompositionEnd);
    textarea?.addEventListener("keydown", this.onKeyDown, true);
    textarea?.addEventListener("keyup", this.onKeyUp, true);
    textarea?.addEventListener("blur", this.onBlur, true);
    host.addEventListener("beforeinput", this.onInput, true);
    host.addEventListener("input", this.onInput, true);
  }

  transform(text: string, applyModifiers: (text: string, userInput: boolean) => string): string {
    // A rescued A→B interstitial is ordinary input even when it happens to
    // equal B's current composition prefix.
    if (this.forwardingTrailing === text) return applyModifiers(text, true);
    // Oldest finalized commit first; the active composition has separate state.
    const ranges = this.active ? [...this.pending, this.active] : [...this.pending];
    let rest = text, prefix = "", matchedComposition = false;
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
      if (!length) {
        const trailingLength = Math.min(range.trailingLength ?? 0, committed.length);
        const trailing = boundary === 0 && trailingLength ? committed.slice(-trailingLength) : "";
        if (trailing && rest.startsWith(trailing)) {
          prefix += applyModifiers(trailing, true);
          rest = rest.slice(trailing.length);
          this.remove(range);
          if (!rest) break;
        }
        continue;
      }
      matchedComposition = true;
      prefix += rest.slice(0, length);
      rest = rest.slice(length);
      const flush = range.trailingFlush;
      if (flush && rest.startsWith(flush.text)) {
        this.cancelTrailingFlush(flush);
        range.trailingFlush = undefined;
        prefix += applyModifiers(flush.text, true);
        rest = rest.slice(flush.text.length);
      }
      this.remove(range);
      if (!rest) break;
    }
    return prefix + (rest ? applyModifiers(rest, matchedComposition) : "");
  }

  private remove(range: CompositionRange): void {
    if (range.release !== undefined) clearTimeout(range.release);
    const index = this.pending.indexOf(range);
    if (index !== -1) this.pending.splice(index, 1);
    if (this.active === range) this.active = undefined;
  }
  private observeCompositionValue(range: CompositionRange): void {
    const value = this.textarea?.value;
    if (value !== undefined && range.initialValue !== undefined && value !== range.initialValue)
      range.textareaChanged = true;
  }
  private queueTrailingFlush(range: CompositionRange): void {
    const trailingLength = range.trailingLength ?? 0;
    if (!trailingLength || !range.frozenText || range.trailingFlush) return;
    const text = range.frozenText.slice(-trailingLength);
    const flush: TrailingFlush = { text };
    range.trailingFlush = flush;
    this.trailingFlushes.add(flush);
    flush.release = setTimeout(() => {
      this.trailingFlushes.delete(flush);
      if (range.trailingFlush === flush) range.trailingFlush = undefined;
      this.forwardingTrailing = text;
      try { this.send(text); } finally { this.forwardingTrailing = undefined; }
    }, 0);
  }
  private cancelTrailingFlush(flush: TrailingFlush): void {
    if (flush.release !== undefined) clearTimeout(flush.release);
    this.trailingFlushes.delete(flush);
  }
  reset(): void {
    for (const range of this.pending) if (range.release !== undefined) clearTimeout(range.release);
    for (const flush of this.trailingFlushes) if (flush.release !== undefined) clearTimeout(flush.release);
    this.trailingFlushes.clear();
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
