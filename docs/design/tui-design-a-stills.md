# TUI design system: slice A

Slice A applies the fixed generated light/dark roles throughout the TUI and
removes the private Config/account ANSI palette and raw overlay backdrop colours.
`theme.Roles()` and `theme.Styles()` expose typed fields. The source guard in
`ui/theme/literals_test.go` rejects production colour constructors, adaptive
palette definitions, hex strings and raw extended SGR colour values under `ui/`
and `app/`; PTY parser test inputs and generated SVG fixtures are not chrome.
The all-role SGR test checks foreground and background in both themes within
one channel step of the tokens, documenting termenv's float truncation tracked
in [termenv #217](https://github.com/muesli/termenv/issues/217); the web remains exact.
Legacy palette input no longer changes TUI colours. Slice B owns the shared
overlay recipe and density/copy cuts; slice C owns Light/Dark/System selection
and migration. To meet the stricter 480 ms frame requirement, the sidebar
measures rows through the viewport after its selection/scroll anchor and defers
styling the unreachable suffix. Existing scrolling and cell geometry stay the
same; the container model goldens pass without recapture for this optimization.
Shared daemon config retirement is tracked in
[#3936](https://github.com/sachiniyer/agent-factory/issues/3936), with the
[boundary contract](tui-theme-daemon-follow-up.md).

## Capture provenance

These 64 stills are deterministic real app-model `Update`/`View` output,
converted from ANSI cells by P4's SVG writer. They are not live-daemon screen
recordings. `TestDesignDriverScenes` adds nineteen chrome scenes to P4's thirteen
recovery scenes. Both run only in the isolated sandbox created by
`make playtest-container-detached`; no host TUI, app tests or daemon tests run.
The supplementary P4 live tmux scenario passed retained failed creation,
backend recovery, first attached stand-in terminal and the tiny-terminal state.
Its [captured text](../assets/design/tui-a/live/first-session.txt) does not claim
real agent authentication or agent responses.

Reproduce with the existing make harness and the unique sandbox name it prints:

```sh
make playtest-container-detached
# Inside that sandbox, with /src as cwd:
AF_TUI_DESIGN_CAPTURE=/home/dev/sandbox/p5-design \
AF_TUI_RECOVERY_CAPTURE=/home/dev/sandbox/p5-recovery \
/usr/local/go/bin/go test ./app -run 'Test(Recovery|DesignDriverScenes)' -count=1
bash /src/scripts/tui-3915-scenario.sh
```

Copy the capture directories out of that sandbox, inspect the SVGs in both
palettes, and update the matching `app/testdata/design` / `app/testdata/recovery`
goldens. Without capture environment variables, tests compare the actual output
to those goldens. Keep captured ANSI beside the gallery for source inspection.

## Codex review corrections

Search results now use Lost, Dead and Archived roles; Config key and account
selections use Ink on SurfaceRaised. The blurred sidebar title uses Surface on
InkMuted, with a 4.5:1 contrast gate in both palettes. Regression tests pin these
role assignments. Only the affected accounts, config, zero-accounts, search,
pane, keyboard and preview SVG/ANSI pairs were refreshed in the make-created
playtest sandbox; all 64 driver goldens pass. The search fixture now includes
lost, dead and archived results.

The final whole-PR [role audit](../assets/design/tui-a/role-audit.txt) records
every changed file, role pair and staged B/C boundary. It additionally fixes
blank working/in-flight search cells, archive-failure Dead text, muted Config
location, retained selection ink and token-only placeholder contrast.

## Performance and verification

One `make perf-container` pass at 1,000 sessions verified the Codex corrections
on top of the master merge through `d7705ac0` (#3929 and #3928). Full frame
averaged **65.880 ms** (range 58.376–70.261); key-to-render averaged **84.821 ms**
(70.399–92.373).
Every sample meets P5's stricter 480 / 351 ms limits. The harness still reports
P1's original looser CI ceilings; these are not the thresholds used to accept P5.
[Raw samples](../assets/design/tui-a/perf/tui-runs.json) and the
[complete report](../assets/design/tui-a/perf/metrics.txt) preserve the evidence.
Before deferring invisible rows, this slice averaged 498.625 / 247.484 ms;
[those samples](../assets/design/tui-a/perf/before-viewport.json) motivated the
viewport change. No baseline or budget was relaxed.

All required formatting, build, vet, fast lint, file-length and theme drift
checks passed, as did changed UI/doctor/design-generator package tests. The full
app package passed inside the make-created playtest sandbox. Full diff and all
stills were self-reviewed before requesting review.

## Stills

| Scene | Light | Dark |
| --- | --- | --- |
| archive warning | ![archive warning, light](../assets/design/tui-a/archive-warning-light.svg) | ![archive warning, dark](../assets/design/tui-a/archive-warning-dark.svg) |
| accounts | ![accounts, light](../assets/design/tui-a/accounts-light.svg) | ![accounts, dark](../assets/design/tui-a/accounts-dark.svg) |
| archive failed | ![archive-failed, light](../assets/design/tui-a/archive-failed-light.svg) | ![archive-failed, dark](../assets/design/tui-a/archive-failed-dark.svg) |
| config | ![config, light](../assets/design/tui-a/config-light.svg) | ![config, dark](../assets/design/tui-a/config-dark.svg) |
| confirmation | ![confirmation, light](../assets/design/tui-a/confirmation-light.svg) | ![confirmation, dark](../assets/design/tui-a/confirmation-dark.svg) |
| create failed | ![create-failed, light](../assets/design/tui-a/create-failed-light.svg) | ![create-failed, dark](../assets/design/tui-a/create-failed-dark.svg) |
| help | ![help, light](../assets/design/tui-a/help-light.svg) | ![help, dark](../assets/design/tui-a/help-dark.svg) |
| hooks | ![hooks, light](../assets/design/tui-a/hooks-light.svg) | ![hooks, dark](../assets/design/tui-a/hooks-dark.svg) |
| keyboard | ![keyboard, light](../assets/design/tui-a/keyboard-light.svg) | ![keyboard, dark](../assets/design/tui-a/keyboard-dark.svg) |
| kill failed | ![kill-failed, light](../assets/design/tui-a/kill-failed-light.svg) | ![kill-failed, dark](../assets/design/tui-a/kill-failed-dark.svg) |
| no daemon | ![no-daemon, light](../assets/design/tui-a/no-daemon-light.svg) | ![no-daemon, dark](../assets/design/tui-a/no-daemon-dark.svg) |
| no project | ![no-project, light](../assets/design/tui-a/no-project-light.svg) | ![no-project, dark](../assets/design/tui-a/no-project-dark.svg) |
| pane | ![pane, light](../assets/design/tui-a/pane-light.svg) | ![pane, dark](../assets/design/tui-a/pane-dark.svg) |
| preview | ![preview, light](../assets/design/tui-a/preview-light.svg) | ![preview, dark](../assets/design/tui-a/preview-dark.svg) |
| project picker | ![project-picker, light](../assets/design/tui-a/project-picker-light.svg) | ![project-picker, dark](../assets/design/tui-a/project-picker-dark.svg) |
| projects unavailable | ![projects-unavailable, light](../assets/design/tui-a/projects-unavailable-light.svg) | ![projects-unavailable, dark](../assets/design/tui-a/projects-unavailable-dark.svg) |
| prompt | ![prompt, light](../assets/design/tui-a/prompt-light.svg) | ![prompt, dark](../assets/design/tui-a/prompt-dark.svg) |
| search | ![search, light](../assets/design/tui-a/search-light.svg) | ![search, dark](../assets/design/tui-a/search-dark.svg) |
| selection | ![selection, light](../assets/design/tui-a/selection-light.svg) | ![selection, dark](../assets/design/tui-a/selection-dark.svg) |
| sessions | ![sessions, light](../assets/design/tui-a/sessions-light.svg) | ![sessions, dark](../assets/design/tui-a/sessions-dark.svg) |
| task create | ![task-create, light](../assets/design/tui-a/task-create-light.svg) | ![task-create, dark](../assets/design/tui-a/task-create-dark.svg) |
| task edit save failed | ![task-edit-save-failed, light](../assets/design/tui-a/task-edit-save-failed-light.svg) | ![task-edit-save-failed, dark](../assets/design/tui-a/task-edit-save-failed-dark.svg) |
| task save failed | ![task-save-failed, light](../assets/design/tui-a/task-save-failed-light.svg) | ![task-save-failed, dark](../assets/design/tui-a/task-save-failed-dark.svg) |
| tasks | ![tasks, light](../assets/design/tui-a/tasks-light.svg) | ![tasks, dark](../assets/design/tui-a/tasks-dark.svg) |
| tasks unavailable | ![tasks-unavailable, light](../assets/design/tui-a/tasks-unavailable-light.svg) | ![tasks-unavailable, dark](../assets/design/tui-a/tasks-unavailable-dark.svg) |
| too small | ![too-small, light](../assets/design/tui-a/too-small-light.svg) | ![too-small, dark](../assets/design/tui-a/too-small-dark.svg) |
| zero accounts | ![zero-accounts, light](../assets/design/tui-a/zero-accounts-light.svg) | ![zero-accounts, dark](../assets/design/tui-a/zero-accounts-dark.svg) |
| zero sessions | ![zero-sessions, light](../assets/design/tui-a/zero-sessions-light.svg) | ![zero-sessions, dark](../assets/design/tui-a/zero-sessions-dark.svg) |
| zero tasks | ![zero-tasks, light](../assets/design/tui-a/zero-tasks-light.svg) | ![zero-tasks, dark](../assets/design/tui-a/zero-tasks-dark.svg) |
| Alarm | ![Alarm, light](../assets/design/tui-a/alarm-light.svg) | ![Alarm, dark](../assets/design/tui-a/alarm-dark.svg) |
| task schedule | ![task schedule light](../assets/design/tui-a/task-schedule-light.svg) | ![task schedule dark](../assets/design/tui-a/task-schedule-dark.svg) |
| task weekdays | ![task weekdays light](../assets/design/tui-a/task-weekdays-light.svg) | ![task weekdays dark](../assets/design/tui-a/task-weekdays-dark.svg) |
