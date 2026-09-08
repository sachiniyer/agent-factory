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

## Refocus regression from the full perf lane

CI run 34174632552's trace shows the first (360px) phone pass completing, then
`Control+]` at 63594ms and textarea refocus at 63623ms. On the second (390px)
pass, Ctrl → Up clears Ctrl, but `keyboardInsertText("ls")` at 71080ms emits
nothing. The `[7m`/`[27m` in the failure message are Playwright's diff highlighting,
not bytes in the captured input. `phoneInputStream` only records outgoing
`Op.Input` frames, never PTY output.

Xterm 5 sets `_keyDownSeen` before invoking the custom shortcut handler. The
shortcut blurs the textarea, so xterm misses keyup and retains that flag. Its
native `_inputEvent` then discards composed `insertText` events. The keybar's
existing workaround intercepted only armed input; once the bar consumed the
modifier, the following plain letters fell through to that stale xterm state.

The fix applies the existing soft-input interception to plain text as well.
Physical-key and composition guards remain. The focused regression now repeats
all modifier gestures after `Control+]` and refocus, then verifies physical
letters are sent once. The shared helper also checks locked Ctrl + Up + soft
`x` yields `ESC[1;5A` + `0x18` and remains locked. Both the demo and probe use
these exact input assertions without synthesizing keyup or resetting xterm.

Validation after the refocus fix:

- Full `make perf-container`: [red](perf-red.txt), then [green](perf-green.txt).
  All five visual tests passed without golden updates; the three-run web/TUI
  measurements passed every budget.
- Focused container phone spec: [refocus red](refocus-red.txt), then
  [refocus green](refocus-green.txt), including the repeat after blur/refocus.
- `npm test`: 744/744; typecheck and rebuilt `web/dist` passed.
- gofmt, Go build/vet, fast lint, and file-length lint passed.
