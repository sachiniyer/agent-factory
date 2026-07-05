# File-length lint (#1145)

A structural-health guard that keeps Go source files from silently growing
into 2000-line monsters. Large files are decomposition candidates and a
recurring source of merge pain and gotchas, so CI now bounds them.

## What it checks

`scripts/lint-file-length.sh` counts the **total physical lines** (`wc -l`) of
every tracked Go file and fails if a file is over its limit:

| File kind      | Limit       |
| -------------- | ----------- |
| production `.go` | 1000 lines |
| `*_test.go`      | 1500 lines |

Total lines — not non-blank/non-comment lines — is what a reader scrolls
through and what your editor's gutter shows; it needs no Go parsing and is
unambiguous. Test files get a higher ceiling because table-driven tests run
legitimately longer, but they're still bounded.

## The allowlist (grandfathering + ratchet)

Several files already exceeded these limits when the lint landed. They're
grandfathered in `scripts/file-length-allowlist.txt`, one `path ceiling` per
line, where `ceiling` is the file's line count at grandfathering time.

The ceiling is a **ratchet**: a grandfathered file may not grow past it, so
the big files can only shrink. When a file is decomposed back under the base
limit, remove its entry — the lint fails if a grandfathered file has already
dropped to/under the limit (a dead entry), so the allowlist can't rot.

Do **not** add new entries to dodge the limit. A brand-new file over the limit
should be split, not grandfathered.

The currently grandfathered files (the structural-health audit tracks
splitting them):

| File                       | Ceiling |
| -------------------------- | ------- |
| `session/backend_test.go`  | 2241    |
| `app/app.go`               | 1889    |
| `session/instance.go`      | 1531    |
| `session/tmux/tmux.go`     | 1424    |
| `ui/sidebar.go`            | 1258    |
| `config/config.go`         | 1128    |
| `daemon/control.go`        | 2604    |
| `daemon/watcher.go`        | 1046    |
| `session/backend_hook.go`  | 1026    |

## Running it

```bash
scripts/lint-file-length.sh      # or: make lint-file-length
```

It runs in CI as the **Check file lengths** step of the lint job in both
`.github/workflows/pr.yml` (PR gate) and `.github/workflows/lint.yml` (master
push).

## When CI fails on this

- **New file over the limit** → split it into focused files.
- **Grandfathered file grew past its ceiling** → you added to a file that's
  already too big; move the new code elsewhere or decompose the file.
- **Dead allowlist entry** → you shrank a file under its limit; delete its
  line from the allowlist.
