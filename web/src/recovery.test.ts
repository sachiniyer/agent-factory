import assert from "node:assert/strict";
import { test } from "node:test";
import { appendMutationOutcome, renderMutationOutcome } from "./recovery.js";

class ElementStub {
  className = "";
  children: (ElementStub | string)[] = [];
  append(child: ElementStub | string): void { this.children.push(child); }
  setAttribute(): void {}
  get textContent(): string {
    return this.children.map(child => typeof child === "string" ? child : child.textContent).join(" ");
  }
}

for (const kind of ["uncertain", "confirmed", "failed"] as const) {
  test(`mutation outcome rendering preserves ${kind} semantics`, (t) => {
    const previous = Object.getOwnPropertyDescriptor(globalThis, "document");
    Object.defineProperty(globalThis, "document", { configurable: true, value: {
      createElement: () => new ElementStub(),
      documentElement: { getAttribute: () => "dark" },
    } });
    t.after(() => {
      if (previous) Object.defineProperty(globalThis, "document", previous);
      else Reflect.deleteProperty(globalThis, "document");
    });
    const notice = renderMutationOutcome({ kind, detail: "The session outcome detail." }) as unknown as ElementStub;
    assert.match(notice.textContent, /The session outcome detail/);
    const heading = notice.children[0] as ElementStub;
    if (kind === "uncertain") {
      assert.equal(heading.textContent, "Outcome not confirmed");
      assert.doesNotMatch(notice.textContent, /failed|try again/i);
      assert.match(notice.textContent, /Check the session/);
      assert.equal(heading.className, "");
    } else if (kind === "confirmed") {
      assert.equal(heading.textContent, "Operation completed");
      assert.doesNotMatch(notice.textContent, /failed|try again/i);
      assert.match(notice.textContent, /Review the details before taking further action/);
      assert.equal(heading.className, "");
    } else {
      assert.equal(heading.textContent, "Operation failed");
      assert.match(notice.textContent, /Review the details, then try again/);
      assert.equal(heading.className, "af-recovery-failed");
    }
  });
}

test("overlapping notices retain both details and uncertainty in either arrival order", () => {
  const uncertain = { kind: "uncertain" as const, detail: "Archive outcome unknown." };
  const failed = { kind: "failed" as const, detail: "Create refused." };
  for (const [first, second] of [[uncertain, failed], [failed, uncertain]]) {
    const combined = appendMutationOutcome(appendMutationOutcome(undefined, first), second);
    assert.equal(combined.kind, "uncertain");
    assert.equal(combined.detail, `${first.detail}\n\n${second.detail}`);
  }
  const confirmed = { kind: "confirmed" as const, detail: "Archive completed with a warning." };
  assert.equal(appendMutationOutcome(confirmed, failed).kind, "confirmed");
  assert.equal(appendMutationOutcome(failed, confirmed).kind, "confirmed");
  assert.equal(appendMutationOutcome(uncertain, confirmed).kind, "uncertain");
  assert.equal(appendMutationOutcome(confirmed, uncertain).kind, "uncertain");
  assert.equal(appendMutationOutcome(failed, failed).kind, "failed");
});
