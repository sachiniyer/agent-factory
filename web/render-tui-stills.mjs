// Run by the container recorder after the TUI model captures have been copied in.
import { chromium } from 'playwright';
import { mkdir } from 'node:fs/promises';
const scenes = ['sessions-dense', 'single-project', 'keyboard', 'pane', 'prompt', 'tasks', 'appearance', 'project-picker', 'zero-sessions', 'no-daemon', 'help'];
const out = process.env.AF_TUI_STILLS_OUT || '/work/tui-out';
await mkdir(out, { recursive: true });
const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
try {
  const page = await browser.newPage({ viewport: { width: 1200, height: 800 }, deviceScaleFactor: 1 });
  for (const scene of scenes) for (const mode of ['light', 'dark']) {
    await page.goto(`file:///src/docs/assets/design/tui-c/${scene}-${mode}.svg`);
    await page.locator('svg').screenshot({ path: `${out}/${scene}-${mode}.png`, animations: 'disabled' });
  }
} finally { await browser.close(); }
