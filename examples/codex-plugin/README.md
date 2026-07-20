# Codex plugin (exploration example)

**This is an exploration artifact, not a product surface.** Nothing here is wired
into the build, the release, or any `af` command. It exists to prove the shape of
a Codex plugin that hands the `af` CLI to a Codex session, so a maintainer can
decide whether to ship one. See the exploration issue for the write-up.

## What it is

A minimal Codex plugin — a manifest, one skill, and one lifecycle hook — plus a
local marketplace that serves it.

```
examples/codex-plugin/
├── .agents/plugins/marketplace.json   the marketplace that offers the plugin
└── plugin/
    ├── .codex-plugin/plugin.json      the plugin manifest
    ├── skills/agent-factory/SKILL.md  teaches Codex to drive the af CLI
    └── hooks/
        ├── hooks.json                 SessionStart hook declaration
        └── af-preflight.sh            reports whether af is on PATH
```

## Try it

The plugin installs into `$CODEX_HOME`, so point that at a throwaway directory
unless you actually want it in your own Codex config:

```bash
export CODEX_HOME=$(mktemp -d)
codex plugin marketplace add ./examples/codex-plugin
codex plugin add agent-factory@agent-factory-example
```

Verified against `codex-cli 0.144.6`: the plugin installs to
`$CODEX_HOME/plugins/cache/agent-factory-example/agent-factory/0.1.0/` with the
manifest, skill, and hooks all present.

## What it does not do

- **It does not install the `af` binary.** A Codex plugin has no declarative way
  to ship or depend on a native binary; the only executable seam is hooks. The
  hook here therefore *detects* `af` and prints install instructions rather than
  downloading a release asset — a plugin hook runs as the user before the user
  has any reason to trust it, so fetch-and-execute is the wrong shape.
- **It exposes no MCP tools.** `af` has no MCP server today. The manifest could
  gain `"mcpServers": "./.mcp.json"` later without changing how users install
  the plugin.
- **The skill duplicates `afUsageReference`.** The canonical af usage text lives
  in `session/systemprompt.go` and is delivered to agents af itself launches.
  This SKILL.md is a hand-written variant addressed to a Codex session that is
  *not* running inside af. Shipping for real means generating it from the
  canonical text so the two cannot drift.
