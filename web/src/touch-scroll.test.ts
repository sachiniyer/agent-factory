import assert from "node:assert/strict";
import test from "node:test";

import {
  FLING_DECAY_MS,
  FLING_MAX_V,
  FLING_MAX_DT,
  FLING_MIN_V,
  FLING_STOP_V,
  FLING_VEL_GAIN,
  SCROLL_GAIN,
  TouchScroll,
} from "./touch-scroll.js";

/** A controllable monotonic clock: every physics call reads `now` through it. */
function fakeClock(start = 0) {
  let t = start;
  return {
    now: () => t,
    set(v: number) {
      t = v;
    },
    advance(ms: number) {
      t += ms;
    },
  };
}
type Clock = ReturnType<typeof fakeClock>;

/** Replays recorded (y, t) move samples, returning the summed content px. */
function drag(s: TouchScroll, clock: Clock, samples: readonly { y: number; t: number }[]): number {
  let total = 0;
  for (const sample of samples) {
    clock.set(sample.t);
    total += s.push(sample.y);
  }
  return total;
}

/** Steps the coast at a fixed frame interval; returns every frame's output px. */
function coastFrames(s: TouchScroll, clock: Clock, frameMs = 16.7, maxFrames = 600): number[] {
  const frames: number[] = [];
  for (let i = 0; i < maxFrames; i++) {
    clock.advance(frameMs);
    const px = s.tick();
    if (px === null) {
      break;
    }
    frames.push(px);
  }
  return frames;
}

test("a drag moves content at SCROLL_GAIN times the finger's travel", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(500);
  // Finger UP the screen scrolls toward the newest output: positive, x GAIN.
  assert.equal(drag(s, clock, [{ y: 490, t: 16 }, { y: 470, t: 32 }]), 30 * SCROLL_GAIN);
  // …and finger DOWN pulls older output in: negative, same magnitude per px.
  assert.equal(drag(s, clock, [{ y: 480, t: 48 }]), -10 * SCROLL_GAIN);
});

test("a full-height swipe scrolls at least 3x the viewport (#5020 property 1)", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  const heightPx = 700;
  const rowHeight = 17;
  const viewportRows = heightPx / rowHeight;
  s.stop(); s.push(0);
  const steps: { y: number; t: number }[] = [];
  for (let i = 1; i <= 20; i++) {
    steps.push({ y: (heightPx * i) / 20, t: i * 60 }); // a slow full-height drag
  }
  const contentPx = drag(s, clock, steps);
  // Finger DOWN pulls older output in — negative by xterm's convention; the
  // magnitude is what the issue's 3x requirement measures.
  assert.equal(contentPx, -heightPx * SCROLL_GAIN);
  assert.ok(
    -contentPx / rowHeight >= 3 * viewportRows,
    `${-contentPx / rowHeight} rows must be >= 3x the ${viewportRows}-row viewport`,
  );
});

test("release velocity is read off only the trailing 100ms of the drag", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(400);
  // A long slow approach (400px over 400ms = 1 px/ms, itself fling-worthy) ends in
  // a fast tail: 100px inside the last 80ms. The window must see ~the tail's rate,
  // not the gesture's average — an early-motion average is exactly what the window
  // exists to discard.
  const samples = [{ y: 400, t: 0 }];
  for (let i = 1; i <= 10; i++) {
    samples.push({ y: 400 - i * 10, t: 20 + i * 8 });
  }
  drag(s, clock, samples);
  // Lift 10ms after the last move: the anchor lands on the sample preceding the
  // window (t=0), so the 100px of tail travel measures over 110ms — the lift-time
  // gap counts, and the boundary sample keeps the estimate off a cliff.
  clock.set(110);
  const v0 = s.release();
  const expected = (100 * FLING_VEL_GAIN) / 110; // 100px finger / 110ms, gained
  assert.ok(Math.abs(v0 - expected) < expected * 0.001, `release velocity ${v0} ≈ ${expected} px/ms content`);
});

test("a quick flick travels many screenfuls before it stops (#5020 property 2)", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(400);
  // ~2 px/ms finger over the last 100ms — an ordinary brisk flick.
  const samples: { y: number; t: number }[] = [];
  for (let i = 1; i <= 8; i++) {
    samples.push({ y: 400 - i * 20, t: i * 10 });
  }
  drag(s, clock, samples);
  clock.set(80);
  const v0 = s.release();
  assert.ok(Math.abs(v0 - 2 * FLING_VEL_GAIN) < 0.01);
  const frames = coastFrames(s, clock);
  const total = frames.reduce((a, b) => a + b, 0);
  // Exponential coast integrates to v0 * DECAY; the per-frame discrete sum
  // overshoots that integral by ~dt/(2·DECAY), and the tail stops early.
  const ideal = v0 * FLING_DECAY_MS;
  assert.ok(total > ideal * 0.95 && total < ideal * 1.03, `${total}px coasted vs ${ideal}px ideal`);
  const viewportPx = 700;
  assert.ok(total > 3 * viewportPx, `the flick must travel many screenfuls: ${total / viewportPx} viewports`);
});

test("the coast decays exponentially and halts under the stop threshold", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(300);
  const samples = [{ y: 300, t: 0 }];
  for (let i = 1; i <= 5; i++) {
    samples.push({ y: 300 - i * 12, t: i * 12 });
  }
  drag(s, clock, samples);
  clock.set(70);
  const v0 = s.release();
  assert.ok(v0 > 0);
  const dt = 16.7;
  const frames = coastFrames(s, clock, dt);
  assert.ok(frames.length > 10, "a flick coasts over many frames, not a single lurch");
  // Between equal-sized frames the velocity decays by exactly exp(-dt/DECAY);
  // frame output is v*dt, so the same ratio shows in the distances.
  const ratio = Math.exp(-dt / FLING_DECAY_MS);
  for (let i = 1; i < frames.length; i++) {
    assert.ok(Math.abs(frames[i] / frames[i - 1] - ratio) < 1e-9, `frame ${i} must decay by exp(-dt/τ)`);
  }
  assert.equal(s.tick(), null);
});

test("holding still before the lift bleeds the release velocity away", () => {
  // The same brisk tail, two endings: lifted mid-motion versus after a 60ms
  // stationary hold. The window includes the pause, so the held lift dilutes.
  const build = (gapMs: number) => {
    const clock = fakeClock();
    const s = TouchScroll(clock.now);
    s.stop(); s.push(400);
    drag(s, clock, [{ y: 300, t: 0 }, { y: 250, t: 20 }, { y: 200, t: 40 }]);
    clock.set(40 + gapMs);
    return s.release();
  };
  const hot = build(0), held = build(60);
  assert.ok(hot > 0 && held > 0 && held < hot, `a 60ms hold must dilute ${hot} below it, got ${held}`);
});

test("a drag that pauses before the lift leaves no momentum", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(600);
  drag(s, clock, [{ y: 500, t: 0 }, { y: 420, t: 16 }, { y: 350, t: 32 }]);
  // The finger rests 200ms — well past the velocity window — then lifts.
  clock.set(232);
  assert.equal(s.release(), 0);
  clock.advance(100);
  assert.equal(s.tick(), null);
});

test("a slow drag still creeping at lift stays under the fling floor", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(500);
  // 0.3 px/ms, uniform and unhurried — under FLING_MIN_V.
  const v = FLING_MIN_V * 0.6;
  const samples: { y: number; t: number }[] = [];
  for (let i = 1; i <= 10; i++) {
    samples.push({ y: 500 - i * v * 10, t: i * 10 });
  }
  drag(s, clock, samples);
  clock.set(100);
  assert.equal(s.release(), 0);
  assert.equal(s.tick(), null);
});

test("a tap's wobble cannot fling", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(300);
  drag(s, clock, [{ y: 302, t: 30 }, { y: 304, t: 60 }, { y: 306, t: 90 }]);
  clock.set(95);
  assert.equal(s.release(), 0);
});

test("a new touch interrupts the coast immediately (tap-to-stop)", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(400);
  drag(s, clock, [{ y: 340, t: 0 }, { y: 280, t: 20 }, { y: 220, t: 40 }]);
  clock.set(45);
  assert.notEqual(s.release(), 0);
  clock.advance(16.7);
  assert.ok(s.tick() !== null);
  s.stop();
  assert.equal(s.tick(), null);
  // …and the interrupted gesture restarts cleanly from the new finger-down.
  clock.advance(200);
  s.stop(); s.push(350);
  clock.advance(16);
  assert.equal(drag(s, clock, [{ y: 340, t: clock.now() }]), (350 - 340) * SCROLL_GAIN);
});

test("a wild sample stream is clamped to the fling ceiling", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(5000);
  // 30 px/ms — far past anything a finger does; the ceiling must hold it.
  drag(s, clock, [{ y: 2000, t: 0 }, { y: 500, t: 50 }]);
  clock.set(55);
  const v0 = s.release();
  assert.equal(v0, FLING_MAX_V * FLING_VEL_GAIN);
  clock.advance(10);
  const px = s.tick();
  assert.ok(px !== null && px <= v0 * 10 * 1.001);
});

test("both fling directions keep their sign", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(100);
  drag(s, clock, [{ y: 200, t: 0 }, { y: 300, t: 40 }]);
  clock.set(45);
  const v0 = s.release();
  assert.ok(v0 < 0, "a downward finger coasts back into history (negative)");
  clock.advance(16.7);
  assert.ok((s.tick() ?? 0) < 0);
});

test("coast distance scales linearly with release velocity, clamped at the ceiling", () => {
  // The review's spread, in release px/ms (CSS): gentle ~1 => a few viewports,
  // hard >=3.4 => >=10 viewports, 10 clamps to the 6 px/ms ceiling, and a
  // paused lift (no recent samples => release() = 0) is covered separately.
  const distances = new Map<number, number>();
  for (const v of [0.5, 1, 2, 4, 6, 10]) {
    const clock = fakeClock();
    const s = TouchScroll(clock.now);
    s.stop(); s.push(5000);
    const samples: { y: number; t: number }[] = [];
    for (let i = 1; i <= 20; i++) {
      samples.push({ y: 5000 - i * v * 10, t: i * 10 });
    }
    drag(s, clock, samples);
    clock.set(200);
    s.release();
    const total = coastFrames(s, clock).reduce((a, b) => a + b, 0);
    distances.set(v, total);
    const ideal = Math.min(v, FLING_MAX_V) * FLING_VEL_GAIN * FLING_DECAY_MS;
    assert.ok(Math.abs(total - ideal) < ideal * 0.08, `v=${v} px/ms must coast ~v·G·τ = ${ideal}px, got ${total}px`);
  }
  const at = (v: number): number => distances.get(v) ?? 0;
  for (const [slow, fast] of [[0.5, 1], [1, 2], [2, 4], [4, 6]] as const) {
    assert.ok(at(fast) > at(slow), `a faster release must coast farther: ${fast} vs ${slow} px/ms`);
  }
  assert.ok(Math.abs(at(10) - at(6)) < at(6) * 0.01, "past the ceiling, the same coast — 10 clamps to 6");
});

test("a stalled frame emits a bounded slice, while the fling ages by real time", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(400);
  drag(s, clock, [{ y: 340, t: 0 }, { y: 280, t: 20 }, { y: 220, t: 40 }]);
  clock.set(45);
  const v0 = s.release();
  assert.notEqual(v0, 0);
  // The tab slept 5s mid-coast: the resumed frame must not land the whole gap.
  clock.advance(5000);
  const px = s.tick();
  assert.ok(px !== null, "v>0 still has motion to emit");
  assert.ok(Math.abs(px) <= Math.abs(v0) * FLING_MAX_DT * 1.001,
    `one stalled frame emitted ${px}px — must cap at ~${FLING_MAX_DT}ms of travel`);
  // …but the coast aged by the real 5s — long past the decay — so it ends now.
  assert.equal(s.tick(), null);
});

test("stop() forgets the old stream, so a tap cannot seed the next gesture", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  s.stop(); s.push(300);
  drag(s, clock, [{ y: 302, t: 10 }, { y: 306, t: 20 }]);
  // The tap ends unclaimed — release() never runs — and a new finger lands.
  s.stop();
  clock.advance(40);
  assert.equal(s.push(700), 0, "the first sample of a new gesture must not diff against the tap's");
});

test("a coast with no drag samples and a tick with no coast are inert", () => {
  const clock = fakeClock();
  const s = TouchScroll(clock.now);
  assert.equal(s.tick(), null);
  assert.equal(s.release(), 0);
  s.stop(); s.push(400);
  clock.advance(20);
  assert.equal(s.release(), 0, "one sample has no velocity to measure");
});
