# Comparison

The agent-orchestration space is splitting into several shapes: terminal
multiplexers, desktop worktree apps, kanban-style planning boards, cloud coding
agents, built-in agent dashboards, and plain shell workflows. Agent Factory sits
in a specific lane: **terminal-native, git-worktree-first, daemon-supervised,
and scriptable**.

This page is a map of tradeoffs, not a scorecard. The best choice depends on
where you want the center of gravity to live: your terminal, a desktop app, a
team board, a hosted cloud agent, or a manual shell setup.

## Big Picture

| Tool family | Examples | Best center of gravity | Agent Factory contrast |
|---|---|---|---|
| **Agent Factory** | `af` | One task becomes one branch/worktree, supervised from a TUI, CLI, daemon, and local API. | Git review, cron/watch automation, remote hooks, usage-limit recovery, and recoverable lifecycle are the core product. Supports Claude Code, Codex, Aider, Gemini, and Amp through named agent choices. |
| **Agent terminal multiplexers** | Herdr, Agent Deck, Claude Squad | Keep many live terminal agents visible, switchable, and persistent. | Agent Factory is more opinionated about repo-scoped sessions, worktree ownership, task scheduling, and archive/restore semantics. |
| **Desktop worktree apps** | Conductor, Superset, Nimbalyst, Crystal | Visual workspace management, built-in diff review, and local parallel branches. | Agent Factory keeps the primary interface in the terminal and hands review back to normal git/PR tools. |
| **Kanban/review boards** | Vibe Kanban | Plan, dispatch, review, and track many agent tasks as team work items. | Agent Factory is lighter and repo-local, with cron/watch automation instead of a shared planning board. |
| **Built-in agent dashboards** | Claude Code Agent View, Codex in ChatGPT | Native multi-session surfaces for one agent ecosystem. | Agent Factory is agent-agnostic across Claude Code, Codex, Aider, Gemini, and Amp, with one lifecycle around all of them. |
| **Cloud coding agents** | Codex cloud, Copilot cloud agent, Cursor Cloud Agents, Jules, Devin | Delegate work to hosted environments and return for review or PRs. | Agent Factory keeps work local by default and gives you direct terminal/process control. |
| **Single-agent CLIs** | Claude Code, Codex CLI, OpenCode, Aider, Gemini CLI, Amp, Crush | One focused coding loop in a terminal or editor. | Agent Factory runs and supervises those tools as fleet members rather than replacing their agent loop. |
| **Manual stack** | tmux/Zellij, git worktree, cron, shell scripts | Maximum control with the fewest product assumptions. | Flexible but self-managed: naming, scheduling, cleanup, restore, and safety checks are yours. |

## Tool-By-Tool Table

| Tool | Category | Primary interface | Isolation and review model | Best fit | Agent Factory contrast |
|---|---|---|---|---|---|
| **Agent Factory** | Terminal orchestration | TUI, JSON CLI, local HTTP API | One normal session creates one branch and git worktree; archive/restore/kill are explicit. | Local multi-agent repo work where reviewable branches and automation matter. | Baseline. |
| **Herdr** | Agent terminal multiplexer | Terminal panes, mouse/keyboard UI, CLI/socket API | Persistent real PTYs and agent state; worktree workflow can sit around the terminal runtime. | People who want a true terminal multiplexer redesigned around agents. | Herdr is stronger as a general live terminal runtime; Agent Factory is stronger as a repo/worktree/task lifecycle manager. |
| **Agent Deck** | Agent terminal manager | Terminal dashboard | Groups, search, forking, worktrees, cost tracking, and phone-controlled conductor workflows. | Managing many terminal-agent sessions across projects. | Agent Deck is broader fleet management; Agent Factory is narrower around repo-scoped daemon state, tasks, and branch lifecycle. |
| **Claude Squad** | Terminal worktree manager | TUI | Multiple AI terminal agents in isolated git workspaces with review/check-out flows. | Lightweight terminal management for local agents. | Agent Factory is a fork that adds per-repo scoping, daemon-owned state, tasks, remote hooks, JSON CLI, HTTP API, archive/restore, and generated docs. |
| **Conductor** | Desktop worktree app | macOS desktop app | Each task gets a workspace, branch, files, terminal, diff, and review path. | Visual local workflow for parallel Claude Code, Codex, Cursor, and OpenCode work. | Conductor has a stronger visual review surface; Agent Factory stays terminal-native and scriptable. |
| **Superset** | Desktop/workspace app | Desktop app | Runs many coding agents in parallel and emphasizes ready-for-review diffs. | Users who want visual parallel execution and review queues on one machine. | Superset is more GUI/review-board oriented; Agent Factory is CLI/API/daemon oriented. |
| **Nimbalyst** | Visual agent workspace | Desktop/web-style visual workspace | Kanban, worktrees, inline AI diffs, and Claude Code/Codex workflows. | Teams or individuals wanting an integrated visual workspace around agent output. | Nimbalyst is a broader visual environment; Agent Factory is a small terminal control plane. |
| **Crystal** | Desktop worktree app | Electron desktop app | Parallel Claude Code/Codex sessions against git worktrees. | People who liked the original Crystal multi-session local workflow. | Crystal is now positioned as Nimbalyst's predecessor; Agent Factory keeps the terminal-first version of this pattern. |
| **Vibe Kanban** | Kanban/review board | Web app/project board | Issues become workspaces where supported agents execute; status and PRs feed back into the board. | Planning and reviewing many agent tasks, especially in a team setting. | Vibe Kanban owns planning/review; Agent Factory owns local session/process/worktree lifecycle. |
| **Claude Code Agent View** | Built-in agent dashboard | Claude Code native view | Background sessions use isolated `.claude/worktrees/` before editing files. | Claude Code users who want native multi-session status and background work. | Agent View is excellent inside Claude's ecosystem; Agent Factory wraps several agent CLIs and exposes daemon/CLI/API automation. |
| **Claude Code worktrees** | Manual/native agent workflow | Claude Code CLI/docs workflow | You create or rely on worktrees for parallel Claude file isolation. | Claude-only users comfortable managing terminals and git workflow directly. | Agent Factory automates session creation, status, restore, archive, tasks, and cross-agent support. |
| **Codex in ChatGPT / Codex cloud** | Hosted coding agent | ChatGPT/Codex web, cloud environments | Built-in worktrees/cloud environments; background parallel tasks; review diff or open PR. | Delegating longer work to hosted environments without tying up a local machine. | Codex cloud is stronger for hosted background delegation; Agent Factory is local, terminal-first, and agent-agnostic. |
| **Codex CLI** | Single-agent CLI | Local terminal TUI/CLI | Works against the selected local repository; can be scripted with `codex exec`. | A focused local OpenAI coding loop. | Agent Factory can launch Codex sessions in isolated worktrees and supervise many at once. |
| **GitHub Copilot cloud agent** | Hosted code agent | GitHub issues, agents tab, dashboard, Copilot Chat | Runs autonomously in a GitHub Actions-powered environment, makes branch changes, and can open PRs. | GitHub-centered teams who want issue-to-PR delegation. | Copilot cloud agent is hosted and GitHub-native; Agent Factory is local/process-native and works across agent CLIs. |
| **Cursor Cloud Agents** | Hosted/editor-adjacent agent | Cursor cloud/editor workflow | Runs Cursor Agent in cloud environments for continuous assistance. | Cursor users who want background cloud tasks tied to their editor workflow. | Cursor owns the editor/cloud experience; Agent Factory owns a terminal daemon and local worktrees. |
| **Google Jules** | Hosted coding agent | Web, GitHub, CLI/API | Clones a repo into a Cloud VM, develops a plan, and can work asynchronously. | GitHub-connected cloud coding tasks with Google/Gemini infrastructure. | Jules is hosted and task-oriented; Agent Factory keeps the working processes on your local or hook-defined backend. |
| **Devin** | Autonomous cloud engineer | Devin web app, integrations, API | Cloud-based engineer that can write, run, test code, create PRs, and participate in review workflows. | Larger delegated engineering tasks and review workflows managed in Devin. | Devin is an autonomous hosted teammate; Agent Factory is a local supervisor for agent CLIs you already run. |
| **OpenCode** | Single-agent CLI/editor/desktop agent | Terminal, IDE, desktop | Open source coding agent with multi-session support and session sharing. | Users who want an open source agent loop across terminal/editor/desktop. | OpenCode is the agent; Agent Factory can make many OpenCode-like sessions observable and isolated. |
| **Aider** | Single-agent CLI pair programmer | Terminal REPL | Edits local git repos directly and supports many model providers. | Model-flexible terminal pair programming, especially in existing git repos. | Aider is one excellent worker; Agent Factory gives many workers isolated worktrees and shared lifecycle. |
| **Gemini CLI** | Single-agent CLI | Terminal | Open source Gemini-powered ReAct loop over local tools and MCP servers. | Terminal users in the Google/Gemini ecosystem. | Agent Factory can run Gemini CLI as one session type but adds worktree/session/task orchestration around it. |
| **Amp** | Single-agent terminal/editor agent | Terminal and editor | Frontier coding agent with threads and multi-model behavior. | People who want a polished, fast-moving agent CLI/editor loop. | Amp is the coding agent; Agent Factory is the terminal control plane around many agent processes. |
| **Crush** | Single-agent terminal agent | Terminal TUI | Open source Charm terminal coding agent wired into local tools and LLMs. | Users who like a Charm-style terminal UX for one agent session. | Crush improves one agent loop; Agent Factory organizes multiple loops and branches. |
| **tmux/Zellij + manual worktrees** | Manual terminal stack | Terminal multiplexer and shell | You create worktrees, panes, prompts, logs, cleanup, and review paths yourself. | Teams with strong shell discipline and custom scripts. | Agent Factory packages the common policy: repo-scoped sessions, daemon state, scheduling, JSON output, and safe cleanup. |
| **Plain git worktrees + scripts** | Manual git workflow | Shell scripts, cron, CI | Worktrees provide isolation; scripts provide launch/schedule/cleanup if you build them. | Maximum control and minimal dependency surface. | Agent Factory is the prebuilt version of the operational scaffolding. |

## Capability Matrix

| Capability | Agent Factory | Herdr | Desktop worktree apps | Kanban/review boards | Built-in dashboards | Cloud agents | Manual stack |
|---|---|---|---|---|---|---|---|
| Local terminal-first workflow | Yes | Yes | Partial | No | Depends | No | Yes |
| Worktree isolation by default | Yes | Varies | Usually | Usually workspace-backed | Yes for some surfaces | Hosted branch/env | Manual |
| Many agents at once | Yes | Yes | Yes | Yes | Ecosystem-specific | Yes | Manual |
| Persistent session supervision | Daemon-owned | Runtime/server-owned | App-owned | App-owned | Product-owned | Cloud-owned | tmux/scripts |
| Built-in scheduler/watch automation | Yes | API/scriptable | Varies | Board-triggered | Varies | Integrations | cron/scripts |
| JSON CLI/API surface | CLI + local HTTP | CLI/socket | Varies | Varies | Varies | Product API | Whatever you build |
| Built-in diff/review UI | No, use git/PR tools | No, terminal-first | Usually | Usually | Usually | Usually | No |
| Remote/cloud execution | Remote hooks | SSH/remote attach | Varies | Often hosted | Product-specific | Native | SSH/manual |
| Archive/restore session lifecycle | Yes | Persistence-focused | Varies | Workspace lifecycle | Product-specific | Product-specific | Manual |

## When To Choose Agent Factory

Choose Agent Factory when your desired unit of work is:

```text
prompt -> session -> branch/worktree -> visible agent -> git review -> archive or merge
```

It is a good fit when you want:

- a terminal-first workflow instead of a desktop or browser app;
- every normal session isolated by git worktree by default;
- a daemon-owned state model shared by the TUI, CLI, and API;
- built-in cron/watch automations;
- explicit archive, restore, kill, and usage-limit recovery behavior;
- local and remote sessions under one repo-scoped view.

## When To Choose Something Else

Choose **Herdr** or **Agent Deck** when the live terminal workspace itself is
the product: persistent panes, fast switching, agent state, direct attach, and a
general control surface for terminal processes.

Choose **Conductor**, **Superset**, **Nimbalyst**, or **Crystal-style tools**
when you want a visual app around parallel workspaces, inline diffs, and review
queues.

Choose **Vibe Kanban** when the primary pain is planning, assigning, reviewing,
and tracking agent work as team-visible issues.

Choose **Claude Code Agent View** or **Codex in ChatGPT** when you want the
native dashboard inside one agent ecosystem.

Choose **Codex cloud**, **GitHub Copilot cloud agent**, **Cursor Cloud Agents**,
**Jules**, or **Devin** when you want hosted background execution and a
review/PR handoff instead of local terminal control.

Choose a **single-agent CLI** such as Claude Code, Codex CLI, OpenCode, Aider,
Gemini CLI, Amp, or Crush when you only need one focused agent loop.

Choose **tmux/Zellij + manual worktrees** when you want the fewest moving parts
and are comfortable owning all lifecycle policy yourself.

## Source Notes

- Agent Factory behavior is documented in [Worktree-isolated agents](concepts/worktree-agents.md),
  [The daemon](concepts/daemon.md), [Tasks & automation](tasks.md),
  [Remote hooks](remote-hooks.md), the [CLI guide](cli.md), and the generated
  [CLI reference](reference/cli.md).
- Terminal/orchestration tools: [Herdr](https://herdr.dev/),
  [Herdr compare](https://herdr.dev/compare/),
  [Agent Deck](https://github.com/asheshgoplani/agent-deck),
  [Claude Squad](https://github.com/smtg-ai/claude-squad), and
  [claude-worktree](https://github.com/bucket-robotics/claude-worktree).
- Desktop/workspace tools: [Conductor docs](https://www.conductor.build/docs),
  [Superset](https://superset.sh/), [Nimbalyst](https://nimbalyst.com/),
  [Nimbalyst on Crystal](https://nimbalyst.com/crystal/), and
  [Crystal](https://github.com/stravu/crystal).
- Planning/review tools: [Vibe Kanban](https://vibekanban.com/),
  [Vibe Kanban docs](https://vibekanban.com/docs), and
  [supported agents](https://vibekanban.com/docs/supported-coding-agents).
- Built-in or hosted agent surfaces: [Claude Code Agent View](https://code.claude.com/docs/en/agent-view),
  [Claude Code worktrees](https://code.claude.com/docs/en/worktrees),
  [Codex in ChatGPT](https://openai.com/codex/),
  [Codex cloud](https://developers.openai.com/codex/cloud),
  [GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent),
  [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent),
  [Google Jules](https://jules.google/docs/), and
  [Devin docs](https://docs.devin.ai/get-started/devin-intro).
- Single-agent CLIs: [Codex CLI](https://developers.openai.com/codex/cli),
  [OpenCode](https://opencode.ai/), [Aider](https://aider.chat/),
  [Gemini CLI](https://developers.google.com/gemini-code-assist/docs/gemini-cli),
  [Amp](https://ampcode.com/manual), and
  [Crush](https://github.com/charmbracelet/crush).
- tmux describes itself as a terminal multiplexer with detachable sessions, and
  git's isolation primitive is [git-worktree](https://git-scm.com/docs/git-worktree).
