# TUI recovery states

P4 applies the [empty-state and notice recipes](interface-design.md#empty-states)
using the generated lipgloss roles. Conditions have one bold row, explanation
and the next action use body ink, and empty rail sections reserve no rows.

These are **deterministic app-model driver captures**, not recordings of a live
daemon. `app/recovery_test.go` drives the real `home.Update` and `home.View`
paths, injects rejected mutations, and checks that session and task input
survives. The SVGs faithfully convert the resulting ANSI cell grid. Matching
SVG and ANSI goldens in `app/testdata/recovery` are both compared byte-for-byte
by `TestRecoveryDriverScenes`, making geometry, copy, weight, colour and terminal
escape sequences reviewable in tests. The 14 scenes in both themes have 28
`.svg`/`.ansi` pairs. Existing surrounding TUI chrome is outside P4.
The zero-task and task-load-failure scenes keep the task manager's `Tasks`
title and a pinned `n new · esc back` hint containing only live actions.

| State | Light | Dark |
| --- | --- | --- |
| Zero sessions | ![Zero sessions, light](../assets/recovery/tui-model-driver/zero-sessions-light.svg) | ![Zero sessions, dark](../assets/recovery/tui-model-driver/zero-sessions-dark.svg) |
| No project registered | ![No project registered, light](../assets/recovery/tui-model-driver/no-project-light.svg) | ![No project registered, dark](../assets/recovery/tui-model-driver/no-project-dark.svg) |
| Zero tasks | ![Zero tasks, light](../assets/recovery/tui-model-driver/zero-tasks-light.svg) | ![Zero tasks, dark](../assets/recovery/tui-model-driver/zero-tasks-dark.svg) |
| Zero accounts | ![Zero accounts, light](../assets/recovery/tui-model-driver/zero-accounts-light.svg) | ![Zero accounts, dark](../assets/recovery/tui-model-driver/zero-accounts-dark.svg) |
| Remote accounts; login refused | ![Remote accounts, light](../assets/recovery/tui-model-driver/remote-accounts-light.svg) | ![Remote accounts, dark](../assets/recovery/tui-model-driver/remote-accounts-dark.svg) |
| Tasks unavailable | ![Tasks unavailable, light](../assets/recovery/tui-model-driver/tasks-unavailable-light.svg) | ![Tasks unavailable, dark](../assets/recovery/tui-model-driver/tasks-unavailable-dark.svg) |
| Projects unavailable | ![Projects unavailable, light](../assets/recovery/tui-model-driver/projects-unavailable-light.svg) | ![Projects unavailable, dark](../assets/recovery/tui-model-driver/projects-unavailable-dark.svg) |
| Cannot reach the daemon | ![Cannot reach the daemon, light](../assets/recovery/tui-model-driver/no-daemon-light.svg) | ![Cannot reach the daemon, dark](../assets/recovery/tui-model-driver/no-daemon-dark.svg) |
| Failed session create | ![Failed session create, light](../assets/recovery/tui-model-driver/create-failed-light.svg) | ![Failed session create, dark](../assets/recovery/tui-model-driver/create-failed-dark.svg) |
| Failed archive | ![Failed archive, light](../assets/recovery/tui-model-driver/archive-failed-light.svg) | ![Failed archive, dark](../assets/recovery/tui-model-driver/archive-failed-dark.svg) |
| Failed kill | ![Failed kill, light](../assets/recovery/tui-model-driver/kill-failed-light.svg) | ![Failed kill, dark](../assets/recovery/tui-model-driver/kill-failed-dark.svg) |
| Failed task save | ![Failed task save, light](../assets/recovery/tui-model-driver/task-save-failed-light.svg) | ![Failed task save, dark](../assets/recovery/tui-model-driver/task-save-failed-dark.svg) |
| Failed existing task save | ![Failed existing task save, light](../assets/recovery/tui-model-driver/task-edit-save-failed-light.svg) | ![Failed existing task save, dark](../assets/recovery/tui-model-driver/task-edit-save-failed-dark.svg) |
| Too-small terminal | ![Too-small terminal, light](../assets/recovery/tui-model-driver/too-small-light.svg) | ![Too-small terminal, dark](../assets/recovery/tui-model-driver/too-small-dark.svg) |

The complementary [real tmux driver](https://github.com/sachiniyer/agent-factory/blob/f030c8fe7b26353a8bc966f046df4d3858801a6c/scripts/tui-3915-scenario.sh) runs
inside the isolated testbox with a cheap bash stand-in. It starts with no
sessions, opens the empty task manager, submits a create rejected by the real
daemon, verifies the name remains in the form, changes its backend, and reaches
a live interactive terminal with `FIRST_SESSION_REACHED` echoed. It then checks
the too-small-terminal screen. [Its captured terminal text](../assets/recovery/tui-live-driver/first-session.txt)
is real application evidence; it does not assert agent authentication or agent
responses. No host AF home, daemon, app process or account is used.

Mutation recovery screens persist until one key returns to the retained state;
the key is consumed so dismissing a failure cannot accidentally submit or delete.
An async create failure while another form is open preserves that newer form and
retains the failed draft for the next create in its original project. Background
snapshot failures retain loaded sessions and retry automatically.

To verify, run `scripts/testbox.sh test ./app -run 'TestRecovery' -count=1` and
`scripts/testbox.sh scenario scripts/tui-3915-scenario.sh`.

To recapture, run the following from the writable source copy **inside** an
isolated [testbox sandbox](../dev/container-testing.md):

```sh
AF_TUI_RECOVERY_CAPTURE=/tmp/recovery-stills \
go test ./app -run 'TestRecoveryDriverScenes' -count=1
```

Capture mode writes both `<scene>-<theme>.svg` and `<scene>-<theme>.ansi` to
that directory and skips golden comparisons. Copy the directory out before the
sandbox exits. Inspect every SVG and ANSI diff, then replace both halves in
`app/testdata/recovery` and the gallery's `docs/assets/recovery/tui-model-driver`
as needed. Keep the `.gitattributes` ANSI whitespace rule: terminal cell padding
and trailing viewport rows are part of the asserted frame. Verify again without
`AF_TUI_RECOVERY_CAPTURE` so the test checks the committed pairs.
