// The Docs tab's editor: CodeMirror 6 with markdown live preview. It is part of
// the lazy Docs bundle (see docs_pane.ts and build.mjs), never the main bundle.

import { Annotation, Compartment, EditorState, type Extension, Transaction } from "@codemirror/state";
import { EditorView, keymap } from "@codemirror/view";
import { defaultKeymap, history, historyKeymap } from "@codemirror/commands";
import { Language, LanguageSupport, defineLanguageFacet, languageDataProp } from "@codemirror/language";
import { GFM, parser as baseParser } from "@lezer/markdown";
import { nextQuestionOffset } from "./callouts.js";
import { livePreview } from "./md_live_preview.js";
import type { TextChange } from "./text_diff.js";

export interface DocsEditorOptions {
  text: string;
  /** Called after each change that the user made. Not called for setText or
   *  applyRemote. */
  onChange(text: string): void;
  /** The accessible name of the editing area. */
  label: string;
}

export interface DocsEditor {
  getText(): string;
  /** Replaces the whole doc with a fresh undo history, for a switch to another doc. */
  setText(text: string): void;
  /** Applies the agent's or a merge's changes. The cursor, the selection and the
   *  scroll stay with the text around them, and undo does not revert them. */
  applyRemote(changes: readonly TextChange[]): void;
  /** Moves the cursor to the next `> [!question]` callout after the cursor, or,
   *  with `wrap`, to the first one. False if there is none to go to. */
  goToNextQuestion(wrap: boolean, from?: number): boolean;
  /** Moves the cursor to the first question callout of the doc. False if none. */
  goToFirstQuestion(): boolean;
  /** Whether the user can type. Off while a doc is loading, so no keystroke lands
   *  in the previous doc's text or is dropped when the load completes. */
  setEditable(editable: boolean): void;
  focus(): void;
  destroy(): void;
}

const remote = Annotation.define<boolean>();

const markdownData = defineLanguageFacet({ commentTokens: { block: { open: "<!--", close: "-->" } } });

/** Markdown with the GFM extensions (tables, task lists, strikethrough). Built
 *  from @lezer/markdown directly: @codemirror/lang-markdown also bundles the
 *  HTML, CSS and JavaScript languages for embedded code, which this tab does not
 *  need. */
export const markdownLanguage = new Language(
  markdownData,
  baseParser.configure([GFM, { props: [languageDataProp.add({ Document: markdownData })] }]),
  [],
  "markdown",
);

export function editorExtensions(): Extension[] {
  return [
    history(),
    keymap.of([...defaultKeymap, ...historyKeymap]),
    EditorView.lineWrapping,
    new LanguageSupport(markdownLanguage),
    livePreview(),
  ];
}

export function createDocsEditor(parent: HTMLElement, opts: DocsEditorOptions): DocsEditor {
  const editable = new Compartment();
  let isEditable = true;
  const makeState = (text: string): EditorState =>
    EditorState.create({
      doc: text,
      extensions: [
        ...editorExtensions(),
        editable.of([EditorView.editable.of(isEditable), EditorState.readOnly.of(!isEditable)]),
        EditorView.contentAttributes.of({ "aria-label": opts.label, spellcheck: "true" }),
        EditorView.updateListener.of((u) => {
          if (u.docChanged && !u.transactions.some((tr) => tr.annotation(remote))) {
            opts.onChange(u.state.doc.toString());
          }
        }),
      ],
    });

  const view = new EditorView({ state: makeState(opts.text), parent });

  return {
    getText: () => view.state.doc.toString(),
    setText(text) {
      view.setState(makeState(text));
    },
    applyRemote(changes) {
      if (changes.length === 0) return;
      view.dispatch({
        changes: changes.map((c) => ({ from: c.from, to: c.to, insert: c.insert })),
        annotations: [remote.of(true), Transaction.addToHistory.of(false)],
      });
    },
    setEditable(next) {
      isEditable = next;
      view.dispatch({ effects: editable.reconfigure([EditorView.editable.of(next), EditorState.readOnly.of(!next)]) });
    },
    goToFirstQuestion() {
      return this.goToNextQuestion(true, -1);
    },
    goToNextQuestion(wrap, from) {
      const text = view.state.doc.toString();
      const pos = nextQuestionOffset(text, from ?? view.state.selection.main.head, wrap);
      if (pos === null) return false;
      view.dispatch({
        selection: { anchor: pos },
        effects: EditorView.scrollIntoView(pos, { y: "start", yMargin: 48 }),
      });
      view.focus();
      return true;
    },
    focus: () => view.focus(),
    destroy: () => view.destroy(),
  };
}
