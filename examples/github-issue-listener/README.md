# GitHub Issue Listener

A small polling script that watches a GitHub repo for newly opened issues and spawns an Agent Factory session for each one. Each session is named `issue-<number>` and seeded with the issue's title and body as its prompt.

## Prerequisites

- `gh` authenticated against the target repo (`gh auth status` should report you as logged in).
- `jq` on `PATH`.
- `af` on `PATH` (`./dev-install.sh` from the repo root).

## Quick start

```bash
export REPO=owner/name           # required
export POLL_INTERVAL=60          # optional
export LABEL=agent-factory       # optional — only spawn for issues with this label
./listen.sh
```

The first run records the current UTC timestamp in the state file and only reacts to issues opened after that. Historical issues are not backfilled.

## Configuration

| Var | Default | Meaning |
|---|---|---|
| `REPO` | — (required) | Repo to watch, in `owner/name` form. |
| `POLL_INTERVAL` | `60` | Seconds to sleep between polls. |
| `STATE_FILE` | `$HOME/.agent-factory/issue-listener-state` | Cursor file. Holds the UTC timestamp of the last poll. Delete it to reset. |
| `LABEL` | unset | If set, only issues carrying this label spawn a session. |

## Run it persistently

The project's `task/` directory has fuller systemd / launchd integration for scheduled jobs; the snippets below are standalone and have no dependency on it.

### Linux — systemd user unit

`~/.config/systemd/user/af-issue-listener.service`:

```ini
[Unit]
Description=Agent Factory GitHub issue listener
After=network-online.target

[Service]
Type=simple
Environment=REPO=owner/name
Environment=LABEL=agent-factory
ExecStart=%h/path/to/listen.sh
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now af-issue-listener.service
journalctl --user -u af-issue-listener -f
```

### macOS — launchd plist

`~/Library/LaunchAgents/dev.agent-factory.issue-listener.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>dev.agent-factory.issue-listener</string>
    <key>ProgramArguments</key>
    <array>
        <string>/Users/you/path/to/listen.sh</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>REPO</key><string>owner/name</string>
        <key>LABEL</key><string>agent-factory</string>
    </dict>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>/tmp/af-issue-listener.out</string>
    <key>StandardErrorPath</key><string>/tmp/af-issue-listener.err</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/dev.agent-factory.issue-listener.plist
```

## Caveats

- **Cost.** Every new issue spawns a session, which runs your configured AI agent against your account. Set `LABEL` to opt in selectively rather than dispatching on every issue.
- **Idempotence.** If a session named `issue-<n>` already exists, `af sessions create` fails and the script logs and continues. Re-running on a repo with existing matching session names will not double-spawn.
- **Trust.** Anyone who can open an issue on the watched repo can effectively dispatch a session on your machine. Don't point this at repos with untrusted issue authors unless you're comfortable with that.
- **Closed/triaged issues.** The script only reacts to *newly opened* issues — those whose `created_at` is after the cursor in the state file. Existing open issues are never picked up, even after a restart. Delete the state file to reset the cursor (this will pick up any issues opened after the new initial timestamp, not historical ones).
