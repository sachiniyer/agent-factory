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
asks you to review their exact definitions first. The skill works if you decline
them, but the tmux teardown guard does not: use `/hooks` to trust the plugin's
current hooks if you want the protection described below. A plugin update that
changes a hook requires review again.

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

## Codex hooks

The Codex plugin ships two optional hooks:

- A `SessionStart` preflight reports whether `af` is on your `PATH` and, if not,
  prints the install command.
- A `PreToolUse` hook checks every Codex shell command with `af
  hook-guard-tmux`. It blocks commands the shared policy cannot prove safe,
  including a bare `tmux kill-server` and pattern-based process kills. A
  socket-scoped teardown such as `tmux -L test-socket kill-server` remains
  available. If the installed `af` helper is missing or fails, the hook blocks
  the command rather than silently dropping the guard.

The preflight deliberately does not fetch or install anything. A plugin hook runs as you,
with your permissions. Af verifies release checksums, but those checksums are
unsigned and arrive through the same release channel as the archive: they catch
corruption, not a compromised publisher or release channel. Downloading and
executing a binary from inside an agent session is the wrong shape however
convenient it would be. Detect and instruct instead.

## Tmux guard coverage

The guard is a safety layer, not a sandbox. Its policy is agent-neutral, but
each agent needs a blocking lifecycle seam that af actually delivers:

| Agent | Guarded by af | Delivery boundary |
| --- | --- | --- |
| Claude Code | Yes, for sessions launched by af | af injects its runtime plugin with `--plugin-dir` on every Claude launch. |
| Codex | Yes, when this plugin is installed, enabled, and its current hooks are trusted | The plugin's `PreToolUse` hook covers Codex shell and unified-exec calls. An af-launched Codex session is not guarded merely because af wrote its optional skill. |
| Gemini CLI | No | af ships a skill, not a blocking hook integration. |
| Amp | No | af ships a skill, not a blocking hook integration. |
| Aider | No | af injects read-only guidance with `--read`; it has no af-delivered blocking command hook. |
| OpenCode | No | af injects guidance through `OPENCODE_CONFIG`; it has no af-delivered blocking command hook. |

Codex can disable hooks globally, skip untrusted plugin hooks, or be configured
to accept managed hooks only. Those modes are outside this plugin's control and
leave the Codex session unguarded. Gemini, Amp, Aider, and OpenCode must be
treated as unguarded until af ships and verifies a blocking delivery seam for
each one.

Claude and Codex both fail closed on shell here-documents. Quoting the delimiter
prevents shell expansion, but it cannot prove whether the receiving program
treats the body as data or executable code. Use the agent's non-shell file tool
to create a reviewable literal file, then pass its literal path—for example,
`python3 /tmp/task.py` or `gh pr comment <number> --body-file /tmp/reply.md`.
The hook intentionally has no “trust this inline code” escape hatch because it
cannot verify review provenance from inside the same Bash request.

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
