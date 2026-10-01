// Phone touch-scroll physics (#5020): pure, clock-injected, unit-testable;
// finger px in, content px out, +=scroll up.
export const SCROLL_GAIN = 3;
export const FLING_WINDOW_MS = 100;
export const FLING_MIN_V = 0.5;
export const FLING_MAX_V = 6;
export const FLING_VEL_GAIN = 4.5;
export const FLING_DECAY_MS = 650;
export const FLING_STOP_V = 0.04;
export const FLING_MAX_DT = 50;

export class TouchScroll {
  private samples: { y: number; t: number }[] = [];
  private v = 0;
  private lastTick = 0;
  constructor(private readonly now: () => number) {}

  push(y: number): number {
    const last = this.samples[this.samples.length - 1];
    this.samples.push({ y, t: this.now() });
    return last ? (last.y - y) * SCROLL_GAIN : 0;
  }

  release(): number {
    const t = this.now();
    const s = this.samples;
    this.samples = [];
    this.v = 0;
    const last = s[s.length - 1];
    if (!last) {
      return 0;
    }
    let i = s.length - 1;
    for (; i > 0 && s[i - 1].t >= t - FLING_WINDOW_MS; --i);
    const dt = last.t - s[i].t;
    const w = dt > 0 ? (s[i].y - last.y) / dt : 0;
    if (Math.abs(w) < FLING_MIN_V) {
      return 0;
    }
    this.v = Math.max(-FLING_MAX_V, Math.min(FLING_MAX_V, w)) * FLING_VEL_GAIN;
    this.lastTick = t;
    return this.v;
  }

  tick(): number | null {
    if (this.v === 0) {
      return null;
    }
    const t = this.now();
    const dt = t - this.lastTick;
    this.lastTick = t;
    // A stalled rAF must not land the whole gap in one jump; decay is real time.
    const px = this.v * Math.min(dt, FLING_MAX_DT);
    this.v *= Math.exp(-dt / FLING_DECAY_MS);
    if (Math.abs(this.v) < FLING_STOP_V) {
      this.v = 0;
    }
    return px;
  }

  stop(): void {
    this.v = 0;
    // An unclaimed gesture (a tap) never reaches release(); drop its samples.
    this.samples = [];
  }
}
