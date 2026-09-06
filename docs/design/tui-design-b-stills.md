# TUI design system: slice B

The TUI uses one rounded, raised dialog frame with two horizontal cells of
inset. Pickers, prompts, confirmations, help, and the Config/Accounts, hooks and
task forms share it. Titles and actionable hints use ink. Nested text resets
restore the dialog background so styled labels do not leave stripes. This
applies only to AF-owned dialog content, before framing; agent terminals and
RGB values are not rewritten. Assistant and account-login sessions retain their
existing full-terminal tmux handoff, with no extra AF overlay around the child.

Confirmations keep a border-coloured frame, dead consequence text, and one
accent confirm action. Task deletion names its target and returns to the task
list on cancel or confirm; a refresh cannot change the confirmed task ID.

Collapsed, unselected sessions use one row; selected/expanded groups disclose
branch and churn detail, while failed restores/archives remain visible.
The rail omits an empty branch line, and Agent no longer shares the limit-state
diamond. The tab shown by the pane is marked “open” rather than an unexplained
asterisk. Pane titles read Session · Tab · Preview or Keyboard; preview origin
is available in help. Rail section headings use ink, with underline for focus and no decorative chip.
Terminal frames share square geometry; dialogs alone use rounded corners.
The footer uses one middle-dot separator and readable ink, retaining its
context-sensitive choices, width-based shedding, exit route, and complete help.

Empty task sections already reserved no rows. Zero or one project now reserves
no rail block either: the header retains project context, the picker remains
available, and its Delete action retains the existing identity-aware
confirmation. Two or more projects remain in the rail. Tasks show names and
next occurrence first, expand selected metadata beneath, separate groups with
one blank row, and keep the selected group in view. Edit is visible; `? actions`
discloses run/toggle/delete, with no run action offered for a watch.

Light/Dark/System selection and migration remain slice C. No daemon theme
operation or token value changes in this slice. The literal guard and all-role
foreground/background SGR tolerance tests remain in force.

## Capture provenance

These 112 light/dark stills are actual app-model Update/View output from the
existing P4 driver fixtures, serialized by its ANSI-to-SVG writer. Each SVG has
its source `.ansi` alongside it. Eight added scene pairs cover account selection, task actions and deletion,
one/multiple/degraded projects, dense sessions, and preview-origin help.
All app execution runs inside a sandbox
created by `make playtest-container-detached`; no host TUI or app/daemon tests.
The A/P4 live scenario remains supplementary evidence; these new captures are
deterministic app-model frames, not authenticated-agent recordings.

Reproduce inside the make-created sandbox with `/src` as cwd:

```sh
AF_TUI_DESIGN_CAPTURE=/home/dev/sandbox/p5-b-design \
AF_TUI_RECOVERY_CAPTURE=/home/dev/sandbox/p5-b-recovery \
/usr/local/go/bin/go test ./app -run 'Test(Recovery|DesignDriverScenes)' -count=1
```

Without capture variables the tests compare with `app/testdata/design` and
`app/testdata/recovery`. The A gallery retains its previously approved stills.

## Verification

The formatting, build, vet, fast lint, file-length, literal guard, SGR tolerance
and generated-theme drift checks pass. The UI and design-token packages pass,
as do the container driver goldens. All 112 driver goldens and the changed app tests pass inside the sandbox.
[State mapping enumeration](../assets/design/tui-b/state-mappings.txt),
[source branch inventory](../assets/design/tui-b/state-branches.txt), and
[role/contrast audit](../assets/design/tui-b/role-audit.txt) cover the complete diff.

`make perf-container` passed at 1,000 sessions: full frame mean **66.873 ms**,
max **68.649 ms**; key-to-render mean **68.232 ms**, max **80.148 ms**.
Every sample meets the stricter P5 480 / 351 ms limits; no budget was relaxed.
[Raw samples](../assets/design/tui-b/perf/tui-runs.json) and the
[complete report](../assets/design/tui-b/perf/metrics.txt) preserve the evidence.
Strict MkDocs builds include this gallery. SVG and ANSI pairs are self-reviewed
before the draft is submitted for Captain's design review.

## Stills

| Scene | Light | Dark |
| --- | --- | --- |
| account picker | ![account-picker light](../assets/design/tui-b/account-picker-light.svg) [ANSI](../assets/design/tui-b/account-picker-light.ansi) | ![account-picker dark](../assets/design/tui-b/account-picker-dark.svg) [ANSI](../assets/design/tui-b/account-picker-dark.ansi) |
| account register | ![account-register light](../assets/design/tui-b/account-register-light.svg) [ANSI](../assets/design/tui-b/account-register-light.ansi) | ![account-register dark](../assets/design/tui-b/account-register-dark.svg) [ANSI](../assets/design/tui-b/account-register-dark.ansi) |
| accounts | ![accounts light](../assets/design/tui-b/accounts-light.svg) [ANSI](../assets/design/tui-b/accounts-light.ansi) | ![accounts dark](../assets/design/tui-b/accounts-dark.svg) [ANSI](../assets/design/tui-b/accounts-dark.ansi) |
| alarm | ![alarm light](../assets/design/tui-b/alarm-light.svg) [ANSI](../assets/design/tui-b/alarm-light.ansi) | ![alarm dark](../assets/design/tui-b/alarm-dark.svg) [ANSI](../assets/design/tui-b/alarm-dark.ansi) |
| archive failed | ![archive-failed light](../assets/design/tui-b/archive-failed-light.svg) [ANSI](../assets/design/tui-b/archive-failed-light.ansi) | ![archive-failed dark](../assets/design/tui-b/archive-failed-dark.svg) [ANSI](../assets/design/tui-b/archive-failed-dark.ansi) |
| archive warning | ![archive-warning light](../assets/design/tui-b/archive-warning-light.svg) [ANSI](../assets/design/tui-b/archive-warning-light.ansi) | ![archive-warning dark](../assets/design/tui-b/archive-warning-dark.svg) [ANSI](../assets/design/tui-b/archive-warning-dark.ansi) |
| config edit | ![config-edit light](../assets/design/tui-b/config-edit-light.svg) [ANSI](../assets/design/tui-b/config-edit-light.ansi) | ![config-edit dark](../assets/design/tui-b/config-edit-dark.svg) [ANSI](../assets/design/tui-b/config-edit-dark.ansi) |
| config | ![config light](../assets/design/tui-b/config-light.svg) [ANSI](../assets/design/tui-b/config-light.ansi) | ![config dark](../assets/design/tui-b/config-dark.svg) [ANSI](../assets/design/tui-b/config-dark.ansi) |
| confirmation | ![confirmation light](../assets/design/tui-b/confirmation-light.svg) [ANSI](../assets/design/tui-b/confirmation-light.ansi) | ![confirmation dark](../assets/design/tui-b/confirmation-dark.svg) [ANSI](../assets/design/tui-b/confirmation-dark.ansi) |
| create failed | ![create-failed light](../assets/design/tui-b/create-failed-light.svg) [ANSI](../assets/design/tui-b/create-failed-light.ansi) | ![create-failed dark](../assets/design/tui-b/create-failed-dark.svg) [ANSI](../assets/design/tui-b/create-failed-dark.ansi) |
| failure notice | ![failure-notice light](../assets/design/tui-b/failure-notice-light.svg) [ANSI](../assets/design/tui-b/failure-notice-light.ansi) | ![failure-notice dark](../assets/design/tui-b/failure-notice-dark.svg) [ANSI](../assets/design/tui-b/failure-notice-dark.ansi) |
| help | ![help light](../assets/design/tui-b/help-light.svg) [ANSI](../assets/design/tui-b/help-light.ansi) | ![help dark](../assets/design/tui-b/help-dark.svg) [ANSI](../assets/design/tui-b/help-dark.ansi) |
| hooks add | ![hooks-add light](../assets/design/tui-b/hooks-add-light.svg) [ANSI](../assets/design/tui-b/hooks-add-light.ansi) | ![hooks-add dark](../assets/design/tui-b/hooks-add-dark.svg) [ANSI](../assets/design/tui-b/hooks-add-dark.ansi) |
| hooks edit | ![hooks-edit light](../assets/design/tui-b/hooks-edit-light.svg) [ANSI](../assets/design/tui-b/hooks-edit-light.ansi) | ![hooks-edit dark](../assets/design/tui-b/hooks-edit-dark.svg) [ANSI](../assets/design/tui-b/hooks-edit-dark.ansi) |
| hooks | ![hooks light](../assets/design/tui-b/hooks-light.svg) [ANSI](../assets/design/tui-b/hooks-light.ansi) | ![hooks dark](../assets/design/tui-b/hooks-dark.svg) [ANSI](../assets/design/tui-b/hooks-dark.ansi) |
| keyboard | ![keyboard light](../assets/design/tui-b/keyboard-light.svg) [ANSI](../assets/design/tui-b/keyboard-light.ansi) | ![keyboard dark](../assets/design/tui-b/keyboard-dark.svg) [ANSI](../assets/design/tui-b/keyboard-dark.ansi) |
| kill failed | ![kill-failed light](../assets/design/tui-b/kill-failed-light.svg) [ANSI](../assets/design/tui-b/kill-failed-light.ansi) | ![kill-failed dark](../assets/design/tui-b/kill-failed-dark.svg) [ANSI](../assets/design/tui-b/kill-failed-dark.ansi) |
| multiple projects | ![multiple-projects light](../assets/design/tui-b/multiple-projects-light.svg) [ANSI](../assets/design/tui-b/multiple-projects-light.ansi) | ![multiple-projects dark](../assets/design/tui-b/multiple-projects-dark.svg) [ANSI](../assets/design/tui-b/multiple-projects-dark.ansi) |
| no daemon | ![no-daemon light](../assets/design/tui-b/no-daemon-light.svg) [ANSI](../assets/design/tui-b/no-daemon-light.ansi) | ![no-daemon dark](../assets/design/tui-b/no-daemon-dark.svg) [ANSI](../assets/design/tui-b/no-daemon-dark.ansi) |
| no project | ![no-project light](../assets/design/tui-b/no-project-light.svg) [ANSI](../assets/design/tui-b/no-project-light.ansi) | ![no-project dark](../assets/design/tui-b/no-project-dark.svg) [ANSI](../assets/design/tui-b/no-project-dark.ansi) |
| notice | ![notice light](../assets/design/tui-b/notice-light.svg) [ANSI](../assets/design/tui-b/notice-light.ansi) | ![notice dark](../assets/design/tui-b/notice-dark.svg) [ANSI](../assets/design/tui-b/notice-dark.ansi) |
| pane | ![pane light](../assets/design/tui-b/pane-light.svg) [ANSI](../assets/design/tui-b/pane-light.ansi) | ![pane dark](../assets/design/tui-b/pane-dark.svg) [ANSI](../assets/design/tui-b/pane-dark.ansi) |
| preview help | ![preview-help light](../assets/design/tui-b/preview-help-light.svg) [ANSI](../assets/design/tui-b/preview-help-light.ansi) | ![preview-help dark](../assets/design/tui-b/preview-help-dark.svg) [ANSI](../assets/design/tui-b/preview-help-dark.ansi) |
| preview | ![preview light](../assets/design/tui-b/preview-light.svg) [ANSI](../assets/design/tui-b/preview-light.ansi) | ![preview dark](../assets/design/tui-b/preview-dark.svg) [ANSI](../assets/design/tui-b/preview-dark.ansi) |
| project picker existing | ![project-picker-existing light](../assets/design/tui-b/project-picker-existing-light.svg) [ANSI](../assets/design/tui-b/project-picker-existing-light.ansi) | ![project-picker-existing dark](../assets/design/tui-b/project-picker-existing-dark.svg) [ANSI](../assets/design/tui-b/project-picker-existing-dark.ansi) |
| project picker | ![project-picker light](../assets/design/tui-b/project-picker-light.svg) [ANSI](../assets/design/tui-b/project-picker-light.ansi) | ![project-picker dark](../assets/design/tui-b/project-picker-dark.svg) [ANSI](../assets/design/tui-b/project-picker-dark.ansi) |
| project picker overflow | ![project-picker-overflow light](../assets/design/tui-b/project-picker-overflow-light.svg) [ANSI](../assets/design/tui-b/project-picker-overflow-light.ansi) | ![project-picker-overflow dark](../assets/design/tui-b/project-picker-overflow-dark.svg) [ANSI](../assets/design/tui-b/project-picker-overflow-dark.ansi) |
| projects unavailable | ![projects-unavailable light](../assets/design/tui-b/projects-unavailable-light.svg) [ANSI](../assets/design/tui-b/projects-unavailable-light.ansi) | ![projects-unavailable dark](../assets/design/tui-b/projects-unavailable-dark.svg) [ANSI](../assets/design/tui-b/projects-unavailable-dark.ansi) |
| prompt | ![prompt light](../assets/design/tui-b/prompt-light.svg) [ANSI](../assets/design/tui-b/prompt-light.ansi) | ![prompt dark](../assets/design/tui-b/prompt-dark.svg) [ANSI](../assets/design/tui-b/prompt-dark.ansi) |
| rail project selection | ![rail-project-selection light](../assets/design/tui-b/rail-project-selection-light.svg) [ANSI](../assets/design/tui-b/rail-project-selection-light.ansi) | ![rail-project-selection dark](../assets/design/tui-b/rail-project-selection-dark.svg) [ANSI](../assets/design/tui-b/rail-project-selection-dark.ansi) |
| rail task selection | ![rail-task-selection light](../assets/design/tui-b/rail-task-selection-light.svg) [ANSI](../assets/design/tui-b/rail-task-selection-light.ansi) | ![rail-task-selection dark](../assets/design/tui-b/rail-task-selection-dark.svg) [ANSI](../assets/design/tui-b/rail-task-selection-dark.ansi) |
| search | ![search light](../assets/design/tui-b/search-light.svg) [ANSI](../assets/design/tui-b/search-light.ansi) | ![search dark](../assets/design/tui-b/search-dark.svg) [ANSI](../assets/design/tui-b/search-dark.ansi) |
| search overflow | ![search-overflow light](../assets/design/tui-b/search-overflow-light.svg) [ANSI](../assets/design/tui-b/search-overflow-light.ansi) | ![search-overflow dark](../assets/design/tui-b/search-overflow-dark.svg) [ANSI](../assets/design/tui-b/search-overflow-dark.ansi) |
| selection | ![selection light](../assets/design/tui-b/selection-light.svg) [ANSI](../assets/design/tui-b/selection-light.ansi) | ![selection dark](../assets/design/tui-b/selection-dark.svg) [ANSI](../assets/design/tui-b/selection-dark.ansi) |
| selection overflow | ![selection-overflow light](../assets/design/tui-b/selection-overflow-light.svg) [ANSI](../assets/design/tui-b/selection-overflow-light.ansi) | ![selection-overflow dark](../assets/design/tui-b/selection-overflow-dark.svg) [ANSI](../assets/design/tui-b/selection-overflow-dark.ansi) |
| sessions | ![sessions light](../assets/design/tui-b/sessions-light.svg) [ANSI](../assets/design/tui-b/sessions-light.ansi) | ![sessions dark](../assets/design/tui-b/sessions-dark.svg) [ANSI](../assets/design/tui-b/sessions-dark.ansi) |
| single project | ![single-project light](../assets/design/tui-b/single-project-light.svg) [ANSI](../assets/design/tui-b/single-project-light.ansi) | ![single-project dark](../assets/design/tui-b/single-project-dark.svg) [ANSI](../assets/design/tui-b/single-project-dark.ansi) |
| task actions | ![task-actions light](../assets/design/tui-b/task-actions-light.svg) [ANSI](../assets/design/tui-b/task-actions-light.ansi) | ![task-actions dark](../assets/design/tui-b/task-actions-dark.svg) [ANSI](../assets/design/tui-b/task-actions-dark.ansi) |
| task create | ![task-create light](../assets/design/tui-b/task-create-light.svg) [ANSI](../assets/design/tui-b/task-create-light.ansi) | ![task-create dark](../assets/design/tui-b/task-create-dark.svg) [ANSI](../assets/design/tui-b/task-create-dark.ansi) |
| task delete | ![task-delete light](../assets/design/tui-b/task-delete-light.svg) [ANSI](../assets/design/tui-b/task-delete-light.ansi) | ![task-delete dark](../assets/design/tui-b/task-delete-dark.svg) [ANSI](../assets/design/tui-b/task-delete-dark.ansi) |
| task edit save failed | ![task-edit-save-failed light](../assets/design/tui-b/task-edit-save-failed-light.svg) [ANSI](../assets/design/tui-b/task-edit-save-failed-light.ansi) | ![task-edit-save-failed dark](../assets/design/tui-b/task-edit-save-failed-dark.svg) [ANSI](../assets/design/tui-b/task-edit-save-failed-dark.ansi) |
| task program | ![task-program light](../assets/design/tui-b/task-program-light.svg) [ANSI](../assets/design/tui-b/task-program-light.ansi) | ![task-program dark](../assets/design/tui-b/task-program-dark.svg) [ANSI](../assets/design/tui-b/task-program-dark.ansi) |
| task save failed | ![task-save-failed light](../assets/design/tui-b/task-save-failed-light.svg) [ANSI](../assets/design/tui-b/task-save-failed-light.ansi) | ![task-save-failed dark](../assets/design/tui-b/task-save-failed-dark.svg) [ANSI](../assets/design/tui-b/task-save-failed-dark.ansi) |
| task schedule | ![task-schedule light](../assets/design/tui-b/task-schedule-light.svg) [ANSI](../assets/design/tui-b/task-schedule-light.ansi) | ![task-schedule dark](../assets/design/tui-b/task-schedule-dark.svg) [ANSI](../assets/design/tui-b/task-schedule-dark.ansi) |
| task schedule type | ![task-schedule-type light](../assets/design/tui-b/task-schedule-type-light.svg) [ANSI](../assets/design/tui-b/task-schedule-type-light.ansi) | ![task-schedule-type dark](../assets/design/tui-b/task-schedule-type-dark.svg) [ANSI](../assets/design/tui-b/task-schedule-type-dark.ansi) |
| task trigger | ![task-trigger light](../assets/design/tui-b/task-trigger-light.svg) [ANSI](../assets/design/tui-b/task-trigger-light.ansi) | ![task-trigger dark](../assets/design/tui-b/task-trigger-dark.svg) [ANSI](../assets/design/tui-b/task-trigger-dark.ansi) |
| task weekdays | ![task-weekdays light](../assets/design/tui-b/task-weekdays-light.svg) [ANSI](../assets/design/tui-b/task-weekdays-light.ansi) | ![task-weekdays dark](../assets/design/tui-b/task-weekdays-dark.svg) [ANSI](../assets/design/tui-b/task-weekdays-dark.ansi) |
| task weekdays unchecked | ![task-weekdays-unchecked light](../assets/design/tui-b/task-weekdays-unchecked-light.svg) [ANSI](../assets/design/tui-b/task-weekdays-unchecked-light.ansi) | ![task-weekdays-unchecked dark](../assets/design/tui-b/task-weekdays-unchecked-dark.svg) [ANSI](../assets/design/tui-b/task-weekdays-unchecked-dark.ansi) |
| tasks | ![tasks light](../assets/design/tui-b/tasks-light.svg) [ANSI](../assets/design/tui-b/tasks-light.ansi) | ![tasks dark](../assets/design/tui-b/tasks-dark.svg) [ANSI](../assets/design/tui-b/tasks-dark.ansi) |
| tasks unavailable | ![tasks-unavailable light](../assets/design/tui-b/tasks-unavailable-light.svg) [ANSI](../assets/design/tui-b/tasks-unavailable-light.ansi) | ![tasks-unavailable dark](../assets/design/tui-b/tasks-unavailable-dark.svg) [ANSI](../assets/design/tui-b/tasks-unavailable-dark.ansi) |
| too small | ![too-small light](../assets/design/tui-b/too-small-light.svg) [ANSI](../assets/design/tui-b/too-small-light.ansi) | ![too-small dark](../assets/design/tui-b/too-small-dark.svg) [ANSI](../assets/design/tui-b/too-small-dark.ansi) |
| zero accounts | ![zero-accounts light](../assets/design/tui-b/zero-accounts-light.svg) [ANSI](../assets/design/tui-b/zero-accounts-light.ansi) | ![zero-accounts dark](../assets/design/tui-b/zero-accounts-dark.svg) [ANSI](../assets/design/tui-b/zero-accounts-dark.ansi) |
| zero sessions | ![zero-sessions light](../assets/design/tui-b/zero-sessions-light.svg) [ANSI](../assets/design/tui-b/zero-sessions-light.ansi) | ![zero-sessions dark](../assets/design/tui-b/zero-sessions-dark.svg) [ANSI](../assets/design/tui-b/zero-sessions-dark.ansi) |
| zero tasks | ![zero-tasks light](../assets/design/tui-b/zero-tasks-light.svg) [ANSI](../assets/design/tui-b/zero-tasks-light.ansi) | ![zero-tasks dark](../assets/design/tui-b/zero-tasks-dark.svg) [ANSI](../assets/design/tui-b/zero-tasks-dark.ansi) |
| sessions dense | ![sessions-dense light](../assets/design/tui-b/sessions-dense-light.svg) [ANSI](../assets/design/tui-b/sessions-dense-light.ansi) | ![sessions-dense dark](../assets/design/tui-b/sessions-dense-dark.svg) [ANSI](../assets/design/tui-b/sessions-dense-dark.ansi) |
| projects degraded | ![projects-degraded light](../assets/design/tui-b/projects-degraded-light.svg) [ANSI](../assets/design/tui-b/projects-degraded-light.ansi) | ![projects-degraded dark](../assets/design/tui-b/projects-degraded-dark.svg) [ANSI](../assets/design/tui-b/projects-degraded-dark.ansi) |
