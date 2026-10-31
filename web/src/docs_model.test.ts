import assert from "node:assert/strict";
import { test } from "node:test";
import { changeAction, nextQuestionDoc, orderDocs, questionCountLabel, statusText } from "./docs_model.js";
import type { ProposalChange } from "./proposal_api.js";

const doc = (name: string) => ({ name, file: `${name}.md` });

test("docs are in template order, then alphabetical", () => {
  const ordered = orderDocs(["zeta", "api", "alpha", "goals", "future"].map(doc)).map((d) => d.name);
  assert.deepEqual(ordered, ["goals", "api", "future", "alpha", "zeta"]);
});

test("status copy follows the spec", () => {
  assert.equal(statusText("saved"), "");
  assert.equal(statusText("saving"), "Saving…");
  assert.equal(statusText("pending"), "Saving…");
  assert.equal(statusText("merged"), "Merged agent edits");
  assert.equal(statusText("error", "cannot reach the daemon"), "Not saved · cannot reach the daemon");
});

test("changeAction routes an event to the open doc, another doc or the list", () => {
  const docs = ["goals", "api"].map(doc);
  const ev = (over: Partial<ProposalChange>): ProposalChange =>
    ({ repoId: "", slug: "auth", doc: "api.md", commit: "c", author: "agent", ...over });
  assert.equal(changeAction(ev({}), "auth", docs[1]!, docs), "open-doc");
  assert.equal(changeAction(ev({}), "auth", docs[0]!, docs), "other-doc");
  assert.equal(changeAction(ev({ doc: "new.md" }), "auth", docs[0]!, docs), "doc-list");
  assert.equal(changeAction(ev({ doc: "" }), "auth", docs[0]!, docs), "doc-list");
  assert.equal(changeAction(ev({ slug: "other" }), "auth", docs[1]!, docs), "ignore");
});

test("next question stays in the doc, then moves on in strip order, then wraps", () => {
  const docs = ["goals", "technical", "api"].map(doc);
  const counts = new Map([["goals", 1], ["technical", 0], ["api", 2]]);
  assert.deepEqual(nextQuestionDoc(docs, docs[0]!, counts, true), { doc: docs[0], here: true });
  assert.deepEqual(nextQuestionDoc(docs, docs[0]!, counts, false), { doc: docs[2], here: false });
  assert.deepEqual(nextQuestionDoc(docs, docs[2]!, counts, false), { doc: docs[0], here: false });
  const onlyHere = new Map([["goals", 1]]);
  assert.deepEqual(nextQuestionDoc(docs, docs[0]!, onlyHere, false), { doc: docs[0], here: true });
  assert.equal(nextQuestionDoc(docs, docs[0]!, new Map(), false), null);
});

test("question count label", () => {
  assert.equal(questionCountLabel(1), "1 open question");
  assert.equal(questionCountLabel(3), "3 open questions");
});
