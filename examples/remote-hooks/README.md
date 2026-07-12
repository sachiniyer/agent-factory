# Remote Hooks — Skeleton Scripts

Starter scripts for the Agent Factory remote-hook backend (provision-and-expose,
#1592 Phase 4 PR7). Copy them into your repo or infra tooling directory and fill
in the `TODO` sections with your provisioning logic.

For the full protocol specification and a ready-to-use reference `launch.sh`, see
[docs/remote-hooks.md](../../docs/remote-hooks.md).

## Scripts

| Script | Purpose |
|---|---|
| `launch.sh` | Provision the workspace, start an `af agent-server`, echo its `{url,token,tls_fingerprint}` |
| `delete.sh` | Tear the provisioned sandbox back down |

The old `list.sh` / `attach.sh` / `terminal.sh` are **gone**: enumeration, terminal
proxying, and preview capture are now served by the in-workspace `af agent-server`
over its `wss://` stream. The daemon drives a hook session exactly like a
docker/ssh one.

## Quick start

```bash
mkdir -p .agent-factory/hooks
cp examples/remote-hooks/*.sh .agent-factory/hooks/
chmod +x .agent-factory/hooks/*.sh
# Edit launch.sh (and delete.sh) to add your infrastructure logic
```

Then configure your repo in `<repo-root>/.agent-factory/config.toml`:

```toml
backend = "hook"

[remote_hooks]
launch_cmd = "./.agent-factory/hooks/launch.sh"
delete_cmd = "./.agent-factory/hooks/delete.sh"
```
