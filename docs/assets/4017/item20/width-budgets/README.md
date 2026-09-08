# Tab deletion copy at fixed terminal budgets

Captured inside testbox from the daemon-free app model with the existing pinned
design clock, then rendered and visually read with Chromium inside an isolated
container. No terminal or AF daemon ran on the host.

The before captures use the merged #4030 head `4e0067dbf88c27fd9926600466752b903e7d1202`.
The after captures change only the tab hint and help description:

| Surface | Before | After |
| --- | --- | --- |
| Footer | `w delete tab` | `w del tab` |
| Help | Delete the current tab after confirmation (except the agent tab) | Delete tab (asks first; agent tab excluded) |

The 100×24 footer retains every tab/pane hint and the help/quit keys. At 120×24,
`a archive` fits again. The 80×24 help row wraps to two lines rather than three;
the complete general help passes its existing maximum of five page-downs.
Light and dark stills cover all three views before and after.

Full container validation: `scripts/testbox.sh test ./app -count=1` and
`scripts/testbox.sh test ./ui -count=1` passed. No page limit, terminal size,
required hint, or hint-drop priority changed. Existing design goldens passed
unchanged.
