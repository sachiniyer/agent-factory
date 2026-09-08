import assert from "node:assert/strict";
import { test } from "node:test";
import { StickyModifiers } from "./terminal-keybar.js";
import { TerminalSoftInput } from "./terminal-soft-input.js";

function composition(type: string, data: string): Event {
  return Object.assign(new Event(type), { data });
}

function insertText(data: string, type = "input", isComposing = false): Event {
  return Object.assign(new Event(type, { cancelable: true }), { data, inputType: "insertText", isComposing });
}

test("compositionstart → update → end → insertText commits once without consuming Ctrl", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = new EventTarget();
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const writes: string[] = [];
  const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
  // Xterm binds this listener before the keybar. CompositionHelper reads the
  // committed textarea value in setTimeout(0), not a microtask, after end/input.
  textarea.addEventListener("compositionend", () => setTimeout(() => send("字"), 0));
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "字"));
  textarea.dispatchEvent(composition("compositionend", "字"));
  host.dispatchEvent(insertText("字")); // isComposing is already false
  t.mock.timers.tick(0);
  assert.deepEqual(writes, ["字"]);
  assert.equal(modifiers.state("Ctrl"), "once");
  host.dispatchEvent(insertText("c"));
  assert.deepEqual(writes, ["字", "\x03"]);
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("composition ownership survives microtasks and leaves beforeinput uncancelled", async t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget(), textarea = new EventTarget();
  let sent = 0;
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => sent++);
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", "字"));
  await Promise.resolve(); // CompositionHelper has not forwarded its commit yet.
  const before = insertText("字", "beforeinput");
  host.dispatchEvent(before);
  assert.equal(before.defaultPrevented, false);
  host.dispatchEvent(insertText("字"));
  assert.equal(sent, 0);
  assert.equal(soft.transform("字", () => "modified"), "字");
  t.mock.timers.tick(0);
  host.dispatchEvent(insertText("x"));
  assert.equal(sent, 1);
});

test("old release leaves the new composition owned; reset and disposal clear all ownership", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget(), textarea = new EventTarget();
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "字"));
  t.mock.timers.tick(0);
  assert.equal(soft.transform("字", () => "modified"), "字");
  soft.reset();
  assert.equal(soft.transform("x", () => "modified"), "modified");
  textarea.dispatchEvent(composition("compositionend", "字"));
  soft.dispose();
  textarea.dispatchEvent(new Event("compositionstart"));
  t.mock.timers.tick(0);
  assert.equal(soft.transform("x", () => "modified"), "modified");
});

for (const [payload, expected, state] of [
  ["字x", "字\x18", "off"],
  ["字", "字", "once"],
  ["x", "\x18", "off"],
] as const) {
  test(`composition commit payload ${payload} modifies only ordinary input`, t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const host = new EventTarget(), textarea = new EventTarget();
    const modifiers = new StickyModifiers();
    modifiers.tap("Ctrl", 0);
    const writes: string[] = [];
    const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
    textarea.addEventListener("compositionend", () => setTimeout(() => send(payload), 0));
    const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
    t.after(() => soft.dispose());
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.dispatchEvent(composition("compositionupdate", "仮"));
    textarea.dispatchEvent(composition("compositionend", "字"));
    t.mock.timers.tick(0);
    assert.deepEqual(writes, [expected]);
    assert.equal(modifiers.state("Ctrl"), state);
    host.dispatchEvent(insertText("x"));
    assert.deepEqual(writes, [expected, state === "once" ? "\x18" : "x"]);
    assert.equal(modifiers.state("Ctrl"), "off");
  });
}

test("compositionupdate supplies the prefix when compositionend omits data", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget(), textarea = new EventTarget();
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "字"));
  textarea.dispatchEvent(new Event("compositionend"));
  assert.equal(soft.transform("字x", text => text.toUpperCase()), "字X");
  soft.reset();
  assert.equal(soft.transform("字x", () => "ordinary"), "ordinary");
});

for (const modifier of ["Ctrl", "Alt"] as const) {
  for (const trailing of ["", "x"]) {
    test(`textarea Korean reshape with ${modifier} and trailing ${JSON.stringify(trailing)}`, t => {
      t.mock.timers.enable({ apis: ["setTimeout"] });
      const host = new EventTarget();
      const textarea = Object.assign(new EventTarget(), { value: "old", selectionStart: 0 });
      const modifiers = new StickyModifiers();
      modifiers.tap(modifier, 0);
      const writes: string[] = [];
      const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
      textarea.addEventListener("compositionend", () => setTimeout(() => send(textarea.value.substring(3)), 0));
      const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
      t.after(() => soft.dispose());
      textarea.dispatchEvent(new Event("compositionstart"));
      textarea.dispatchEvent(composition("compositionupdate", "가"));
      textarea.value = "old각";
      textarea.dispatchEvent(composition("compositionend", "가"));
      host.dispatchEvent(insertText("가")); // native commit observation
      t.mock.timers.tick(0); // The next ordinary key arrives in a later turn.
      if (trailing) host.dispatchEvent(insertText(trailing));
      const modified = modifier === "Ctrl" ? "\x18" : "\x1bx";
      assert.deepEqual(writes, ["각", ...(trailing ? [modified] : [])]);
      assert.equal(modifiers.state(modifier), trailing ? "off" : "once");
      host.dispatchEvent(insertText("x"));
      assert.deepEqual(writes, ["각", ...(trailing ? [modified] : []), trailing ? "x" : modified]);
    });
  }
}

test("deferred textarea commit ignores event data and splits a payload suffix", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "old" });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", "가"));
  host.dispatchEvent(insertText("가", "beforeinput"));
  textarea.value = "old각";
  host.dispatchEvent(insertText("가"));
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  assert.equal(soft.transform("각x", text => modifiers.input(text)), "각\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("finalize reads reshaped textarea contents rather than the compositionend snapshot", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "old" });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "old가";
  textarea.dispatchEvent(composition("compositionend", "가"));
  textarea.value = "old각";
  host.dispatchEvent(insertText("가"));
  const modifiers = new StickyModifiers();
  modifiers.tap("Alt", 0);
  assert.equal(soft.transform("각x", text => modifiers.input(text)), "각\x1bx");
  assert.equal(modifiers.state("Alt"), "off");
});

for (const modifier of ["Ctrl", "Alt"] as const) {
  test(`pending composition A survives B starting with ${modifier} armed`, t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const host = new EventTarget();
    const textarea = Object.assign(new EventTarget(), { value: "" });
    const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
    t.after(() => soft.dispose());
    const modifiers = new StickyModifiers();
    modifiers.tap(modifier, 0);
    const transform = (text: string) => soft.transform(text, value => modifiers.input(value));
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value = "字";
    textarea.dispatchEvent(composition("compositionend", "字"));
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value = "字文";
    textarea.dispatchEvent(composition("compositionupdate", "文"));
    assert.equal(transform("字"), "字");
    assert.equal(modifiers.state(modifier), "once");
    t.mock.timers.tick(0); // A's release must not clear B.
    textarea.dispatchEvent(composition("compositionend", "文"));
    assert.equal(transform("文"), "文");
    assert.equal(modifiers.state(modifier), "once");
    t.mock.timers.tick(0);
    assert.equal(transform("x"), modifier === "Ctrl" ? "\x18" : "\x1bx");
  });

  for (const committed of ["가", "가나"]) {
    test(`final native mutation to ${committed} refreshes the ${modifier} commit boundary`, t => {
      t.mock.timers.enable({ apis: ["setTimeout"] });
      const host = new EventTarget();
      const textarea = Object.assign(new EventTarget(), { value: "old" });
      const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
      t.after(() => soft.dispose());
      const modifiers = new StickyModifiers();
      modifiers.tap(modifier, 0);
      textarea.dispatchEvent(new Event("compositionstart"));
      textarea.value = "oldᄀ";
      textarea.dispatchEvent(composition("compositionend", "ᄀ"));
      textarea.value = "old" + committed;
      assert.equal(soft.transform(committed + "x", value => modifiers.input(value)),
        committed + (modifier === "Ctrl" ? "\x18" : "\x1bx"));
      assert.equal(modifiers.state(modifier), "off");
    });
  }
}

test("two finalized commits match oldest first and each entry is consumed once", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  for (const letter of ["字", "文"]) {
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value += letter;
    textarea.dispatchEvent(composition("compositionend", letter));
  }
  assert.equal(soft.transform("字", () => "modified"), "字");
  assert.equal(soft.transform("文", () => "modified"), "文");
  assert.equal(soft.transform("字", () => "modified"), "modified");
  t.mock.timers.tick(0);
});

test("reset cancels every pending release and drops every finalized range", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget(), textarea = new EventTarget();
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  for (const letter of ["字", "文"]) {
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.dispatchEvent(composition("compositionend", letter));
  }
  soft.reset();
  assert.equal(soft.transform("字", () => "modified"), "modified");
  assert.equal(soft.transform("文", () => "modified"), "modified");
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "新"));
  t.mock.timers.tick(0);
  assert.equal(soft.transform("新", () => "modified"), "新");
});


test("one xterm payload can include two pending commits and an ordinary suffix", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  for (const letter of ["字", "文"]) {
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value += letter;
    textarea.dispatchEvent(composition("compositionend", letter));
  }
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  assert.equal(soft.transform("字文x", text => modifiers.input(text)), "字文\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
  assert.equal(soft.transform("字文", () => "ordinary"), "ordinary");
});

for (const modifier of ["Ctrl", "Alt"] as const) {
  test(`ordinary input without a post-composition commit event consumes ${modifier}`, t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const host = new EventTarget();
    const textarea = Object.assign(new EventTarget(), { value: "old" });
    const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
    t.after(() => soft.dispose());
    const modifiers = new StickyModifiers();
    modifiers.tap(modifier, 0);
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value = "old字";
    textarea.dispatchEvent(composition("compositionend", "字"));
    // No commit insertText is required; finalize before the next key's turn.
    assert.equal(soft.transform("字", value => modifiers.input(value)), "字");
    t.mock.timers.tick(0);
    assert.equal(soft.transform("x", value => modifiers.input(value)),
      modifier === "Ctrl" ? "\x18" : "\x1bx");
    assert.equal(modifiers.state(modifier), "off");
    assert.equal(soft.transform("z", value => modifiers.input(value)), "z");
  });
}

for (const modifier of ["Ctrl", "Alt"] as const) {
  test(`append-shaped final IME mutation stays inside the commit with ${modifier}`, t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const host = new EventTarget();
    const textarea = Object.assign(new EventTarget(), { value: "old" });
    const modifiers = new StickyModifiers();
    modifiers.tap(modifier, 0);
    const writes: string[] = [];
    const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
    textarea.addEventListener("compositionend", () => setTimeout(() => send(textarea.value.substring(3)), 0));
    const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
    t.after(() => soft.dispose());
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.dispatchEvent(composition("compositionupdate", "a"));
    textarea.value = "olda";
    textarea.dispatchEvent(composition("compositionend", "a"));
    host.dispatchEvent(insertText("b", "beforeinput", true));
    textarea.value += "b";
    host.dispatchEvent(insertText("b", "input", true));
    t.mock.timers.tick(0);
    assert.deepEqual(writes, ["ab"]);
    assert.equal(modifiers.state(modifier), "once");
    host.dispatchEvent(insertText("x"));
    assert.deepEqual(writes, ["ab", modifier === "Ctrl" ? "\x18" : "\x1bx"]);
    assert.equal(modifiers.state(modifier), "off");
  });
}

test("unmodified soft input remains native so xterm can observe Backspace", () => {
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const writes: string[] = [];
  host.addEventListener("input", event => {
    if ((event as InputEvent).inputType === "deleteContentBackward") writes.push("\x7f");
  });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => false);
  textarea.dispatchEvent(new Event("keydown"));
  const typed = insertText("a", "beforeinput");
  host.dispatchEvent(typed);
  assert.equal(typed.defaultPrevented, false);
  textarea.value = "a";
  host.dispatchEvent(insertText("a"));
  assert.equal(textarea.value, "a");
  textarea.dispatchEvent(new Event("keyup"));
  const backspace = Object.assign(new Event("beforeinput", { cancelable: true }), {
    inputType: "deleteContentBackward", data: null,
  });
  host.dispatchEvent(backspace);
  assert.equal(backspace.defaultPrevented, false);
  textarea.value = "";
  host.dispatchEvent(Object.assign(new Event("input"), { inputType: "deleteContentBackward", data: null }));
  assert.deepEqual(writes, ["\x7f"]);
  soft.dispose();
});

test("reshaped commit is the first mutation and leaves Ctrl for the next key", t => {
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "old" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "oldᄀ";
  textarea.dispatchEvent(composition("compositionend", "가"));
  textarea.value = "old각";
  host.dispatchEvent(insertText("가"));
  assert.equal(soft.transform("각", value => modifiers.input(value)), "각");
  assert.equal(modifiers.state("Ctrl"), "once");
  assert.equal(soft.transform("x", value => modifiers.input(value)), "\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("repeated commit character then ordinary character consumes Ctrl once", t => {
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "x";
  textarea.dispatchEvent(composition("compositionend", "x"));
  host.dispatchEvent(insertText("x"));
  textarea.value = "xx";
  host.dispatchEvent(insertText("x"));
  assert.equal(soft.transform("xx", value => modifiers.input(value)), "x\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("stale keydown recovers every composed input until a key lifecycle resumes", () => {
  const host = new EventTarget();
  const textarea = new EventTarget();
  const writes: string[] = [];
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => false);
  textarea.dispatchEvent(new Event("keydown"));
  textarea.dispatchEvent(new Event("blur"));
  textarea.dispatchEvent(new Event("focus"));
  for (const letter of ["a", "b", "c"]) {
    const input = insertText(letter, "beforeinput", true);
    host.dispatchEvent(input);
    assert.equal(input.defaultPrevented, true);
  }
  assert.deepEqual(writes, ["a", "b", "c"]);
  textarea.dispatchEvent(new Event("keydown"));
  textarea.dispatchEvent(new Event("keyup"));
  const native = insertText("d", "beforeinput", true);
  host.dispatchEvent(native);
  assert.equal(native.defaultPrevented, false);
  assert.deepEqual(writes, ["a", "b", "c"]);
  soft.dispose();
});

test("a fresh keydown after blur keeps native input ownership", () => {
  const host = new EventTarget();
  const textarea = new EventTarget();
  const writes: string[] = [];
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => false);
  textarea.dispatchEvent(new Event("keydown"));
  textarea.dispatchEvent(new Event("blur"));
  textarea.dispatchEvent(new Event("keydown"));
  const input = insertText("a", "beforeinput", true);
  host.dispatchEvent(input);
  assert.equal(input.defaultPrevented, false);
  assert.deepEqual(writes, []);
  textarea.dispatchEvent(new Event("keyup"));
  soft.dispose();
});

test("keydown after compositionend marks first ordinary input as trailing", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "字";
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.dispatchEvent(new Event("keydown"));
  textarea.value = "字x";
  host.dispatchEvent(insertText("x"));
  assert.equal(soft.transform("字x", value => modifiers.input(value)), "字\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("commit mutation before keydown freezes boundary before ordinary input", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "字";
  textarea.dispatchEvent(composition("compositionend", "字"));
  host.dispatchEvent(insertText("字"));
  textarea.dispatchEvent(new Event("keydown"));
  textarea.value = "字x";
  host.dispatchEvent(insertText("x"));
  assert.equal(soft.transform("字x", value => modifiers.input(value)), "字\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("armed modifier intercepts ordinary input without stale keydown", () => {
  const host = new EventTarget();
  const textarea = new EventTarget();
  const writes: string[] = [];
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => true);
  const input = insertText("a", "beforeinput");
  host.dispatchEvent(input);
  assert.equal(input.defaultPrevented, true);
  assert.deepEqual(writes, ["a"]);
  soft.dispose();
});
