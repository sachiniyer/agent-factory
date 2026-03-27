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

## Quick start

```bash
cp examples/remote-hooks/*.sh /path/to/your/hooks/
chmod +x /path/to/your/hooks/*.sh
# Edit each script to add your infrastructure logic
```

Then configure your repo to use them:

```json
{
  "remote_hooks": {
    "launch_cmd": "/path/to/your/hooks/launch.sh",
    "list_cmd": "/path/to/your/hooks/list.sh",
    "attach_cmd": "/path/to/your/hooks/attach.sh",
    "delete_cmd": "/path/to/your/hooks/delete.sh"
  }
}
```
