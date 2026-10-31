import assert from "node:assert/strict";
import { test } from "node:test";
import { PushChip, pushView, SENT_VISIBLE_MS, type PushView } from "./docs_push.js";
import type { PushStatus } from "./proposal_api.js";

function status(over: Partial<PushStatus> = {}): PushStatus {
  return { sessionId: "s1", slug: "plan", mode: "auto", state: "idle", dueInMs: null, unsent: 0, ...over };
}

class Clock {
  t = 1_000_000;
  timers: { fn: () => void; at: number; live: boolean }[] = [];
  views: (PushView | null)[] = [];
  chip = new PushChip({
    now: () => this.t,
    setTimer: (fn, ms) => {
      const timer = { fn, at: this.t + ms, live: true };
      this.timers.push(timer);
      return timer;
    },
    clearTimer: (handle) => {
      (handle as { live: boolean }).live = false;
    },
    render: (view) => this.views.push(view),
  });

  advance(ms: number): void {
    const end = this.t + ms;
    for (;;) {
      const next = this.timers.filter((x) => x.live && x.at <= end).sort((a, b) => a.at - b.at)[0];
      if (!next) break;
      this.t = next.at;
      next.live = false;
      next.fn();
    }
    this.t = end;
  }

  get last(): PushView | null {
    return this.views[this.views.length - 1] ?? null;
  }

  live(): number {
    return this.timers.filter((x) => x.live).length;
  }
}

test("pushView: auto mode copy and actions for each state", () => {
  assert.deepEqual(pushView(status({ state: "countdown", dueInMs: 4000, unsent: 1 }), 3200, true), {
    mode: "auto", state: "countdown", text: "Sending to agent in 4s", action: "hold", actionEnabled: true, busy: false,
    unsent: 1,
  });
  assert.equal(pushView(status({ state: "countdown" }), 0, true).text, "Sending to agent…");
  const waiting = pushView(status({ state: "waiting_agent", unsent: 2 }), 0, true);
  assert.equal(waiting.text, "Waits for agent");
  assert.equal(waiting.action, null);
  const held = pushView(status({ state: "held", unsent: 2 }), 0, true);
  assert.equal(held.text, "Held");
  assert.equal(held.action, "send");
  assert.equal(held.actionEnabled, true);
  assert.equal(pushView(status({ state: "sent" }), 0, true).text, "Sent to agent");
  assert.equal(pushView(status({ state: "sent" }), 0, false).text, "");
  const idle = pushView(status(), 0, true);
  assert.equal(idle.text, "");
  assert.equal(idle.action, null);
});

test("pushView: manual mode always offers Send now, enabled only with unsent edits", () => {
  const none = pushView(status({ mode: "manual" }), 0, true);
  assert.equal(none.action, "send");
  assert.equal(none.actionEnabled, false);
  assert.equal(none.text, "");
  const some = pushView(status({ mode: "manual", unsent: 3 }), 0, true);
  assert.equal(some.actionEnabled, true);
  assert.equal(pushView(status({ mode: "manual", state: "waiting_agent", unsent: 3 }), 0, true).text,
    "Waits for agent");
  assert.equal(pushView(status({ mode: "manual", state: "sent" }), 0, true).text, "Sent to agent");
});

test("pushView: busy disables the action; an error replaces the text", () => {
  const v = pushView(status({ mode: "manual", unsent: 1 }), 0, true, true, "Could not send · boom");
  assert.equal(v.actionEnabled, false);
  assert.equal(v.busy, true);
  assert.equal(v.text, "Could not send · boom");
  assert.equal(pushView(status({ state: "countdown", dueInMs: 1 }), 1, true, true).actionEnabled, false);
});

test("PushChip counts down from due_in_ms at receipt, once per second", () => {
  const c = new Clock();
  c.chip.apply(status({ state: "countdown", dueInMs: 4500, unsent: 1 }));
  assert.equal(c.last?.text, "Sending to agent in 5s");
  c.advance(499);
  assert.equal(c.last?.text, "Sending to agent in 5s");
  c.advance(1);
  assert.equal(c.last?.text, "Sending to agent in 4s");
  c.advance(1000);
  assert.equal(c.last?.text, "Sending to agent in 3s");
  c.advance(3000);
  assert.equal(c.last?.text, "Sending to agent…");
  assert.equal(c.live(), 0, "the countdown stops at zero");
});

test("PushChip restarts the countdown from each new status", () => {
  const c = new Clock();
  c.chip.apply(status({ state: "countdown", dueInMs: 3000, unsent: 1 }));
  c.advance(2000);
  assert.equal(c.last?.text, "Sending to agent in 1s");
  c.chip.apply(status({ state: "countdown", dueInMs: 10_000, unsent: 2 }));
  assert.equal(c.last?.text, "Sending to agent in 10s");
  assert.equal(c.live(), 1, "the old tick is cleared");
});

test("PushChip shows Sent to agent briefly", () => {
  const c = new Clock();
  c.chip.apply(status({ state: "sent" }));
  assert.equal(c.last?.text, "Sent to agent");
  c.advance(SENT_VISIBLE_MS - 1);
  assert.equal(c.last?.text, "Sent to agent");
  c.advance(1);
  assert.equal(c.last?.text, "");
  assert.equal(c.live(), 0);
});

test("PushChip busy and error keep the countdown running; a new status clears the error", () => {
  const c = new Clock();
  c.chip.apply(status({ state: "countdown", dueInMs: 3000, unsent: 1 }));
  c.chip.setBusy(true);
  assert.equal(c.last?.actionEnabled, false);
  assert.equal(c.live(), 1);
  c.chip.setBusy(false);
  c.chip.setError("Could not hold · offline");
  assert.equal(c.last?.text, "Could not hold · offline");
  c.chip.apply(status({ state: "held", unsent: 1 }));
  assert.equal(c.last?.text, "Held");
});

test("PushChip renders null without a status and dispose stops its timer", () => {
  const c = new Clock();
  c.chip.apply(null);
  assert.equal(c.last, null);
  c.chip.apply(status({ state: "countdown", dueInMs: 5000 }));
  c.chip.dispose();
  assert.equal(c.live(), 0);
});

test("PushChip drops a reply older than a status that arrived while the request was out", () => {
  const c = new Clock();
  c.chip.apply(status({ state: "waiting_agent", unsent: 2, mode: "manual" }));
  const sentAt = c.chip.version; // Send now is clicked: the daemon queues delivery
  c.chip.apply(status({ state: "sent", unsent: 0, mode: "manual" })); // the agent became ready and got it
  assert.equal(c.chip.applyReply(status({ state: "waiting_agent", unsent: 2, mode: "manual" }), sentAt), false);
  assert.equal(c.chip.current?.state, "sent", "the newer status stands");
  const later = c.chip.version;
  assert.equal(c.chip.applyReply(status({ state: "sent", unsent: 0, mode: "manual" }), later), true, "a reply nothing overtook is applied");
});
