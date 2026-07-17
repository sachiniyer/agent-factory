## Summary

Expected client and session teardown races were emitting repeated warning messages even though the underlying failures were benign disconnects. This PR suppresses only recognized response-write disconnect errors and the exact session-deletion capture race, while preserving warnings for unexpected failures.

Fixes #1992

## Changes

- Ignore `context.Canceled`, closed-connection, broken-pipe, and connection-reset errors when writing an HTTP response after a client disconnects.
- Suppress the exact `session %q is being deleted` pane refresh error when the selected instance is already tearing down.
- Add focused regression tests proving benign teardown errors are quiet and unexpected errors remain visible.

## Testing

- `sandbox-run "/usr/local/go/bin/go test ./daemon -run '^TestHTTPResponseWriteAbandoned$'"` passed.
- `sandbox-run "/usr/local/go/bin/go test ./app -run '^(TestRefreshPaneBindingCmd_SuppressesTearingDownError|TestRefreshPaneBindingCmd_LogsUnexpectedError|TestPaneRefreshTearingDown)$'"` passed.
- `sandbox-run "/usr/local/go/bin/go test ./daemon ./app"` was attempted; the daemon package has existing sandbox failures because `tmux` is unavailable, and the app package has an unrelated `TestHandleStateTasks_ValidationFailureLeavesTaskPaneStale` failure.
- `gofmt` was run through `sandbox-run`; `git diff --check` passes.

---
### AI assistance disclosure

This contribution was produced by an autonomous AI coding agent (Claude Code) that @Dodothereal operates and monitors. @Dodothereal is accountable for it, will address review feedback promptly, and will close this PR immediately if this kind of contribution is unwelcome in this project. Commits carry an `Assisted-by: Claude Code` trailer.
