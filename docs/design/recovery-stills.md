# Recovery screens

P4 implements the [empty-state and notice recipes](interface-design.md#empty-states).
The web selftest renders the actual application and intercepts API responses to
make each empty or unavailable state deterministic. Failure scenes assert that
the form retains input. Run `AF_PLAYWRIGHT_ARGS=recovery.spec.ts make web-selftest-container`
to reproduce the captures; every image is also attached to the test report.

| Web state | Light | Dark |
| --- | --- | --- |
| No project registered | ![Light No project registered](../assets/recovery/web-no-project-light.png) | ![Dark No project registered](../assets/recovery/web-no-project-dark.png) |
| No sessions | ![Light No sessions](../assets/recovery/web-no-sessions-light.png) | ![Dark No sessions](../assets/recovery/web-no-sessions-dark.png) |
| No tasks | ![Light No tasks](../assets/recovery/web-no-tasks-light.png) | ![Dark No tasks](../assets/recovery/web-no-tasks-dark.png) |
| No accounts | ![Light No accounts](../assets/recovery/web-no-accounts-light.png) | ![Dark No accounts](../assets/recovery/web-no-accounts-dark.png) |
| Cannot reach the daemon | ![Light Cannot reach the daemon](../assets/recovery/web-no-daemon-light.png) | ![Dark Cannot reach the daemon](../assets/recovery/web-no-daemon-dark.png) |
| Login expired | ![Light Login expired](../assets/recovery/web-login-expired-light.png) | ![Dark Login expired](../assets/recovery/web-login-expired-dark.png) |
| Create failed | ![Light Create failed](../assets/recovery/web-create-failed-light.png) | ![Dark Create failed](../assets/recovery/web-create-failed-dark.png) |
| Archive failed | ![Light Archive failed](../assets/recovery/web-archive-failed-light.png) | ![Dark Archive failed](../assets/recovery/web-archive-failed-dark.png) |
| Kill failed | ![Light Kill failed](../assets/recovery/web-kill-failed-light.png) | ![Dark Kill failed](../assets/recovery/web-kill-failed-dark.png) |
| Task save failed | ![Light Task save failed](../assets/recovery/web-task-save-failed-light.png) | ![Dark Task save failed](../assets/recovery/web-task-save-failed-dark.png) |
