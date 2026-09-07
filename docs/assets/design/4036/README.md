# Phone keybar modifiers · #4036

Container-captured phone design stills (390×812):

| Before | After |
| --- | --- |
| ![Ctrl remains armed after Up](before-phone-keybar.png) | ![Ctrl clears after the bar key](after-phone-keybar.png) |

The before still is master with the new browser regression: Ctrl → Arrows → Up
→ Back sends plain `ESC[A` and leaves Ctrl armed. The after still is the fixed
client after the focused modifier flow. The container uses its scripted test
agent; the displayed byte echo is not a real-agent interaction.

Reproduce the browser assertions and screenshots with:

```sh
AF_PLAYWRIGHT_ARGS='phone-keybar.spec.ts' scripts/testbox.sh web-selftest
```

The browser observes actual outgoing binary Op.Input frames via the existing
`phoneInputStream` seam. Ctrl → Up → soft-keyboard `ls` must send exactly
`ESC[1;5Als`; Alt → Left → `ls` must send exactly `ESC[1;3Dls`.
Both one-shots clear their state and armed accessibility description.
The focused flow also verifies locked Ctrl + Up and one-shot Alt + Tab + `z`.
The demo’s existing full phone flow reuses these assertions and retains its
coverage of plain arrows, interrupt, composition, physical keypresses, focus,
and viewport resizing.

[Unit red](unit-red.txt) records the issue's two original tests failing against
master before implementation. The second reproduction now invokes the explicit
bar-key operation (`key("←")`) and expects the modified bytes; its original
`input(ESC[D)` call represents xterm emissions, which intentionally must not
consume modifiers. The existing terminal-reply preservation test remains intact.
[Unit green](unit-green.txt) records all 10 focused tests passing.
[Browser red](browser-red.txt) and [browser green](browser-green.txt) record the
same browser regression before and after the fix.

No Go TUI design golden changed.
