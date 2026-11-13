import assert from "node:assert/strict";
import { test } from "node:test";
import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { editorExtensions, typing } from "./docs_editor.js";

function stateFor(canType: boolean): EditorState {
  return EditorState.create({ doc: "# goals\n", extensions: [...editorExtensions(), typing(canType)] });
}

// #7256: while a doc loads the editor takes no keystrokes, but it must stay focusable. A content
// DOM that is not editable cannot hold focus, so a click on it left the keyboard on the page and
// the app's rail shortcuts took the typing (t opened a terminal tab, 1 left the proposal view).
test("a loading editor is read-only but keeps the keyboard", () => {
  const loading = stateFor(false);
  assert.equal(loading.readOnly, true, "a loading doc takes no input");
  assert.equal(loading.facet(EditorView.editable), true, "its content stays focusable");
});

test("a loaded editor takes typing", () => {
  const loaded = stateFor(true);
  assert.equal(loaded.readOnly, false);
  assert.equal(loaded.facet(EditorView.editable), true);
});
