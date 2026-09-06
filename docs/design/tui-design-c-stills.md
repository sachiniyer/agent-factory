# TUI design system: slice C

The gallery files were regenerated from master after P2/P5 for
[#3917](https://github.com/sachiniyer/agent-factory/issues/3917). The slice history
below records the original implementation; the images now show the final design.

The TUI now offers **Light**, **Dark** and **System**, with the same saved
`light`, `dark` and `system` values as the browser. The global `appearance`
setting applies on the next TUI launch. System detects the terminal background
before Bubble Tea starts, using termenv's OSC 11 / `COLORFGBG` fallback and dark
when unavailable. Forced modes bypass detection. The browser header preference
is separate and follows the OS for System.

Legacy `auto`, absent and unknown values resolve to System; reading does not
rewrite the file, and saving a valid choice replaces it. Retired palette rows
are hidden even from older daemon manifests. See [configuration migration](../configuration.md#theme-colors-theme)
for the full retired-key list and the deliberately separate daemon cleanup in
[#3936](https://github.com/sachiniyer/agent-factory/issues/3936).

Help omits the `v` prefix when the build has no Version. Released builds still
show their version. No other TUI layout or semantic mapping changes in C.

## Capture provenance

These 116 light/dark frames were captured inside the existing make-created
`af-p5-3916-b` playtest sandbox, using the P4 app-model driver and ANSI-to-SVG
writer. Each SVG has its source ANSI beside it. No host TUI or app/daemon tests.
The new appearance scenes show explicit Light/Dark and System with each resolved
palette. System's resolved background is fixed by the driver for deterministic
stills; separate unit tests exercise detection, environment fallback, dark
fallback, and bypass for explicit choices.

Only eight TUI SVGs differ from B: four new appearance captures and four help /
preview-help captures that remove the empty version prefix. The other 108 are
byte-identical. These are app-model Update/View frames, not authenticated-agent
recordings.

Reproduce inside the make-created sandbox, with `/src` as cwd:

```sh
AF_TUI_DESIGN_CAPTURE=/home/dev/sandbox/p5-c-design \
AF_TUI_RECOVERY_CAPTURE=/home/dev/sandbox/p5-c-recovery \
/usr/local/go/bin/go test ./app -run 'Test(Recovery|DesignDriverScenes|HelpTitleWithAndWithoutVersion)' -count=1
```

Without capture variables, the driver compares the checked-in app goldens.
The shared manifest also adds the TUI setting to web Config, so its affected
Config snapshots are refreshed by the perf container. Browser appearance
selection is unchanged.

## Verification

[Role/contrast audit](../assets/design/tui-c/role-audit.txt),
[state mapping enumeration](../assets/design/tui-c/state-mappings.txt),
[source branch inventory](../assets/design/tui-c/state-branches.txt), and
[style construction inventory](../assets/design/tui-c/style-constructions.txt)
cover the complete C diff and the inherited UI state mappings.
[Verification record](../assets/design/tui-c/verification.txt) records checks.

The final `make perf-container` run passed at 1,000 sessions: full frame mean **79.734 ms**,
max **88.021 ms**; key-to-render mean **63.709 ms**, max **68.426 ms**.
Every sample is within the P5 **480 ms / 351 ms** budgets. No budgets changed.
[Raw TUI samples](../assets/design/tui-c/perf/tui-runs.json) and
[complete metrics](../assets/design/tui-c/perf/metrics.txt) retain the evidence.

## Stills

| Scene | Light | Dark |
| --- | --- | --- |
| appearance | ![appearance light](../assets/design/tui-c/appearance-light.svg) [ANSI](../assets/design/tui-c/appearance-light.ansi) | ![appearance dark](../assets/design/tui-c/appearance-dark.svg) [ANSI](../assets/design/tui-c/appearance-dark.ansi) |
| appearance system | ![appearance-system light](../assets/design/tui-c/appearance-system-light.svg) [ANSI](../assets/design/tui-c/appearance-system-light.ansi) | ![appearance-system dark](../assets/design/tui-c/appearance-system-dark.svg) [ANSI](../assets/design/tui-c/appearance-system-dark.ansi) |
| help | ![help light](../assets/design/tui-c/help-light.svg) [ANSI](../assets/design/tui-c/help-light.ansi) | ![help dark](../assets/design/tui-c/help-dark.svg) [ANSI](../assets/design/tui-c/help-dark.ansi) |
| preview help | ![preview-help light](../assets/design/tui-c/preview-help-light.svg) [ANSI](../assets/design/tui-c/preview-help-light.ansi) | ![preview-help dark](../assets/design/tui-c/preview-help-dark.svg) [ANSI](../assets/design/tui-c/preview-help-dark.ansi) |
| account picker | ![account-picker light](../assets/design/tui-c/account-picker-light.svg) [ANSI](../assets/design/tui-c/account-picker-light.ansi) | ![account-picker dark](../assets/design/tui-c/account-picker-dark.svg) [ANSI](../assets/design/tui-c/account-picker-dark.ansi) |
| account register | ![account-register light](../assets/design/tui-c/account-register-light.svg) [ANSI](../assets/design/tui-c/account-register-light.ansi) | ![account-register dark](../assets/design/tui-c/account-register-dark.svg) [ANSI](../assets/design/tui-c/account-register-dark.ansi) |
| accounts | ![accounts light](../assets/design/tui-c/accounts-light.svg) [ANSI](../assets/design/tui-c/accounts-light.ansi) | ![accounts dark](../assets/design/tui-c/accounts-dark.svg) [ANSI](../assets/design/tui-c/accounts-dark.ansi) |
| alarm | ![alarm light](../assets/design/tui-c/alarm-light.svg) [ANSI](../assets/design/tui-c/alarm-light.ansi) | ![alarm dark](../assets/design/tui-c/alarm-dark.svg) [ANSI](../assets/design/tui-c/alarm-dark.ansi) |
| archive failed | ![archive-failed light](../assets/design/tui-c/archive-failed-light.svg) [ANSI](../assets/design/tui-c/archive-failed-light.ansi) | ![archive-failed dark](../assets/design/tui-c/archive-failed-dark.svg) [ANSI](../assets/design/tui-c/archive-failed-dark.ansi) |
| archive warning | ![archive-warning light](../assets/design/tui-c/archive-warning-light.svg) [ANSI](../assets/design/tui-c/archive-warning-light.ansi) | ![archive-warning dark](../assets/design/tui-c/archive-warning-dark.svg) [ANSI](../assets/design/tui-c/archive-warning-dark.ansi) |
| config | ![config light](../assets/design/tui-c/config-light.svg) [ANSI](../assets/design/tui-c/config-light.ansi) | ![config dark](../assets/design/tui-c/config-dark.svg) [ANSI](../assets/design/tui-c/config-dark.ansi) |
| config edit | ![config-edit light](../assets/design/tui-c/config-edit-light.svg) [ANSI](../assets/design/tui-c/config-edit-light.ansi) | ![config-edit dark](../assets/design/tui-c/config-edit-dark.svg) [ANSI](../assets/design/tui-c/config-edit-dark.ansi) |
| confirmation | ![confirmation light](../assets/design/tui-c/confirmation-light.svg) [ANSI](../assets/design/tui-c/confirmation-light.ansi) | ![confirmation dark](../assets/design/tui-c/confirmation-dark.svg) [ANSI](../assets/design/tui-c/confirmation-dark.ansi) |
| create failed | ![create-failed light](../assets/design/tui-c/create-failed-light.svg) [ANSI](../assets/design/tui-c/create-failed-light.ansi) | ![create-failed dark](../assets/design/tui-c/create-failed-dark.svg) [ANSI](../assets/design/tui-c/create-failed-dark.ansi) |
| failure notice | ![failure-notice light](../assets/design/tui-c/failure-notice-light.svg) [ANSI](../assets/design/tui-c/failure-notice-light.ansi) | ![failure-notice dark](../assets/design/tui-c/failure-notice-dark.svg) [ANSI](../assets/design/tui-c/failure-notice-dark.ansi) |
| hooks | ![hooks light](../assets/design/tui-c/hooks-light.svg) [ANSI](../assets/design/tui-c/hooks-light.ansi) | ![hooks dark](../assets/design/tui-c/hooks-dark.svg) [ANSI](../assets/design/tui-c/hooks-dark.ansi) |
| hooks add | ![hooks-add light](../assets/design/tui-c/hooks-add-light.svg) [ANSI](../assets/design/tui-c/hooks-add-light.ansi) | ![hooks-add dark](../assets/design/tui-c/hooks-add-dark.svg) [ANSI](../assets/design/tui-c/hooks-add-dark.ansi) |
| hooks edit | ![hooks-edit light](../assets/design/tui-c/hooks-edit-light.svg) [ANSI](../assets/design/tui-c/hooks-edit-light.ansi) | ![hooks-edit dark](../assets/design/tui-c/hooks-edit-dark.svg) [ANSI](../assets/design/tui-c/hooks-edit-dark.ansi) |
| keyboard | ![keyboard light](../assets/design/tui-c/keyboard-light.svg) [ANSI](../assets/design/tui-c/keyboard-light.ansi) | ![keyboard dark](../assets/design/tui-c/keyboard-dark.svg) [ANSI](../assets/design/tui-c/keyboard-dark.ansi) |
| kill failed | ![kill-failed light](../assets/design/tui-c/kill-failed-light.svg) [ANSI](../assets/design/tui-c/kill-failed-light.ansi) | ![kill-failed dark](../assets/design/tui-c/kill-failed-dark.svg) [ANSI](../assets/design/tui-c/kill-failed-dark.ansi) |
| multiple projects | ![multiple-projects light](../assets/design/tui-c/multiple-projects-light.svg) [ANSI](../assets/design/tui-c/multiple-projects-light.ansi) | ![multiple-projects dark](../assets/design/tui-c/multiple-projects-dark.svg) [ANSI](../assets/design/tui-c/multiple-projects-dark.ansi) |
| no daemon | ![no-daemon light](../assets/design/tui-c/no-daemon-light.svg) [ANSI](../assets/design/tui-c/no-daemon-light.ansi) | ![no-daemon dark](../assets/design/tui-c/no-daemon-dark.svg) [ANSI](../assets/design/tui-c/no-daemon-dark.ansi) |
| no project | ![no-project light](../assets/design/tui-c/no-project-light.svg) [ANSI](../assets/design/tui-c/no-project-light.ansi) | ![no-project dark](../assets/design/tui-c/no-project-dark.svg) [ANSI](../assets/design/tui-c/no-project-dark.ansi) |
| notice | ![notice light](../assets/design/tui-c/notice-light.svg) [ANSI](../assets/design/tui-c/notice-light.ansi) | ![notice dark](../assets/design/tui-c/notice-dark.svg) [ANSI](../assets/design/tui-c/notice-dark.ansi) |
| pane | ![pane light](../assets/design/tui-c/pane-light.svg) [ANSI](../assets/design/tui-c/pane-light.ansi) | ![pane dark](../assets/design/tui-c/pane-dark.svg) [ANSI](../assets/design/tui-c/pane-dark.ansi) |
| preview | ![preview light](../assets/design/tui-c/preview-light.svg) [ANSI](../assets/design/tui-c/preview-light.ansi) | ![preview dark](../assets/design/tui-c/preview-dark.svg) [ANSI](../assets/design/tui-c/preview-dark.ansi) |
| project picker | ![project-picker light](../assets/design/tui-c/project-picker-light.svg) [ANSI](../assets/design/tui-c/project-picker-light.ansi) | ![project-picker dark](../assets/design/tui-c/project-picker-dark.svg) [ANSI](../assets/design/tui-c/project-picker-dark.ansi) |
| project picker existing | ![project-picker-existing light](../assets/design/tui-c/project-picker-existing-light.svg) [ANSI](../assets/design/tui-c/project-picker-existing-light.ansi) | ![project-picker-existing dark](../assets/design/tui-c/project-picker-existing-dark.svg) [ANSI](../assets/design/tui-c/project-picker-existing-dark.ansi) |
| project picker overflow | ![project-picker-overflow light](../assets/design/tui-c/project-picker-overflow-light.svg) [ANSI](../assets/design/tui-c/project-picker-overflow-light.ansi) | ![project-picker-overflow dark](../assets/design/tui-c/project-picker-overflow-dark.svg) [ANSI](../assets/design/tui-c/project-picker-overflow-dark.ansi) |
| projects degraded | ![projects-degraded light](../assets/design/tui-c/projects-degraded-light.svg) [ANSI](../assets/design/tui-c/projects-degraded-light.ansi) | ![projects-degraded dark](../assets/design/tui-c/projects-degraded-dark.svg) [ANSI](../assets/design/tui-c/projects-degraded-dark.ansi) |
| projects unavailable | ![projects-unavailable light](../assets/design/tui-c/projects-unavailable-light.svg) [ANSI](../assets/design/tui-c/projects-unavailable-light.ansi) | ![projects-unavailable dark](../assets/design/tui-c/projects-unavailable-dark.svg) [ANSI](../assets/design/tui-c/projects-unavailable-dark.ansi) |
| prompt | ![prompt light](../assets/design/tui-c/prompt-light.svg) [ANSI](../assets/design/tui-c/prompt-light.ansi) | ![prompt dark](../assets/design/tui-c/prompt-dark.svg) [ANSI](../assets/design/tui-c/prompt-dark.ansi) |
| rail project selection | ![rail-project-selection light](../assets/design/tui-c/rail-project-selection-light.svg) [ANSI](../assets/design/tui-c/rail-project-selection-light.ansi) | ![rail-project-selection dark](../assets/design/tui-c/rail-project-selection-dark.svg) [ANSI](../assets/design/tui-c/rail-project-selection-dark.ansi) |
| rail task selection | ![rail-task-selection light](../assets/design/tui-c/rail-task-selection-light.svg) [ANSI](../assets/design/tui-c/rail-task-selection-light.ansi) | ![rail-task-selection dark](../assets/design/tui-c/rail-task-selection-dark.svg) [ANSI](../assets/design/tui-c/rail-task-selection-dark.ansi) |
| search | ![search light](../assets/design/tui-c/search-light.svg) [ANSI](../assets/design/tui-c/search-light.ansi) | ![search dark](../assets/design/tui-c/search-dark.svg) [ANSI](../assets/design/tui-c/search-dark.ansi) |
| search overflow | ![search-overflow light](../assets/design/tui-c/search-overflow-light.svg) [ANSI](../assets/design/tui-c/search-overflow-light.ansi) | ![search-overflow dark](../assets/design/tui-c/search-overflow-dark.svg) [ANSI](../assets/design/tui-c/search-overflow-dark.ansi) |
| selection | ![selection light](../assets/design/tui-c/selection-light.svg) [ANSI](../assets/design/tui-c/selection-light.ansi) | ![selection dark](../assets/design/tui-c/selection-dark.svg) [ANSI](../assets/design/tui-c/selection-dark.ansi) |
| selection overflow | ![selection-overflow light](../assets/design/tui-c/selection-overflow-light.svg) [ANSI](../assets/design/tui-c/selection-overflow-light.ansi) | ![selection-overflow dark](../assets/design/tui-c/selection-overflow-dark.svg) [ANSI](../assets/design/tui-c/selection-overflow-dark.ansi) |
| sessions | ![sessions light](../assets/design/tui-c/sessions-light.svg) [ANSI](../assets/design/tui-c/sessions-light.ansi) | ![sessions dark](../assets/design/tui-c/sessions-dark.svg) [ANSI](../assets/design/tui-c/sessions-dark.ansi) |
| sessions dense | ![sessions-dense light](../assets/design/tui-c/sessions-dense-light.svg) [ANSI](../assets/design/tui-c/sessions-dense-light.ansi) | ![sessions-dense dark](../assets/design/tui-c/sessions-dense-dark.svg) [ANSI](../assets/design/tui-c/sessions-dense-dark.ansi) |
| single project | ![single-project light](../assets/design/tui-c/single-project-light.svg) [ANSI](../assets/design/tui-c/single-project-light.ansi) | ![single-project dark](../assets/design/tui-c/single-project-dark.svg) [ANSI](../assets/design/tui-c/single-project-dark.ansi) |
| task actions | ![task-actions light](../assets/design/tui-c/task-actions-light.svg) [ANSI](../assets/design/tui-c/task-actions-light.ansi) | ![task-actions dark](../assets/design/tui-c/task-actions-dark.svg) [ANSI](../assets/design/tui-c/task-actions-dark.ansi) |
| task create | ![task-create light](../assets/design/tui-c/task-create-light.svg) [ANSI](../assets/design/tui-c/task-create-light.ansi) | ![task-create dark](../assets/design/tui-c/task-create-dark.svg) [ANSI](../assets/design/tui-c/task-create-dark.ansi) |
| task delete | ![task-delete light](../assets/design/tui-c/task-delete-light.svg) [ANSI](../assets/design/tui-c/task-delete-light.ansi) | ![task-delete dark](../assets/design/tui-c/task-delete-dark.svg) [ANSI](../assets/design/tui-c/task-delete-dark.ansi) |
| task edit save failed | ![task-edit-save-failed light](../assets/design/tui-c/task-edit-save-failed-light.svg) [ANSI](../assets/design/tui-c/task-edit-save-failed-light.ansi) | ![task-edit-save-failed dark](../assets/design/tui-c/task-edit-save-failed-dark.svg) [ANSI](../assets/design/tui-c/task-edit-save-failed-dark.ansi) |
| task program | ![task-program light](../assets/design/tui-c/task-program-light.svg) [ANSI](../assets/design/tui-c/task-program-light.ansi) | ![task-program dark](../assets/design/tui-c/task-program-dark.svg) [ANSI](../assets/design/tui-c/task-program-dark.ansi) |
| task save failed | ![task-save-failed light](../assets/design/tui-c/task-save-failed-light.svg) [ANSI](../assets/design/tui-c/task-save-failed-light.ansi) | ![task-save-failed dark](../assets/design/tui-c/task-save-failed-dark.svg) [ANSI](../assets/design/tui-c/task-save-failed-dark.ansi) |
| task schedule | ![task-schedule light](../assets/design/tui-c/task-schedule-light.svg) [ANSI](../assets/design/tui-c/task-schedule-light.ansi) | ![task-schedule dark](../assets/design/tui-c/task-schedule-dark.svg) [ANSI](../assets/design/tui-c/task-schedule-dark.ansi) |
| task schedule type | ![task-schedule-type light](../assets/design/tui-c/task-schedule-type-light.svg) [ANSI](../assets/design/tui-c/task-schedule-type-light.ansi) | ![task-schedule-type dark](../assets/design/tui-c/task-schedule-type-dark.svg) [ANSI](../assets/design/tui-c/task-schedule-type-dark.ansi) |
| task trigger | ![task-trigger light](../assets/design/tui-c/task-trigger-light.svg) [ANSI](../assets/design/tui-c/task-trigger-light.ansi) | ![task-trigger dark](../assets/design/tui-c/task-trigger-dark.svg) [ANSI](../assets/design/tui-c/task-trigger-dark.ansi) |
| task weekdays | ![task-weekdays light](../assets/design/tui-c/task-weekdays-light.svg) [ANSI](../assets/design/tui-c/task-weekdays-light.ansi) | ![task-weekdays dark](../assets/design/tui-c/task-weekdays-dark.svg) [ANSI](../assets/design/tui-c/task-weekdays-dark.ansi) |
| task weekdays unchecked | ![task-weekdays-unchecked light](../assets/design/tui-c/task-weekdays-unchecked-light.svg) [ANSI](../assets/design/tui-c/task-weekdays-unchecked-light.ansi) | ![task-weekdays-unchecked dark](../assets/design/tui-c/task-weekdays-unchecked-dark.svg) [ANSI](../assets/design/tui-c/task-weekdays-unchecked-dark.ansi) |
| tasks | ![tasks light](../assets/design/tui-c/tasks-light.svg) [ANSI](../assets/design/tui-c/tasks-light.ansi) | ![tasks dark](../assets/design/tui-c/tasks-dark.svg) [ANSI](../assets/design/tui-c/tasks-dark.ansi) |
| tasks unavailable | ![tasks-unavailable light](../assets/design/tui-c/tasks-unavailable-light.svg) [ANSI](../assets/design/tui-c/tasks-unavailable-light.ansi) | ![tasks-unavailable dark](../assets/design/tui-c/tasks-unavailable-dark.svg) [ANSI](../assets/design/tui-c/tasks-unavailable-dark.ansi) |
| too small | ![too-small light](../assets/design/tui-c/too-small-light.svg) [ANSI](../assets/design/tui-c/too-small-light.ansi) | ![too-small dark](../assets/design/tui-c/too-small-dark.svg) [ANSI](../assets/design/tui-c/too-small-dark.ansi) |
| zero accounts | ![zero-accounts light](../assets/design/tui-c/zero-accounts-light.svg) [ANSI](../assets/design/tui-c/zero-accounts-light.ansi) | ![zero-accounts dark](../assets/design/tui-c/zero-accounts-dark.svg) [ANSI](../assets/design/tui-c/zero-accounts-dark.ansi) |
| zero sessions | ![zero-sessions light](../assets/design/tui-c/zero-sessions-light.svg) [ANSI](../assets/design/tui-c/zero-sessions-light.ansi) | ![zero-sessions dark](../assets/design/tui-c/zero-sessions-dark.svg) [ANSI](../assets/design/tui-c/zero-sessions-dark.ansi) |
| zero tasks | ![zero-tasks light](../assets/design/tui-c/zero-tasks-light.svg) [ANSI](../assets/design/tui-c/zero-tasks-light.ansi) | ![zero-tasks dark](../assets/design/tui-c/zero-tasks-dark.svg) [ANSI](../assets/design/tui-c/zero-tasks-dark.ansi) |
