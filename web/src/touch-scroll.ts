// Phone touch-scroll physics (#5020): pure, clock-injected, unit-testable;
// finger px in, content px out, +=scroll up.
export const SCROLL_GAIN = 3, FLING_WINDOW_MS = 100, FLING_MIN_V = 0.5, FLING_MAX_V = 6,
  FLING_VEL_GAIN = 4.5, FLING_DECAY_MS = 650, FLING_STOP_V = 0.04, FLING_MAX_DT = 50;

export const TouchScroll = (now: () => number) => {
  let samples: { y: number; t: number }[] = [];
  let v = 0;
  let lastTick = 0;
  return {
    get active() {
      return samples.length !== 0;
    },
    push(y: number): number {
      const last = samples.at(-1);
      samples.push({ y, t: now() });
      return last ? (last.y - y) * SCROLL_GAIN : 0;
    },
    release(): number {
      const t = now();
      const s = samples.splice(0), last = s.at(-1);
      v = 0;
      if (!last) return 0;
      // Velocity comes from the trailing window measured against the LIFT time —
      // a pause before it dilutes w smoothly instead of clipping at the window.
      let i = s.length - 1;
      for (; i > 0 && s[i - 1].t >= t - FLING_WINDOW_MS; --i);
      const dt = t - s[i].t;
      const w = dt > 0 ? (s[i].y - last.y) / dt : 0;
      if (Math.abs(w) < FLING_MIN_V) return 0;
      v = Math.max(-FLING_MAX_V, Math.min(FLING_MAX_V, w)) * FLING_VEL_GAIN;
      lastTick = t;
      return v;
    },
    tick(): number | null {
      if (v === 0) return null;
      const t = now(), dt = t - lastTick;
      lastTick = t;
      // A stalled rAF must not land the whole gap in one jump; decay is real time.
      const px = v * Math.min(dt, FLING_MAX_DT);
      v *= Math.exp(-dt / FLING_DECAY_MS);
      if (Math.abs(v) < FLING_STOP_V) v = 0;
      return px;
    },
    stop(): void {
      v = 0;
      // An unclaimed gesture (a tap) never reaches release(); drop its samples.
      samples = [];
    },
  };
};
export type TouchScroll = ReturnType<typeof TouchScroll>;
