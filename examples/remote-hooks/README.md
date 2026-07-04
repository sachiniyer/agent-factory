# Remote Hooks — Skeleton Scripts

These are starter scripts for the Agent Factory remote hooks protocol. Copy them into your own repo or infrastructure tooling directory and fill in the `TODO` sections with your provisioning logic.

For the full protocol specification, see [docs/remote-hooks.md](../../docs/remote-hooks.md).

## Scripts

| Script | Purpose |
|---|---|
| `launch.sh` | Start a new remote agent session |
| `list.sh` | List all running remote sessions |
| `attach.sh` | Attach interactively to a session |
| `delete.sh` | Tear down a session and clean up |
| `terminal.sh` | Open an interactive shell in a session's workspace (optional — powers the Terminal tab) |

## Quick start

```bash
mkdir -p .agent-factory/hooks
cp examples/remote-hooks/*.sh .agent-factory/hooks/
chmod +x .agent-factory/hooks/*.sh
# Edit each script to add your infrastructure logic
```

Then configure your repo to use them in `<repo-root>/.agent-factory/config.toml`:

```toml
[remote_hooks]
launch_cmd = "./.agent-factory/hooks/launch.sh"
list_cmd = "./.agent-factory/hooks/list.sh"
attach_cmd = "./.agent-factory/hooks/attach.sh"
delete_cmd = "./.agent-factory/hooks/delete.sh"
terminal_cmd = "./.agent-factory/hooks/terminal.sh"
```
