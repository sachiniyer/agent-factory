---
name: tui-playtest
description: Daily dogfood play-test — build af from master, drive the real TUI through a small project in a fully isolated sandbox, and file new findings as GitHub issues
user_invocable: true
---

# Daily TUI Play-Test (#1102)

Play-test the real `af` TUI by doing a small, realistic project end-to-end,
the way a new user would. The goal is *feel*: UX friction, confusing states,
and visual glitches count just as much as hard bugs. At the end, file each
NEW distinct finding as a GitHub issue and emit a run summary.

You are driving a live TUI. Interact with it through a private tmux server
using `send-keys` / `capture-pane`; read the screen after every action and
judge what a human would think of it.

## Hard isolation rules (non-negotiable)

These rules exist because a previous play-test took down the whole dev box
(2026-07-03 outage — orphaned infinite `yes` processes). Violating any of
them is a failed run, regardless of what else you find.

- Use a throwaway **MOCK git repo under /tmp** — never a real repo, and
  never this repo.
- Use an isolated **AGENT_FACTORY_HOME** so config, state, logs, and the
  daemon socket never touch the real `~/.agent-factory`.
- Use a **private tmux server** (`tmux -L <unique socket>`) — never the
  default server. Also export `TMUX_TMPDIR` to a private dir and clear
  `$TMUX`: tmux resolves `$TMUX` *before* `TMUX_TMPDIR`, so a run started
  from inside a real tmux pane would otherwise leak onto the real server.
- Instances run **cheap programs (`bash`), not real agents**, except where
  testing agent flows genuinely requires a real agent.
- Any stress or fast-output generator must be **BOUNDED**:
  `timeout 5 yes > /dev/null` or `head -c 1M </dev/zero` — **never bare
  `yes`, never an infinite loop**.
- **Cap concurrency**: one build at a time, one TUI at a time, at most 3
  instances alive at once.
- **TEARDOWN at the end, even on failure**: kill the private tmux server,
  kill every spawned PID (including the sandbox daemon), pgrep-verify that
  nothing survived, remove the temp dirs, and REPORT the teardown
  verification in the run summary.
- Never run a broad kill (`pkill af`, `pkill tmux`, `tmux kill-server` on
  the default server, `pgrep`-kill by bare program name). The dev box runs
  a real `af` daemon and real tmux sessions; scope every kill and every
  pgrep to the sandbox path or to PIDs you recorded yourself.

## 1. Setup

Write the teardown script FIRST (section 4) so cleanup works even if the
run wedges, then build the sandbox:

```bash
RUN_ID="$(date +%Y%m%d-%H%M%S)"
WORK="/tmp/af-playtest-$RUN_ID"        # keep short: unix socket paths cap at ~104 bytes
mkdir -p "$WORK/home" "$WORK/tmux"

export AGENT_FACTORY_HOME="$WORK/home"
export TMUX_TMPDIR="$WORK/tmux"
export TMUX=""                          # $TMUX wins over TMUX_TMPDIR; must be cleared
SOCK="pt-$RUN_ID"                       # private tmux socket name; use tmux -L "$SOCK" everywhere
```

**Build current master** (fresh clone so a dirty checkout can't skew results):

```bash
git clone --depth 1 https://github.com/sachiniyer/agent-factory "$WORK/src"
cd "$WORK/src" && go build -o "$WORK/af" .
AF="$WORK/af"
```

**Mock project repo** — a small but real one, so worktrees/diffs have
something to show:

```bash
mkdir "$WORK/mock-repo" && cd "$WORK/mock-repo"
git init -b master
git config user.name "AF Playtest" && git config user.email "playtest@example.com"
# seed a tiny project: e.g. a shell todo app — a script, a README, a test file
git add -A && git commit -m "initial project"
```

**Cheap instances** — write `$AGENT_FACTORY_HOME/config.json` before first
launch so instances run bash instead of a real agent
(`default_program` must be a supported agent name; the override supplies
the actual command):

```json
{ "default_program": "claude", "program_overrides": { "claude": "bash" } }
```

**Verify isolation before launching anything**: `"$AF" debug` must print
paths under `$WORK` only. If it prints anything under the real home, STOP
and fix the environment first.

**Start the driver session and launch the TUI at 80x24**:

```bash
tmux -L "$SOCK" new-session -d -s drive -x 80 -y 24
tmux -L "$SOCK" send-keys -t drive "cd $WORK/mock-repo && $AF" Enter
sleep 2
tmux -L "$SOCK" capture-pane -p -t drive   # read the screen
```

Record every PID you spawn (`$WORK/pids.txt`). The sandbox daemon `af`
auto-spawns writes its PID to `$AGENT_FACTORY_HOME/daemon.pid` — that file
is part of teardown.

## 2. Play-test script

Do a small real project through the TUI — e.g. "build a shell todo app":
create instances for subtasks, do the work in the bash instance panes
(write files, run bounded commands, commit), and manage it all through the
TUI. After every keypress: `capture-pane`, read the screen, and ask "does
this feel right?" Note anything that feels wrong — slow feedback, unclear
labels, surprising focus, stale content, ugly truncation — not just crashes.

Exercise at least these flows (keys from `keys/keys.go`; press `?` in-app
to cross-check the help view against reality):

1. **Create instances** — `n`, name it, submit. Create 2–3 (respect the
   cap). Check the sidebar list, statuses, and the worktree actually
   existing on disk.
2. **Attach & drive** — `enter`/`o` to open, `tab`/`shift+tab` to move
   focus, type real shell work into the instance pane, detach. Does focus
   go where you expect? Is it obvious how to get back out?
3. **Tabs** — `t` new tab, `1`–`9` jump, `w` close. Also
   `"$AF" sessions tab-create` from the CLI and confirm the TUI reflects it.
4. **Panes** — `s` split, `s` swap, `x` close split.
5. **Search** — `/`, find an instance by name, clear the search. Try a
   query with no matches.
6. **Task flows** — create a task via `"$AF" tasks` (a bounded script,
   e.g. `echo hello`), open the task list with `S`, trigger with `r`,
   check the output landed. Manage automations from the rail.
7. **Navigation & help** — `[`/`]` sections, `h`/`l` collapse/expand,
   `?` help overlay, scroll with `shift+up/down`.
8. **Resize** — `tmux -L "$SOCK" resize-window -t drive -x 80 -y 24`,
   then smaller (`-x 60 -y 20`), then large (`-x 200 -y 50`). Look for
   overlap, truncation, panics, layout garbage at every size.
9. **Kill & quit** — `D` a session (confirm the worktree is cleaned up),
   `q` to quit, relaunch, and confirm surviving sessions persist and
   reattach.
10. **CLI parity** — `"$AF" sessions list/get/preview/send-prompt/kill`
    against the same sandbox; confirm CLI and TUI agree.

Things that don't apply in the sandbox (e.g. `p` open-PR with no GitHub
remote) are still worth one press: a graceful, well-worded error is a pass;
a panic or silent nothing is a finding.

Keep a running findings log (`$WORK/findings.md`): one line per
observation, with the exact repro (keys pressed, screen state, expected vs
actual) and a severity guess.

## 3. File findings as GitHub issues

For each distinct finding, **search before filing**:

```bash
gh issue list --repo sachiniyer/agent-factory --state open --search "<keywords>"
```

- Already tracked → skip it (optionally comment a fresh repro if you have a
  better one).
- New → file ONE issue per distinct finding: clear title, exact repro
  steps, expected vs actual, screen capture text where useful, and label
  suggestions (e.g. `bug`, `tui`, plus a UX/severity hint in the body).
  Mention it came from the daily TUI play-test and sign as **Captain
  Claude**.
- Nothing new → file nothing. The run summary is the only output.

## 4. Teardown (mandatory, even on failure)

Write this as `$WORK/teardown.sh` during setup and run it at the end — and
also if the run aborts partway. The script must be self-contained: bake the
sandbox values in **EXPANDED at write time** (unquoted heredoc for the
header), so it works even when run from a fresh shell with no environment.
An unexpanded/empty `$WORK` would make `pgrep -af "$WORK"` match EVERY
process on the box and turn the kill step into exactly the accident these
rules exist to prevent — so the script also fails closed if its paths look
wrong:

```bash
# header: UNQUOTED heredoc — $WORK/$SOCK/$AGENT_FACTORY_HOME expand NOW
cat > "$WORK/teardown.sh" <<EOF
#!/bin/bash
# scoped teardown — values baked in at write time; touches ONLY this sandbox
WORK="$WORK"
SOCK="$SOCK"
AGENT_FACTORY_HOME="$AGENT_FACTORY_HOME"
EOF
# body: QUOTED heredoc — runs against the baked-in values above
cat >> "$WORK/teardown.sh" <<'EOF'
# fail closed: refuse to kill or rm anything if the paths are empty or unexpected
: "${WORK:?}" "${SOCK:?}" "${AGENT_FACTORY_HOME:?}"
case "$WORK" in /tmp/af-playtest-*) ;; *) echo "refusing: WORK=$WORK"; exit 1;; esac
case "$AGENT_FACTORY_HOME" in "$WORK"/*) ;; *) echo "refusing: AGENT_FACTORY_HOME=$AGENT_FACTORY_HOME"; exit 1;; esac

tmux -L "$SOCK" kill-server 2>/dev/null            # private server only
[ -f "$AGENT_FACTORY_HOME/daemon.pid" ] && kill "$(cat "$AGENT_FACTORY_HOME/daemon.pid")" 2>/dev/null
while read -r pid; do kill "$pid" 2>/dev/null; done < "$WORK/pids.txt"
sleep 2
# verify: pgrep scoped to the sandbox path — must print nothing.
# (grep -v filters this script's own shell, whose argv contains $WORK.)
if pgrep -af "$WORK" | grep -v teardown.sh; then
  echo "TEARDOWN INCOMPLETE — kill -9 those exact PIDs, re-verify; do not just rm"
else
  echo "teardown clean: no surviving processes"
fi
rm -rf "$WORK"
EOF
chmod +x "$WORK/teardown.sh"
```

If `pgrep -af "$WORK"` still shows survivors, `kill -9` those exact PIDs,
re-verify, and say so in the summary. Never widen the pgrep pattern beyond
`$WORK`.

## 5. Run summary

End the run with a summary containing:

- **Exercised**: which flows from section 2 ran, at which terminal sizes.
- **Findings**: every observation, each marked *filed* (with issue URL),
  *duplicate of #N*, or *not issue-worthy* (and why).
- **Teardown proof**: the verbatim output of the teardown verification
  (the `pgrep` check and the final "teardown clean" line).
- **Overall feel**: one paragraph — would a new user enjoy this today?
