# Focused-session phone header · #3967

The six `before-phone-session-*` captures were recorded before implementation,
from master `3905ab48104334fce8cf126e4fdb43641ec8ad8f`, using the existing container
recorder/selftest (`make perf-container`). The only recorder change added the
360, 390 and 430px screenshots; the UI and bundle were unchanged. Both themes
use 812px height. The baseline visual suite and all performance budgets passed.

Before: the session title and project/view navigation disappear, while drawer,
tabs, PR link, Keyboard and Actions compete for one row.

After: project/view navigation remains above the pane; title, static Keyboard
and Actions have one row, with a horizontally scrolling tab row beneath. The
same session, viewport heights and themes are used. The title retains its full
text and tooltip while CSS truncates it with … when needed. Targets are 44px.

These are the real web client and an isolated daemon with the recorder's seeded
stand-in agent transcript, not production work or a layout mockup. The recorder
asserts header visibility, keyboard activation/Tab/Escape, title truncation,
separate rows, scrolling tabs, target heights and viewport fit at each width.

`unit-before.txt` records all three new unit tests failing before implementation.
The resize test also caught desktop Escape focusing the hidden phone trigger;
that regression was fixed before submission.

Current master already had `phone-session` and `phone-session-dark` goldens.
This PR refreshes and strengthens those existing cases, using the unchanged
`maxDiffPixels: 0` oracle. Performance budgets are unchanged.
