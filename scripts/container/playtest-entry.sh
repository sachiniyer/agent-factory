#!/usr/bin/env bash
# Runs INSIDE the testbox container (see scripts/testbox.sh playtest):
# build af from the mounted source, scaffold the play-test sandbox that the
# tui-playtest skill needs (throwaway AF home with cheap-instance config +
# a small mock project repo), then hand over an interactive shell — or park
# with `hold` so a driver can work the sandbox via `docker exec`.
#
# Everything here — the tmux server, the daemon, the AF home, every process
# a play-test spawns — dies with the container. Teardown is `exit` /
# `docker rm -f`, not a checklist.
set -euo pipefail

SANDBOX="$HOME/sandbox"
export AGENT_FACTORY_HOME="${AGENT_FACTORY_HOME:-$SANDBOX/home}"
mkdir -p "$AGENT_FACTORY_HOME" "$HOME/bin"

echo ">>> building af from /src ..."
(cd /src && go build -o "$HOME/bin/af" .)

# Cheap instances: run a plain shell instead of a real agent. The override
# must be a wrapper, not bare "bash": af appends claude-specific flags
# (--plugin-dir ...) to whatever the claude program resolves to, and bash
# exits instantly on the unknown option — the session then dies before the
# existence poll sees it ("timed out waiting for tmux session").
cat >"$HOME/bin/fake-agent" <<'EOF'
#!/bin/bash
# Swallow the agent flags af appends (--plugin-dir ...) and run a shell.
exec bash
EOF
chmod +x "$HOME/bin/fake-agent"
cat >"$AGENT_FACTORY_HOME/config.json" <<EOF
{ "default_program": "claude", "program_overrides": { "claude": "$HOME/bin/fake-agent" } }
EOF

# Mock project repo — small but real, so worktrees/diffs have something to
# show. Never a real repo: the real repos aren't in this container at all.
MOCK="$SANDBOX/mock-repo"
if [ ! -d "$MOCK" ]; then
    mkdir -p "$MOCK"
    cd "$MOCK"
    git init -q -b master
    printf '#!/bin/bash\n# todo: list, add <text>, done <n>\nTODO_FILE="${TODO_FILE:-todo.txt}"\ntouch "$TODO_FILE"\ncase "$1" in\n  add) shift; echo "$*" >> "$TODO_FILE";;\n  done) sed -i "${2}d" "$TODO_FILE";;\n  *) nl -ba "$TODO_FILE";;\nesac\n' >todo.sh
    chmod +x todo.sh
    printf '# todo\nA tiny shell todo app.\n\nUsage: ./todo.sh [add <text> | done <n>]\n' >README.md
    printf '#!/bin/bash\nset -e\nexport TODO_FILE=$(mktemp)\n./todo.sh add "first item"\n./todo.sh | grep -q "first item"\nrm -f "$TODO_FILE"\necho ok\n' >test.sh
    chmod +x test.sh
    git add -A && git commit -qm "initial project"
fi

cat <<EOF

=== af play-test sandbox (container) ==========================
  binary:   $HOME/bin/af  ($(af version 2>/dev/null || echo 'af version unavailable'))
  AF home:  $AGENT_FACTORY_HOME  (throwaway)
  mock repo: $MOCK
  tmux:     this container's own server — kill-server is harmless here

  start playing:   cd $MOCK && af
  teardown:        exit the shell (or: docker rm -f <container>)
===============================================================

EOF

if [ "${1:-}" = "hold" ]; then
    # Detached mode: park so the driver can `docker exec` tmux commands.
    exec sleep infinity
fi
cd "$MOCK"
exec bash
