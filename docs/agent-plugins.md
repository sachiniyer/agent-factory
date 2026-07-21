# Agent plugins

`af` teaches every agent it launches how to drive the `af` CLI. Agent plugins
hand that same knowledge to an agent `af` did **not** launch — a Codex or Claude
Code session you started yourself, in any repo — so you can say "run this in a
background af session" and it knows what to do.

There is one skill, `agent-factory`, packaged once per agent. Installing it is a
pull: you run an install command, and nothing is written into your agent's
configuration until you do.

## Install `af` first

A plugin cannot ship a native binary, and none of these packagings install one:

```
curl -fsSL https://raw.githubusercontent.com/sachiniyer/agent-factory/master/install.sh | sh
```

The plugin checks for `af` and tells you this if it is missing. It never
downloads or runs an installer itself — see [what the hook does
not do](#the-preflight-hook) below.

## Codex

```
codex plugin marketplace add sachiniyer/agent-factory
codex plugin add agent-factory@agent-factory
```

The first command registers this repo as a plugin marketplace; the second
installs the plugin from it. Codex copies the manifest, the skill, and the hook
into `$CODEX_HOME/plugins/cache/agent-factory/agent-factory/<version>/`.

Codex does not trust a plugin's hooks just because the plugin is installed — it
asks you to review them first. Declining the hook is fine: the skill, which is
the whole point, works either way.

## Claude Code

```
claude plugin marketplace add sachiniyer/agent-factory
claude plugin install agent-factory@agent-factory
```

Note that this is a *separate* mechanism from the plugin directory `af` hands
its own Claude sessions (`--plugin-dir`, written under af's config dir on every
launch). Both carry the same usage text. If you have both, Claude sees the
guidance twice — harmless, just slightly redundant.

## Gemini CLI

Gemini installs a skill straight out of the repo:

```
gemini skills install https://github.com/sachiniyer/agent-factory --path plugins/gemini/agent-factory
```

Or copy `plugins/gemini/agent-factory/` into `~/.gemini/skills/` yourself.

## amp

Amp has no skill add/install subcommand or marketplace. Clone the repo, then
copy the skill into its documented project skill directory:

```
git clone https://github.com/sachiniyer/agent-factory
mkdir -p .agents/skills
cp -R agent-factory/plugins/amp/agent-factory .agents/skills/
```

For a user-wide install, copy it into `~/.config/agents/skills/` instead. Amp
also still discovers the legacy `~/.config/amp/skills/` directory that `af`
writes when `global_agent_skills` is on.

## The preflight hook

The Codex plugin ships one optional `SessionStart` hook. It reports whether `af`
is on your `PATH` and, if not, prints the install command. That is all it does.

It deliberately does not fetch or install anything. A plugin hook runs as you,
with your permissions, and af's releases carry no checksum or signature to
verify against — `install.sh` and `af upgrade` both check only that the download
is a well-formed tarball. Downloading and executing a binary from inside an
agent session, on the strength of nothing, is the wrong shape however convenient
it would be. Detect and instruct instead.

## Relationship to `global_agent_skills`

`af` can also *push* this skill into an agent's global config
(`~/.codex/skills`, `~/.gemini/skills`, `~/.config/amp/skills`) when it launches
a session, behind the `global_agent_skills` opt-in, which defaults to off. That
is the same text by a different route. If you install the plugin you do not need
the push, and you can leave `global_agent_skills` off — the plugin reaches
sessions `af` never launched, which the push never did.

## What is not here

- **No MCP server.** `af` exposes no MCP tools today. A plugin manifest can gain
  an `mcpServers` pointer later without changing any install command above.
- **No official directory listing.** OpenAI's Plugins Directory requires a
  hosted MCP server on a public production URL; af's control plane is a local
  daemon on a Unix socket, and should stay one. This repo's marketplace is the
  channel.
- **No Gemini CLI extension.** Gemini's extension packaging is a second,
  unverified way to ship the same text, so the skill is what ships. Tracked as a
  follow-up.

## For maintainers

Everything under `plugins/`, plus `.agents/plugins/marketplace.json` and
`.claude-plugin/marketplace.json`, is **generated**. The source is
`afUsageReference` in `session/systemprompt.go` — the one text every af surface
teaches. `commands/plugins_gen.go` reframes it for a plugin audience (the
canonical text opens "You are running inside Agent Factory (af)", which is false
for a plugin user) and emits each agent's artifacts from that single body.

Change the guidance in `session/systemprompt.go`, then:

```
scripts/gen-docs.sh
```

and commit the result. A hand-edit to any generated artifact is reverted by the
next run and fails both `go test ./commands/...` and the CI drift gate in
`.github/workflows/docs.yml`. Adding an agent is a new entry in `pluginAgents`.
