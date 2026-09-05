/**
 * The recorded viewport for the web demo (#3855), shared by the recorder's
 * config and its spec.
 *
 * It lives in its own module rather than in playwright.demo.config.ts because
 * the spec needs it too, and importing the config from a spec would re-run the
 * config's "AF_WEB_BASE_URL is unset" guard from inside a test file — turning a
 * missing-harness mistake into an error raised at the wrong layer.
 *
 * 1440x900 is a laptop, which is what the web client is for, and it is 16:10 —
 * the aspect the converted video and the poster keep.
 */
export const DEMO_VIEWPORT = {
  width: Number(process.env.AF_DEMO_WIDTH ?? 1440),
  height: Number(process.env.AF_DEMO_HEIGHT ?? 900),
};
