import assert from "node:assert/strict";
import { test } from "node:test";
import { calloutKindOfLine, countOpenQuestions, findCallouts, nextQuestionOffset } from "./callouts.js";

test("calloutKindOfLine reads the three callout tags, in any case", () => {
  assert.equal(calloutKindOfLine("> [!question] Why?"), "question");
  assert.equal(calloutKindOfLine(">[!TODO]"), "todo");
  assert.equal(calloutKindOfLine("   > [!Decision] Use JWT"), "decision");
  assert.equal(calloutKindOfLine("> [!note] Other"), null);
  assert.equal(calloutKindOfLine("[!question] not quoted"), null);
  assert.equal(calloutKindOfLine("    > [!question] indented code"), null);
});

test("a callout runs over the quote lines that follow it", () => {
  const text = "intro\n> [!question] Expire?\n> Body line\n\n> [!todo] Next\n> [!decision] Two in a row\nafter";
  const callouts = findCallouts(text);
  assert.deepEqual(callouts.map((c) => [c.kind, c.line, text.slice(c.from, c.to)]), [
    ["question", 1, "> [!question] Expire?\n> Body line"],
    ["todo", 4, "> [!todo] Next"],
    ["decision", 5, "> [!decision] Two in a row"],
  ]);
});

test("callouts inside fenced code do not count", () => {
  const text = "```md\n> [!question] example\n```\n~~~\n> [!question] also\n~~~\n> [!question] real";
  assert.equal(countOpenQuestions(text), 1);
});

test("nextQuestionOffset moves forward and wraps only when asked", () => {
  const text = "> [!question] one\n\n> [!todo] t\n\n> [!question] two";
  const one = 0;
  const two = text.lastIndexOf("> [!question]");
  assert.equal(nextQuestionOffset(text, -1), one);
  assert.equal(nextQuestionOffset(text, 0), two);
  assert.equal(nextQuestionOffset(text, two), one);
  assert.equal(nextQuestionOffset(text, two, false), null);
  assert.equal(nextQuestionOffset("no callouts", 0), null);
});
