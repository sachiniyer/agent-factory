#!/usr/bin/env bash
# web-demo-agent.sh — the scripted stand-in agent for the recorded web demo
# (scripts/container/web-demo-entry.sh, `make demo-assets`).
#
# The demo records the REAL web client against a REAL af daemon: real sessions,
# real git worktrees, real tabs, real scheduled tasks, the real config view. The
# one thing a recording cannot make real is the agent. Shelling out to Claude
# Code or Codex would need credentials in the sandbox, a network, and a model
# whose wording changes every run — so the video would be neither reproducible
# nor reviewable, and `make demo-assets` would stop being a build step.
#
# So this is a stand-in, and it is a stand-in on purpose:
#
#   * it NAMES ITSELF in its first line, so nobody watching the video takes it
#     for a vendor's agent;
#   * it does REAL work in the session's own AF-owned worktree — it edits the
#     files, runs the mock project's own ./test.sh, and leaves the changes
#     behind — so the diff the demo shows is a diff this script actually made,
#     not a rendered transcript;
#   * it is deterministic: the same lines, the same edits, the same test result,
#     every run.
#
# Which of the four scripts it plays is keyed off the worktree path, i.e. the
# session title — the same trick the web selftest's fake agent uses
# (web-selftest-entry.sh). Once the scripted work is done it reads its own TTY
# forever, so the pane stays live and typed input still echoes, like that fake
# agent's `cat` — see the loop at the bottom for why it is not literally `cat`.
set -uo pipefail

# Seconds between lines. Fast enough that seeding three sessions is not the
# long pole, slow enough that the recorded pane reads as work in progress
# rather than one instant paint.
STEP_DELAY="${AF_DEMO_STEP_DELAY:-0.35}"
# Where the entry script waits for "this session's scripted work is finished".
# Keyed by role, not by title, so the marker name cannot drift from the case
# below.
#
# The default is a PATH rather than an env var, and the write is guarded by
# "does that directory exist and is it writable" rather than by "was the
# variable set". An agent pane is started by the daemon, which starts it under
# tmux, so an env var exported next to the daemon has two process boundaries to
# survive; the directory is the entry script's own and is unambiguous. A
# hand-run outside the container finds no such directory and simply writes no
# marker.
MARKER_DIR="${AF_DEMO_MARKER_DIR:-/work/markers}"

# Everything printed so far, kept so the pane can be REDRAWN.
#
# This is not bookkeeping for its own sake. The scripted work runs while the
# session is being seeded, long before any browser attaches, so it is emitted
# into tmux's default 80-column pane and lands in scrollback already wrapped at
# 80. tmux does not reflow history, so without a redraw the recording shows an
# 80-column transcript sitting inside a much wider pane — an artifact of this
# script being a plain line printer, not of the product. A real agent is a
# full-screen program and repaints itself on SIGWINCH; so does this.
buffer=""

emit() {
    buffer="$buffer$1
"
    printf '%s\n' "$1"
}

# Records a line the TTY has ALREADY echoed (the prompt af types in), so a later
# redraw keeps it without printing it twice.
record() {
    buffer="$buffer$1
"
}

redraw() {
    printf '\033[2J\033[H%s' "$buffer"
}
trap redraw WINCH

say() {
    emit "$*"
    sleep "$STEP_DELAY"
}

# Replaces the FIRST line matching $2 in file $1 with the lines on stdin.
#
# Reading the replacement from stdin is what keeps the four edits below legible
# as the code they insert, rather than as a sed program with three levels of
# quoting. The final `cat >` rather than `mv` is deliberate: it preserves the
# target's mode, and todo.sh has to stay executable for ./test.sh to run.
#
# A miss EXITS rather than returning: this script runs without `set -e` (a failed
# ./test.sh has its own branch below), so a `return` here would be ignored by the
# caller and the recording would show a confident transcript over an edit that
# never happened. Exiting leaves no completion marker, and the entry script fails
# with this pane's contents in the log.
splice() {
    local file=$1 pattern=$2 tmp n
    n="$(grep -n -m1 -e "$pattern" "$file" | cut -d: -f1)"
    if [ -z "$n" ]; then
        echo "demo-agent: no line matching $pattern in $file" >&2
        exit 1
    fi
    tmp="$(mktemp)"
    head -n "$((n - 1))" "$file" >"$tmp"
    cat >>"$tmp"
    tail -n "+$((n + 1))" "$file" >>"$tmp"
    cat "$tmp" >"$file"
    rm -f "$tmp"
}

case "$PWD" in
    *add-json-export*) role=json ;;
    *fix-empty-add*) role=usage ;;
    *document-cli*) role=docs ;;
    *) role=tests ;;
esac

# The prompt each session was created with. Printed only if the real one never
# arrives (see below), so the pane always says what it is working on.
case "$role" in
    json) scripted_prompt="Add a json command to todo.sh that prints the items as a JSON array." ;;
    usage) scripted_prompt="Make ./todo.sh add with no text exit nonzero with a usage line, and cover it." ;;
    docs) scripted_prompt="Document list, add and done in README.md, accurately to todo.sh." ;;
    tests) scripted_prompt="Cover appending a second item, and print one line per case." ;;
esac

emit "demo-agent · a scripted stand-in, recorded for the docs · ${PWD##*/}"
emit ""

# af delivers the session's initial prompt by typing it into this pane once the
# workspace reports ready, so the honest way to show a prompt arriving is to
# read it off the terminal the way an agent would. The tty is in canonical mode
# with echo on, so the characters appear as they are typed and `read` returns
# the finished line.
#
# The timeout is a fallback, not a wait: if delivery is slow or a session was
# created with no prompt, the pane still says what this run is about instead of
# sitting blank for the whole recording.
prompt=""
if IFS= read -r -t 30 prompt && [ -n "$prompt" ]; then
    record "$prompt"
else
    prompt="$scripted_prompt"
    emit "$prompt"
fi
emit ""

case "$role" in
    json)
        say "· reading todo.sh"
        say "· editing todo.sh — a json case beside list, add and done"
        splice todo.sh '^  \*)' <<'EOF'
  json) sed 's/.*/"&"/' "$TODO_FILE" | paste -sd, - | sed 's/^/[/; s/$/]/' ;;
  *) nl -ba "$TODO_FILE" ;;
EOF
        say "· covering it in test.sh"
        cat >>test.sh <<'EOF'

./todo.sh json | grep -q '^\["write the release notes"\]$'
echo "ok · json prints an array"
EOF
        ;;
    usage)
        say "· reading todo.sh"
        say "· editing todo.sh — an empty add is a usage error, not an empty line"
        splice todo.sh '^  add)' <<'EOF'
  add)
    shift
    if [ "$#" -eq 0 ]; then
      echo "usage: todo.sh add <text>" >&2
      exit 2
    fi
    printf '%s\n' "$*" >>"$TODO_FILE"
    ;;
EOF
        say "· covering it in test.sh"
        cat >>test.sh <<'EOF'

if ./todo.sh add 2>/dev/null; then
  echo "fail · an empty add should exit nonzero"
  exit 1
fi
echo "ok · an empty add is refused"
EOF
        ;;
    docs)
        say "· reading todo.sh"
        say "· writing the examples into README.md"
        cat >>README.md <<'EOF'

## Usage

```console
$ ./todo.sh add "write the release notes"
$ ./todo.sh
     1  write the release notes
$ ./todo.sh done 1
```

`add` appends one item, `done <n>` removes the numbered line, and `todo.sh`
with no arguments lists everything with its number. An item number that does
not exist is reported by `sed` and changes nothing.
EOF
        ;;
    tests)
        say "· reading test.sh"
        say "· adding a case for a second item"
        cat >>test.sh <<'EOF'

./todo.sh add "cut the release"
test "$(./todo.sh | wc -l)" -eq 2
echo "ok · a second item appends"
EOF
        ;;
esac

say "· running ./test.sh …"
# Captured rather than streamed, because every line has to reach `buffer` for
# the redraw above to be able to reproduce it.
if test_output="$(./test.sh 2>&1)"; then
    printf '%s\n' "$test_output" | while IFS= read -r l; do printf '  %s\n' "$l"; done
    buffer="$buffer$(printf '%s\n' "$test_output" | sed 's/^/  /')
"
    say ""
    say "$(git diff --shortstat | sed 's/^ *//')"
    say "done · the work is on $(git rev-parse --abbrev-ref HEAD), review it like any branch"
else
    say ""
    say "./test.sh failed — leaving the worktree as it is"
fi

if [ -d "$MARKER_DIR" ] && [ -w "$MARKER_DIR" ]; then
    : >"$MARKER_DIR/$role.done"
fi

emit ""
emit "Waiting for the next instruction …"

# `exec cat` would be shorter and is what the self-test's fake agent does, but it
# would replace this shell and take the SIGWINCH trap with it. The loop keeps
# both: typed input still echoes through the TTY (so the pane behaves like the
# fake agent's), the line is recorded so a redraw keeps it, and `read` is
# interruptible, so a resize repaints instead of waiting for the next keystroke.
while :; do
    if IFS= read -r line && [ -n "$line" ]; then
        record "$line"
    fi
done
