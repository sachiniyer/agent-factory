# Config remedy evidence (#4069)

These 80×24 dark-mode frames were rendered by the app's design-scene harness
inside `scripts/testbox.sh`, with its pinned clock. “Last notice” is open so the
complete remedy is visible. The before fixture uses the exact reserved-title
message from the base revision; the after fixture calls `ReservedTitleRefusal`.
Both use `HOME=/home/operator` and `AGENT_FACTORY_HOME=/home/operator/relocated-af`.

| Before | After |
| --- | --- |
| ![Before](before.svg) | ![After](after.svg) |

The ANSI frames and temporary capture source are included for reproduction.
Copy `capture_test.go.txt` to `app/config_remedy_capture_test.go`, then run only
in the container, after the load gate permits it:

```sh
scripts/testbox.sh test ./app -run 'TestConfigRemedyCapture$' -count=1 -v
```

The test logs base64 SVG/ANSI payloads in before/after order. Remove the temporary
test afterward. No `app/testdata/design` golden was changed.
