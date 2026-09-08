# Remove branch-associated PRs (#4016)

The pinned `TestDesignDriverScenes/help-actions` scene opens general help and
scrolls down 26 lines at 120×36. Captured in the testbox container with
`AF_TUI_DESIGN_CAPTURE=/tmp/capture`; Chromium in the web testbox rendered the
SVGs to PNGs. Both palettes were visually inspected.

The before capture runs base `e4e67cb294b70e215067e7410c4dbf3035f08b02`
with only the new scene added to its existing design harness. The after capture
runs this change.
Configuration now leads directly into Tabs; the GitHub PR heading and `p`/`y`
actions have disappeared. The after SVGs also live in `app/testdata/design/`
as deterministic golden fixtures. All existing design scenes were recaptured
in the container and remained byte-identical.

| | Before | After |
| --- | --- | --- |
| Dark | ![Before, dark](help-before-dark.png) | ![After, dark](help-after-dark.png) |
| Light | ![Before, light](help-before-light.png) | ![After, light](help-after-light.png) |

Capture command (inside the container, after copying the read-only source):

```sh
AF_TUI_DESIGN_CAPTURE=/tmp/capture go test -buildvcs=false ./app \
  -run 'TestDesignDriverScenes/help-actions' -count=1
```

## Web media follow-up

The TUI captures above do not cover the web demo or web visual goldens. Those
were separately recaptured from `7c51922d506a837831c201b823b4deeffadab3d3` with
`web-demo-entry.sh` in isolated containers. Only the three recorder passes
(`web demo` in both palettes and `web chrome`) ran; the behavioral tests,
performance benchmarks, and Go suites did not run for this follow-up.

The poster, MP4, WebM, and GIF were regenerated. The 38 affected web stills and
matching visual goldens were inspected before acceptance, including all four
`review` / `comparison-review` images. Public stills use the settled visual
captures and lossless PNG encoding. Unrelated capture noise was discarded.

At 1440×900 the badge disappears without moving the remaining header controls,
rail, or terminal. At 375×812 the phone menu loses the PR row and its following
actions move up by that row's height; the header and terminal do not move.
The 360, 390, and 430 px phone captures were also read in both palettes; terminal
alignment and drawer geometry remained intact. The poster and sampled MP4/GIF
frames show no PR badge. The MP4 is 32.08 seconds and 455,022 bytes; the GIF is
3,902,256 bytes, within the recorder's existing budgets.
