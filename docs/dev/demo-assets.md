# Demo assets

For maintainers who need to refresh the demo media the README and the docs home
page lead with. After reading this you will know what lives in
`docs/assets/web/`, how to regenerate all of it with one command, and what the
recording is and is not allowed to claim.

Read [Container testing](container-testing.md) first for the isolation boundary,
then the [web guide](../web.md) for the screens the recorder captures. The
[web client selftest](web-selftest.md) explains the shared harness.

## What ships

Everything under `docs/assets/web/` is generated. Nothing there is
hand-captured, cropped, or retouched:

| File | What it is |
| --- | --- |
| `demo.mp4` | The hero video · h264, ≤ 8 MB, ≤ 60 s |
| `demo.webm` | The same recording, VP9 · what the docs site plays |
| `demo.gif` | The fallback for renderers that will not play a video · ≤ 4 MB |
| `demo-poster.png` | The frame shown before the video plays · the dashboard still |
| `dashboard.png` · `new-session.png` · `agent-tab.png` · `review.png` · `tasks.png` · `config-accounts.png` | One still per beat, default theme |
| `parallel-work.png` · `comparison-review.png` · `scheduled-triage.png` · `event-intake.png` | Use-case and comparison stills; task forms are filled but not submitted |
| `session-filter.png` · `project-menu.png` · `phone-session.png` · `phone-drawer.png` · `phone-controls.png` · `phone-project-menu.png` · `phone-filter.png` | Rail/header disclosure and phone evidence, recorded in a separate context after the hero pass |
| `session-actions.png` · `tab-types.png` · `terminal-keyboard.png` · the same three with `phone-` · `split-panes.png` | Terminal actions, tab creation, keyboard ownership and split chrome |
| `create-compact.png` · `create-defaults.png` · `session-lifecycle.png` · `kill-confirmation.png` · `task-actions.png` · `remove-task.png` · `edit-task.png` · `config-dirty.png` · `add-account.png` · `account-error.png` · `assistant-error.png` · `add-project.png` · `login.png` · `unavailable.png` | Form disclosures, confirmations and recovery; failure and sign-in responses are controlled recorder fixtures |
| `phone-create.png` · `phone-tasks.png` · `phone-config.png` · `phone-add-account.png` | Phone forms and management views |
| the same forty-two, `-dark` | One still per scene, dark theme |

`docs/assets/tui/` holds the TUI's own media, produced by a different recorder
(`scripts/container/record-demo.sh`, which drives real Codex sessions through
the TUI and needs a credential file). The two never share a path.

## Regenerating

```bash
make demo-assets
```

This takes a few minutes, writes into
`docs/assets/web/`, and leaves a `git status` to review and commit.

It runs entirely inside a container — the same Go + Node + Chromium image the
[web client self-test](web-selftest.md) uses, plus `ffmpeg`. Nothing touches
your tmux server, your `~/.agent-factory`, or any real repository, and no media
toolchain has to be installed on the host.

Useful overrides:

| Variable | Default | Effect |
| --- | --- | --- |
| `AF_DEMO_TARGET_SECONDS` | `32` | How long the delivered video should run · the speed-up is derived from the measured recording |
| `AF_DEMO_MAX_MP4_BYTES` | `8000000` | Hard cap · the run fails rather than committing a larger file |
| `AF_DEMO_MAX_GIF_BYTES` | `4000000` | Hard cap · the GIF steps down through width, frame rate and palette until it fits |
| `AF_DEMO_WIDTH` · `AF_DEMO_HEIGHT` | `1440` · `900` | The recorded viewport |
| `AF_DEMO_PANE_COLS` · `AF_DEMO_PANE_ROWS` | `168` · `44` | The geometry the seeded terminal panes are given before recording |

## How it works

`make demo-assets` → `scripts/testbox.sh web-demo` → one ephemeral container
running `scripts/container/web-demo-entry.sh`, which:

1. builds `af` from the mounted source;
2. creates a mock project — `todo-cli`, a real git repo with a real program and
   a real `test.sh`;
3. starts a **real** `af` daemon on a throwaway AF home with a loopback HTTP
   listener, exactly as the self-test's sandbox does. The browser is a loopback
   peer, so it connects with no token;
4. seeds three sessions, a review tab, two scheduled tasks, and two credential
   accounts;
5. sizes the seeded panes. A tmux pane opens at 80×24 when nothing is attached,
   and these were created minutes before a browser existed — so without this the
   frame shows an 80-column transcript sitting inside a much wider pane, an
   artifact of *when* the fixture ran. Both stand-ins repaint on `SIGWINCH`, so
   the resize reflows their output rather than widening the window under it;
6. runs `web/selftest/web-demo.spec.ts` under `web/playwright.demo.config.ts`,
   which drives the real web client through six beats — dashboard, the
   new-session modal, the agent tab streaming, the branch's diff in a process
   tab, the Tasks view, the Config view at its Accounts section — twice, once
   per theme, recording video and stills, including parallel work, comparison
   review, and unsubmitted cron/watch task forms for the use-case pages;
7. converts the recording with `ffmpeg` and copies the result out, but only
   after every size and duration budget has passed.

The recorder and the self-test share the harness on purpose: what the docs show
is the product the gate asserts on. They do **not** share a fixture set — the
gate seeds a dead port, a URL-less tab and a session called `probe-noserver`,
all of which exist to make failures reachable and none of which belong on a
README.

## What is real, and what is a stand-in

The recording is honest about being a recording, and the distinction matters
enough to state:

**Real.** The daemon, the sessions, the git worktrees and branches, the tabs,
the terminal streaming over the PTY WebSocket, the scheduled tasks, the config
manifest, the accounts registry, and every file edit and diff you see. The web
client in the video is the client this repository builds.

**Stand-in.** Two things, each because a reproducible recording cannot have
the real one:

- **The agent** (`scripts/container/web-demo-agent.sh`). It names itself in its
  first pane line. Shelling out to Claude Code or Codex would need credentials
  in the sandbox, a network, and output that changes every run — so the video
  would be neither reproducible nor reviewable. The stand-in still does real
  work in the session's own worktree: it edits the files, runs the project's
  `./test.sh`, and leaves the changes behind, which is why the diff in the
  recording is a diff it actually made.
- **The clock**, in one direction only. The scheduled tasks are seeded with a
  next occurrence half a day out, computed rather than fixed, so no task fires
  during the recording. Absolute times the Tasks view renders therefore differ
  between regenerations; everything else — the project, the session names, the
  beats, the transcript, the diff — is identical run to run.

## Not a gate

CI runs the same beats in still-only mode for [visual regression](perf-baselines.md),
without video or pacing. CI never runs the video recorder. It asserts nothing about correctness, and the
`expect` calls in its spec are waits, not assertions — they are how the recorder
knows a beat has landed before it takes the picture.

Two things keep it out of the gate, so neither entry point can pick up the
other's tests:

- it is reached only through `--config=playwright.demo.config.ts`, while
  `scripts/container/web-selftest-entry.sh` runs a bare `npx playwright test`;
- `web/playwright.config.ts` additionally ignores `web-demo.spec.ts` by name.

## When to re-run it

Whenever the web client's chrome changes in a way the demo shows — the rail, the
pane header, the new-session modal, the view tabs, the Tasks list, or the Config
view's Accounts section. The media is the product's face; a screenshot nobody
re-took is worse than no screenshot.

## Public TUI previews

After refreshing the TUI model SVG/ANSI pairs with `AF_TUI_DESIGN_CAPTURE` and
`AF_TUI_RECOVERY_CAPTURE` in the playtest sandbox, `make demo-assets` also renders
22 PNG previews into `docs/assets/tui/`. Use
`AF_TUI_STILLS_ONLY=1 make demo-assets` to render just these previews. The
container installs the SVG writer’s DejaVu Sans Mono font before rendering.
The browser renders the final P5 SVGs
without cropping their content or changing their colours. The style guide links
these smaller previews to the full SVGs; the TUI page uses the pane and Tasks
previews. Run the model capture first so these stay in sync with its goldens.

The real-agent TUI video recorder sets true-colour terminal output and uses the
fixed dark surface for the renderer background. This prevents the recording
terminal's default palette from replacing the P5 colours.

The separate `session-link.png` comes from the `copy-link.png` attachment of
`AF_PLAYWRIGHT_ARGS=session-route.spec.ts make web-selftest-container`. Copy that
attachment from the run’s printed artifact directory when refreshing all public
media. Recovery images likewise come from `recovery.spec.ts` attachments.

The recorder losslessly re-encodes PNGs with ffmpeg, keeping the smaller encoding,
and enforces the twelve tour stills’ 906,092-byte aggregate from #3884. Re-encoding
does not change dimensions or decoded pixels.
