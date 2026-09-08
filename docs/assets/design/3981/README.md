# Session-first phone layout · #3981

A selected terminal at ≤768px gets one 48px chrome row: drawer toggle, truncating
session title, static Keyboard state and one … disclosure. The disclosure keeps
project choices, Sessions · Tasks · Config, Light · Dark · System, Disconnect,
install, pane actions, new-tab choices and the tab switcher reachable. Escape
closes it and returns focus. Drawer, Tasks, Config and desktop layouts remain
available. Opening the drawer preserves the session-first composition and the
terminal rectangle. Split sessions show the focused pane, with Hide pane in the menu,
and restore both panes on desktop. The six-button primary keybar is one 44px row; Arrows replaces it with
More keys and four arrow keys, preserving 44px targets and terminal focus.

## Measured height budget

Measurements use Chromium's visual viewport with the OS soft keyboard closed,
while the terminal owns keyboard input and the keybar is visible. Both themes
produce the same geometry. The terminal element is `.af-pane-host .xterm`.

| Width | Theme | Visual viewport | Top chrome | Keybar | Terminal | Terminal share |
| --- | --- | --- | --- | --- | --- | --- |
| 360px | Light | 812px | 48px | 44px | 718px | 88.4% |
| 360px | Dark | 812px | 48px | 44px | 718px | 88.4% |
| 390px | Light | 812px | 48px | 44px | 718px | 88.4% |
| 390px | Dark | 812px | 48px | 44px | 718px | 88.4% |
| 430px | Light | 812px | 48px | 44px | 718px | 88.4% |
| 430px | Dark | 812px | 48px | 44px | 718px | 88.4% |

The regression assertion requires at least 85% at all three widths in both
themes. The unchanged bundle failed at 360px with a 476px terminal (58.6%).
The pane retains its 2px padding so its inset focus border clears column one.
Browser emulation does not show an actual OS keyboard; the stream suite also
simulates visual viewport resize and pan to verify terminal fit above it.

## Before and after

Before stills are the committed recorder captures from starting master
`df215cfe` (after #3980 merged). After stills use the real client and isolated
daemon with the same scripted stand-in transcript, at 812px viewport height.
These are browser captures, not mockups.

| Width | Theme | Before | After |
| --- | --- | --- | --- |
| 360px | Light | ![Before 360 Light](before-phone-session-360.png) | ![After 360 Light](after-phone-session-360.png) |
| 360px | Dark | ![Before 360 Dark](before-phone-session-360-dark.png) | ![After 360 Dark](after-phone-session-360-dark.png) |
| 390px | Light | ![Before 390 Light](before-phone-session-390.png) | ![After 390 Light](after-phone-session-390.png) |
| 390px | Dark | ![Before 390 Dark](before-phone-session-390-dark.png) | ![After 390 Dark](after-phone-session-390-dark.png) |
| 430px | Light | ![Before 430 Light](before-phone-session-430.png) | ![After 430 Light](after-phone-session-430.png) |
| 430px | Dark | ![Before 430 Dark](before-phone-session-430-dark.png) | ![After 430 Dark](after-phone-session-430-dark.png) |

## Verification

The new composition and one-row keybar tests were watched fail against the
original sources: [unit red evidence](unit-red.txt). The browser budget failed
against the original tracked bundle: [browser red evidence](browser-red.txt).

The browser recorder checks one-row chrome, title truncation and full accessible
text, 44px targets, disclosure activation and Escape focus return, and the height
budget in both themes. Screen bounds must remain inside the pane-host content
box and clear the painted focus border; the host must have zero horizontal
scroll. Opening and closing the drawer must preserve the host rectangle. The existing outgoing binary PTY Input assertions cover
interrupt, one-shot Ctrl, composed input, locked Ctrl, focus retention and
viewport changes; arrow selection additionally checks the outgoing arrow bytes.

Sources: [composition](https://github.com/sachiniyer/agent-factory/blob/master/web/src/components.ts),
[keybar](https://github.com/sachiniyer/agent-factory/blob/master/web/src/terminal-keybar.ts),
[browser assertions](https://github.com/sachiniyer/agent-factory/blob/master/web/selftest/web-demo.spec.ts).

The review regressions were watched fail on head `9edbad57` before the fixes:
[unit output](review-unit-red.txt) and [browser output](review-browser-red.txt).
[Corrected browser output](review-browser-green.txt) records all six measurements.
The requested host bounds and `scrollLeft` checks already passed on that head;
the additional painted-border assertion exposed the clipping (`0px < 2px`).
Restoring token-based padding puts the screen at 2px, clear of the border.
The drawer formerly moved the host from y=48px to y=228px; it now leaves both
its position and size unchanged.

| Width (both themes) | Screen left / painted edge | Screen right / host right | Host scrollLeft | Closed = open host (x, y, width, height) |
| --- | --- | --- | --- | --- |
| 360px | 2px / 2px | 340px / 358px | 0 | (2, 50, 356, 760) |
| 390px | 2px / 2px | 373px / 388px | 0 | (2, 50, 386, 760) |
| 430px | 2px / 2px | 412px / 428px | 0 | (2, 50, 426, 760) |

The browser helpers resolve the visible disclosure trigger and wait for phone or
desktop composition after viewport changes.
The #2219 phone test explicitly documents that desktop caret anchoring is outside
its coverage.

All 740 web unit tests passed, with source and recorder typechecking and the
tracked bundle rebuilt. The full containerized `web-driver.spec.ts` passed all
146 tests with zero retries, including tab reordering, drawer
navigation and split-terminal restoration.

`AF_UPDATE_GOLDENS=1 make perf-container` regenerated both themes; comparison
without the flag passed all five visual scenarios and every unchanged performance
budget. Capture readiness is checked before and after screenshots so transient
operator-state changes cannot become goldens. The pixel oracle and performance thresholds are unchanged.
`scripts/gen-docs.sh`, design-token tests, generated-design checking, file-length
lint and the Docs job's exact `mkdocs build --strict` passed. No Go source changed.
