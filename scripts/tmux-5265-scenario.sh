#!/usr/bin/env bash
# Container play-test for #5265: a repository that spells its own executable
# or its own environment in program_overrides must NOT receive the operator's
# model-provider credential grant.
#
#   scripts/testbox.sh scenario scripts/tmux-5265-scenario.sh
#
# The scenario builds af.master from the master tar the harness injects and
# runs every leg inside THIS sandbox against real daemons, a real tmux server,
# and a fixture repo whose .agent-factory/config.toml sets program_overrides.
# Each leg gets a fresh AGENT_FACTORY_HOME (its own daemon, recorded by pid +
# /proc exe + sha256) and the daemon's environment is built with `env -i`:
# the ONLY credential-shaped names anywhere are the fake sentinels, so a leak
# is impossible to misread and no real key can ever ride along.
#
# Legs (want = sentinel values in the agent process's real environment, or
# the noted refusal/grant evidence):
#   master-rel        af.master + ./codex                     PRESENT 3 (vuln control)
#   branch-rel        ./codex                                 ABSENT 0
#   branch-binrel     bin/codex                               ABSENT 0
#   branch-baretrail  codex, PATH=<bin>:/usr/bin:/bin:        PRESENT 3
#   branch-barelead   codex, PATH=:<bin>:/usr/bin:/bin        ABSENT 0
#   global-abs        global cfg: <bin>/codex (abs, outside)  PRESENT 3
#   branch-absinrepo  in-repo abs <repo>/bin/codex            create REFUSED (absolute)
#   branch-insymlink  in-repo abs <repo>/bin/codex-link       create REFUSED (absolute)
#   branch-envmut     in-repo LD_PRELOAD=./x.so codex         create REFUSED (command-local env)
#   branch-nohit      bare codex, daemon PATH w/o codex       GRANTED (session env sentinels)
#   branch-envrel     ./env codex (relative env wrapper)      ABSENT 0
#   branch-scoped     ./codex --account work                  create REFUSED (scoped)
#   global-claude     global cfg: ANTHROPIC_BASE_URL=… claude PRESENT + BASE_URL in env
#   master-opencode   af.master + default opencode            OPENCODE_CONFIG prefix + sentinels
#   branch-opencode   af + default opencode                   byte-identical shape to master
#
# Evidence per leg: create output, daemon pid/exe/sha256 vs the intended
# binary, the agent process's own env dump AND /proc/<pid>/environ, and the
# pane's process tree. Everything lands under $EVID and is printed at the end.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

SANDBOX="$HOME/sandbox"
EVID="$SANDBOX/evidence-5265"
FIXTURE="$SANDBOX/repo-5265"
FAKEBIN="$HOME/af5265-bin"
DUMP=/tmp/af-5265-dump.env
BASE_PATH="$PATH"
mkdir -p "$EVID"

SUMMARY="$EVID/summary.txt"
: >"$SUMMARY"
record() { # <line> — one verdict line into the run summary and the console.
    printf '%s\n' "$*" | tee -a "$SUMMARY" >&2
}

# The only credential material anywhere in this run. Obviously fake; `env -i`
# below makes them the ONLY names of their kind the daemon can ever see.
# OPENAI_API_KEY is shared by codex and opencode; ANTHROPIC_API_KEY is the
# claude leg's grant marker (it is deliberately NOT in codex's name set, so a
# codex leg that leaked it would still be caught by the ABSENT verdicts).
SENTINEL_ENV=(
    "OPENAI_API_KEY=af5265-sentinel-openai-not-a-real-key"
    "CODEX_API_KEY=af5265-sentinel-codexapi-not-a-real-key"
    "CODEX_ACCESS_TOKEN=af5265-sentinel-codextok-not-a-real-key"
    "ANTHROPIC_API_KEY=af5265-sentinel-anthropic-not-a-real-key"
)
SENTINEL_RE='af5265-sentinel-[a-z]+-not-a-real-key'

# --- provenance ----------------------------------------------------------
git config --global --add safe.directory /src 2>/dev/null || true
BRANCH_SHA="${AF_BRANCH_SHA:-$(git -C /src rev-parse HEAD 2>/dev/null || true)}"
MASTER_SHA="${AF_MASTER_SHA:-$(git -C /src rev-parse origin/master 2>/dev/null || true)}"
[ -n "$BRANCH_SHA" ] || { record "FAIL: branch SHA unknown — pass AF_BRANCH_SHA"; exit 1; }
[ -n "$MASTER_SHA" ] || { record "FAIL: master SHA unknown — pass AF_MASTER_SHA"; exit 1; }
MASTER_SRC="$(mktemp -d /tmp/af-5265-master.XXXXXX)"
if [ -n "${AF_MASTER_TAR:-}" ]; then
    tar -xf "$AF_MASTER_TAR" -C "$MASTER_SRC"
elif ! git -C /src archive "$MASTER_SHA" | tar -x -C "$MASTER_SRC"; then
    record "FAIL: no master source: pass AF_MASTER_TAR (or give /src a reachable gitdir)"
    exit 1
fi
mkdir -p "$HOME/bin"
(cd "$MASTER_SRC" && go build -buildvcs=false -o "$HOME/bin/af.master" .)
AF_BRANCH="$HOME/bin/af"
AF_MASTER="$HOME/bin/af.master"
{
    echo "branch HEAD (af):        $BRANCH_SHA"
    echo "origin/master (af.master): $MASTER_SHA"
    sha256sum "$AF_BRANCH" "$AF_MASTER"
    "$AF_BRANCH" version 2>/dev/null || true
    "$AF_MASTER" version 2>/dev/null || true
    echo "fixture repo:  $FIXTURE"
    echo "fake agent dir: $FAKEBIN"
    echo "base PATH:     $BASE_PATH"
    echo "environment:   env -i + sentinels only (no real credential material)"
} | tee "$EVID/provenance.txt"

# --- fixture --------------------------------------------------------------
# write_agent <path> <ready-output-command> — the fake agent records WHICH
# file ran ($0), its cwd and pid, dumps the whole inherited environment — the
# env the REAL agent process carries — then prints the agent's readiness
# marker (codex: line-leading '›' U+203A; claude: '❯' U+276F; opencode: a
# composer frame '┃' line followed by the '╹▀▀' bottom rule) so the daemon's
# waitForReady sees the session as ready, and parks on `sleep` so the pane
# stays alive for the process-tree record.
write_agent() {
    mkdir -p "$(dirname "$1")"
    cat >"$1" <<EOS
#!/bin/sh
{
    printf 'AF_5265_SELF=%s\n' "\$0"
    printf 'AF_5265_PWD=%s\n' "\$PWD"
    printf 'AF_5265_PID=%s\n' "\$\$"
    env | sort
} > $DUMP || exit 42
echo "af-5265 fake agent started"
$2
exec sleep 3600
EOS
    chmod +x "$1"
}

mkdir -p "$FIXTURE/.agent-factory" "$FIXTURE/bin" "$FAKEBIN"
write_agent "$FIXTURE/codex"      "printf '› \n'"
write_agent "$FIXTURE/bin/codex"  "printf '› \n'"
write_agent "$FAKEBIN/codex"      "printf '› \n'"
write_agent "$FAKEBIN/claude"     "printf '❯ \n'"
write_agent "$FAKEBIN/opencode"   "printf '┃ ready\n╹▀▀▀▀▀▀\n'"
# A minimal env(1) stand-in for the relative-wrapper leg: execs its argv, so
# `./env codex` really runs codex — just without a grant.
printf '#!/bin/sh\nexec "$@"\n' >"$FIXTURE/env"
chmod +x "$FIXTURE/env"
# An in-tree symlink to an out-of-repo executable: spelling stays inside the
# checkout while its target escapes — the load-time absolute refusal does not
# need to resolve it to refuse the checked-in value.
ln -sf "$FAKEBIN/codex" "$FIXTURE/bin/codex-link"
if [ ! -d "$FIXTURE/.git" ]; then
    (cd "$FIXTURE" && git init -q -b master \
        && : >.af-throwaway-sandbox \
        && git add -A && git commit -qm "fixture for #5265")
fi

# write_config — stdin becomes the repo's checked-in .agent-factory/config.toml.
write_config() { cat >"$FIXTURE/.agent-factory/config.toml"; }

# set_override <command> — the common case: codex sessions whose checked-in
# program_overrides.codex is <command>. --here sessions read the live file,
# so no commit needed per leg.
set_override() {
    write_config <<EOF
default_program = "codex"

[program_overrides]
codex = "$1"
EOF
}

# write_global_config <label> — stdin becomes the leg's GLOBAL config.toml
# ($AGENT_FACTORY_HOME/config.toml): the operator-owned layer that may still
# carry absolute executables and command-local environment.
write_global_config() {
    mkdir -p "$(leg_home "$1")"
    cat >"$(leg_home "$1")/config.toml"
}

# subtree_pids <pid> — comma-separated pid list of <pid> plus all descendants,
# walked from ps output (no psmisc in this image).
subtree_pids() {
    local root="$1" pid ppid changed=1
    declare -A seen=()
    seen[$root]=1
    while [ "$changed" -eq 1 ]; do
        changed=0
        while read -r pid ppid _; do
            [ -n "${pid:-}" ] || continue
            if [ -n "${seen[$ppid]:-}" ] && [ -z "${seen[$pid]:-}" ]; then
                seen[$pid]=1
                changed=1
            fi
        done < <(ps -eo pid=,ppid=)
    done
    (IFS=,; echo "${!seen[*]}")
}

# leg_home <label>
leg_home() { printf '%s/leg-%s/home' "$SANDBOX" "$1"; }

# leg_create <label> <binary> <path> [extra af args...] — run
# `af sessions create <label> --repo $FIXTURE --here` in the fixture with a
# hermetic environment: env -i, per-leg PATH, fake sentinels only, per-leg
# AGENT_FACTORY_HOME (=> per-leg daemon). Prints the combined output; returns
# the command's exit code.
leg_create() {
    local label="$1" bin="$2" legpath="$3" home
    shift 3
    home="$(leg_home "$label")"
    mkdir -p "$home"
    (cd "$FIXTURE" && env -i \
        HOME="$HOME" \
        PATH="$legpath" \
        AGENT_FACTORY_HOME="$home" \
        AGENT_FACTORY_AUTO_UPDATE=false \
        "${SENTINEL_ENV[@]}" \
        "$bin" sessions create "$label" --repo "$FIXTURE" --here "$@" 2>&1)
}

# leg_daemon_provenance <label> <binary> — record the leg daemon's pid,
# /proc/<pid>/exe and sha256, and FAIL the leg if the exe is not the binary
# this leg intended (a silently wrong binary voids the control).
leg_daemon_provenance() {
    local label="$1" bin="$2" home dpid dexe dsha bsha deadline
    home="$(leg_home "$label")"
    deadline=$(( $(date +%s) + 15 ))
    while [ ! -s "$home/daemon.pid" ]; do
        [ "$(date +%s)" -lt "$deadline" ] || { record "$label: FAIL — no daemon.pid in $home"; return 1; }
        sleep 0.5
    done
    dpid="$(cat "$home/daemon.pid")"
    dexe="$(readlink "/proc/$dpid/exe" 2>/dev/null || true)"
    [ -n "$dexe" ] || { record "$label: FAIL — daemon $dpid has no /proc exe"; return 1; }
    dsha="$(sha256sum "/proc/$dpid/exe" | awk '{print $1}')"
    bsha="$(sha256sum "$bin" | awk '{print $1}')"
    {
        echo "daemon pid:    $dpid"
        echo "daemon exe:    $dexe"
        echo "daemon sha256: $dsha"
        echo "intended bin:  $bin"
        echo "intended sha256: $bsha"
        echo "daemon cmdline: $(tr '\0' ' ' <"/proc/$dpid/cmdline")"
    } >"$EVID/$label/daemon.txt"
    if [ "$dsha" != "$bsha" ]; then
        record "$label: FAIL — running daemon is $dexe ($dsha), not $bin ($bsha)"
        return 1
    fi
    printf '%s\n' "$dpid"
}

# leg_pane_record <label> — find this leg's af tmux session (af_<hash>_<title>),
# record its pane pid and full descendant process tree.
leg_pane_record() {
    local label="$1" sname pane_pid
    sname="$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E "_${label}$" | head -1 || true)"
    [ -n "$sname" ] || { record "$label: FAIL — no af_*_${label} tmux session"; return 1; }
    pane_pid="$(tmux list-panes -t "=$sname" -F '#{pane_pid}' | head -1)"
    {
        echo "tmux session: $sname"
        echo "pane_pid:     $pane_pid"
        echo "pane exe:     $(readlink "/proc/$pane_pid/exe" 2>/dev/null || echo '?')"
        echo "--- pane argv ---"
        tr '\0' ' ' <"/proc/$pane_pid/cmdline" 2>/dev/null || true
        echo
        echo "--- descendant process tree ---"
        ps -o pid,ppid,args -p "$(subtree_pids "$pane_pid")" 2>/dev/null || true
    } >"$EVID/$label/pane-tree.txt"
    printf '%s\n' "$sname"
}

# leg_env_dump <label> — wait for the fake agent's env dump, then also capture
# /proc/<agent-pid>/environ: the environment of the REAL agent process both by
# its own report and by the kernel's record.
leg_env_dump() {
    local label="$1" deadline agent_pid
    deadline=$(( $(date +%s) + 60 ))
    while [ ! -f "$DUMP" ]; do
        if [ "$(date +%s)" -ge "$deadline" ]; then
            record "$label: FAIL — no agent env dump in 60s (pane died or program never ran)"
            return 1
        fi
        sleep 0.5
    done
    cp "$DUMP" "$EVID/$label/agent.env"
    agent_pid="$(sed -n 's/^AF_5265_PID=//p' "$DUMP" | head -1)"
    if [ -n "$agent_pid" ] && [ -r "/proc/$agent_pid/environ" ]; then
        tr '\0' '\n' <"/proc/$agent_pid/environ" | sort >"$EVID/$label/agent-proc-environ.txt"
    fi
}

# leg_teardown <label> <daemon-pid> <tmux-session> — kill the daemon FIRST so
# its #1108 restore loop cannot re-spawn the pane mid-teardown, then the tmux
# session. Both best-effort: next leg gets a fresh home either way.
leg_teardown() {
    [ -n "${2:-}" ] && kill "$2" 2>/dev/null || true
    [ -n "${3:-}" ] && tmux kill-session -t "=$3" 2>/dev/null || true
    sleep 0.5
}

# sentinel_count <env-file> — how many fake sentinels the agent really got.
sentinel_count() { grep -cE "$SENTINEL_RE" "$1" 2>/dev/null || true; }

# run_leg <label> <binary> <path> <want-count> [extra af args...] — the in-repo
# config must already be written. want-count is the expected sentinel total in
# BOTH the agent's env dump and its /proc environ (0 = denied grant).
run_leg() {
    local label="$1" bin="$2" legpath="$3" want="$4"
    shift 4
    local evidir="$EVID/$label" create_out rc=0 dpid="" sname="" hits hits_proc verdict self
    mkdir -p "$evidir"
    rm -f "$DUMP"
    _af_log "=== $label: bin=$bin PATH='$legpath' want=$want sentinels"

    create_out="$(leg_create "$label" "$bin" "$legpath" "$@")" || rc=$?
    printf '%s\n' "$create_out" >"$evidir/create.log"
    if [ "$rc" -ne 0 ]; then
        record "$label: FAIL — create exited $rc: $(printf '%s' "$create_out" | tail -1)"
        leg_teardown "$label" "" ""
        return 1
    fi

    dpid="$(leg_daemon_provenance "$label" "$bin")" || { leg_teardown "$label" "" ""; return 1; }
    leg_env_dump "$label" || { leg_teardown "$label" "$dpid" ""; return 1; }
    sname="$(leg_pane_record "$label")" || { leg_teardown "$label" "$dpid" ""; return 1; }

    hits="$(sentinel_count "$evidir/agent.env")"
    hits_proc=0
    [ -f "$evidir/agent-proc-environ.txt" ] && hits_proc="$(sentinel_count "$evidir/agent-proc-environ.txt")"
    self="$(sed -n 's/^AF_5265_SELF=//p' "$evidir/agent.env" | head -1)"
    if [ "$hits" -eq "$want" ] && [ "$hits_proc" -eq "$want" ]; then
        verdict=PASS
    else
        verdict="FAIL — $hits/$want sentinels in env, $hits_proc in /proc environ"
    fi
    record "$label: $verdict (want $want, agent=$self, daemon pid $dpid)"
    leg_teardown "$label" "$dpid" "$sname"
    [ "$verdict" = PASS ]
}

# run_refused_leg <label> <grep-pattern> [extra af args...] — the in-repo or
# scoped launch must be refused: create fails naming the refusal (matched
# against <grep-pattern>), with no session left behind.
run_refused_leg() {
    local label="$1" pattern="$2"
    shift 2
    local evidir="$EVID/$label" create_out rc=0 stray dpid=""
    mkdir -p "$evidir"
    rm -f "$DUMP"
    _af_log "=== $label: want refusal matching '$pattern'"

    create_out="$(leg_create "$label" "$AF_BRANCH" "$BASE_PATH" "$@")" || rc=$?
    printf '%s\n' "$create_out" >"$evidir/create.log"
    if [ "$rc" -eq 0 ]; then
        record "$label: FAIL — create SUCCEEDED for a value that must refuse"
        dpid="$(leg_daemon_provenance "$label" "$AF_BRANCH" 2>/dev/null || true)"
        leg_teardown "$label" "${dpid:-}" "$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E "_${label}$" | head -1 || true)"
        return 1
    fi
    if ! printf '%s' "$create_out" | grep -q "$pattern"; then
        record "$label: FAIL — create refused but without '$pattern': $(printf '%s' "$create_out" | tail -1)"
        return 1
    fi
    stray="$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E "_${label}$" || true)"
    if [ -n "$stray" ]; then
        record "$label: FAIL — a tmux session exists despite the refusal: $stray"
        return 1
    fi
    record "$label: PASS (create refused: $(printf '%s' "$create_out" | grep -o "$pattern" | head -1))"
    dpid="$(cat "$(leg_home "$label")/daemon.pid" 2>/dev/null || true)"
    leg_teardown "$label" "${dpid:-}" ""
}

# run_nohit_leg — bare `codex` when the DAEMON's PATH has no codex at all and
# every entry is absolute: the grant must still apply (exec can only fail, it
# can never land in repository content) — the all-absolute no-hit rule.
#
# Two-stage setup, because the CLI's preflight resolves the program in ITS OWN
# environment and would refuse a bare name missing from its PATH before the
# daemon is ever asked:
#   1. a seed session under a no-hit PATH (override './codex' resolves
#      relative to the fixture regardless of PATH) — this spawns the leg's
#      daemon WITH the no-hit PATH;
#   2. the real create under a PATH that DOES hold codex — preflight passes,
#      the request reaches the already-running daemon, whose environment is
#      what the grant predicate walks.
# The pane then runs `sh -c codex` on the daemon's no-hit PATH and dies
# instantly, so the scenario sets the server's global remain-on-exit (the same
# window option af itself uses, start.go) before creating, leaving a dead
# pane whose session environment — populated by the import channel only when
# the grant applied — can be read deterministically with show-environment.
run_nohit_leg() {
    local label=branch-nohit seed=nohit-seed
    local evidir="$EVID/$label" home dpid="" sname="" create_out rc=0 hits notfound verdict
    home="$(leg_home "$label")"
    mkdir -p "$evidir" "$SANDBOX/nohit-a" "$SANDBOX/nohit-b"
    rm -f "$DUMP"
    _af_log "=== $label: daemon PATH lacks codex; grant must still apply"

    # Stage 1: seed daemon under the no-hit PATH.
    set_override "./codex"
    create_out="$( (cd "$FIXTURE" && env -i \
        HOME="$HOME" \
        PATH="$SANDBOX/nohit-a:$SANDBOX/nohit-b:/usr/bin:/bin" \
        AGENT_FACTORY_HOME="$home" \
        AGENT_FACTORY_AUTO_UPDATE=false \
        "${SENTINEL_ENV[@]}" \
        "$AF_BRANCH" sessions create "$seed" --repo "$FIXTURE" --here 2>&1) )" || rc=$?
    printf '%s\n' "$create_out" >"$evidir/seed-create.log"
    if [ "$rc" -ne 0 ]; then
        record "$label: FAIL — seed create exited $rc: $(printf '%s' "$create_out" | tail -1)"
        leg_teardown "$label" "" ""
        return 1
    fi
    dpid="$(leg_daemon_provenance "$label" "$AF_BRANCH")" || { leg_teardown "$label" "" ""; return 1; }
    rm -f "$DUMP"

    # Stage 2: hold dead panes, then create the bare-codex session under a
    # PATH where the CLI can resolve codex. The create itself fails readiness
    # (the pane cannot run), so it runs in the background while the session
    # environment is captured.
    tmux set-option -g remain-on-exit on
    set_override "codex"
    (cd "$FIXTURE" && env -i \
        HOME="$HOME" \
        PATH="$FAKEBIN:/usr/bin:/bin" \
        AGENT_FACTORY_HOME="$home" \
        AGENT_FACTORY_AUTO_UPDATE=false \
        "${SENTINEL_ENV[@]}" \
        "$AF_BRANCH" sessions create "$label" --repo "$FIXTURE" --here) \
        >"$evidir/create.log" 2>&1 &
    local create_pid=$!

    local deadline=$(( $(date +%s) + 30 ))
    sname=""
    while [ -z "$sname" ]; do
        sname="$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E "_${label}$" | head -1 || true)"
        if [ "$(date +%s)" -ge "$deadline" ]; then break; fi
        sleep 0.3
    done
    if [ -z "$sname" ]; then
        kill "$create_pid" 2>/dev/null || true
        wait "$create_pid" 2>/dev/null || true
        record "$label: FAIL — no af_*_${label} tmux session ever appeared: $(tail -1 "$evidir/create.log")"
        tmux set-option -gu remain-on-exit
        leg_teardown "$label" "$dpid" ""
        return 1
    fi
    tmux show-environment -t "=$sname" >"$evidir/session-env.txt" 2>/dev/null || true
    tmux capture-pane -p -t "=$sname" >"$evidir/pane.txt" 2>/dev/null || true

    # The create cannot succeed (the pane's exec fails instantly), so bound the
    # wait rather than trusting af's readiness timeout to return promptly.
    deadline=$(( $(date +%s) + 120 ))
    while kill -0 "$create_pid" 2>/dev/null; do
        if [ "$(date +%s)" -ge "$deadline" ]; then
            kill "$create_pid" 2>/dev/null || true
            break
        fi
        sleep 0.5
    done
    wait "$create_pid" 2>/dev/null || true
    hits="$(sentinel_count "$evidir/session-env.txt")"
    notfound="$(grep -ci 'not found' "$evidir/pane.txt" 2>/dev/null || true)"
    if [ "$hits" -eq 3 ] && [ "$notfound" -ge 1 ]; then
        verdict=PASS
    else
        verdict="FAIL — $hits/3 sentinels in session env, pane 'not found' hits: $notfound"
    fi
    record "$label: $verdict (all-absolute no-hit PATH; grant applied, nothing ran)"
    tmux kill-session -t "=$sname" 2>/dev/null || true
    tmux set-option -gu remain-on-exit
    leg_teardown "$seed" "$dpid" "$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E "_${seed}$" | head -1 || true)"
    [ "$verdict" = PASS ]
}

# --- legs -------------------------------------------------------------------
FAILS=0
tally() { "$@" || FAILS=$((FAILS + 1)); }

# The original vulnerability shape: master grants a repo-spelled ./codex (the
# control that proves the sentinels CAN flow), the branch denies it.
set_override "./codex"
tally run_leg master-rel "$AF_MASTER" "$BASE_PATH" 3
set_override "./codex"
tally run_leg branch-rel "$AF_BRANCH" "$BASE_PATH" 0
set_override "bin/codex"
tally run_leg branch-binrel "$AF_BRANCH" "$BASE_PATH" 0

# Bare-name PATH walk: a trailing empty entry after the hit still grants; a
# LEADING empty entry denies because it resolves codex into the pane's cwd.
set_override "codex"
tally run_leg branch-baretrail "$AF_BRANCH" "$FAKEBIN:/usr/bin:/bin:" 3
set_override "codex"
tally run_leg branch-barelead "$AF_BRANCH" ":$FAKEBIN:/usr/bin:/bin" 0

# An ABSOLUTE executable outside the repo still grants — but only through the
# operator's own layer. The checked-in file may no longer produce the
# spelling at all, so this leg moves to the global config and the in-repo
# file carries no codex override for it to win against.
write_config <<'EOF'
default_program = "codex"
EOF
write_global_config global-abs <<EOF
[program_overrides]
codex = "$FAKEBIN/codex"
EOF
tally run_leg global-abs "$AF_BRANCH" "$BASE_PATH" 3

# Checked-in absolute executables refuse at load — the original leg plus the
# in-tree-symlink spelling, both aimed outside the checkout.
set_override "$FIXTURE/bin/codex"
tally run_refused_leg branch-absinrepo "absolute executable"
set_override "$FIXTURE/bin/codex-link"
tally run_refused_leg branch-insymlink "absolute executable"

# A checked-in command-local environment refuses at load on the same line:
# LD_PRELOAD=./x.so codex is exactly Codex's review repro.
set_override "LD_PRELOAD=./x.so codex"
tally run_refused_leg branch-envmut "command-local environment"

# Bare codex when the daemon's all-absolute PATH holds no codex: the grant
# still applies — resolution can only fail, never land in the repository.
tally run_nohit_leg

# A RELATIVE env wrapper is not attributable: ./env really runs codex, but no
# credential grant rides along.
set_override "./env codex"
tally run_leg branch-envrel "$AF_BRANCH" "$FAKEBIN:/usr/bin:/bin" 0

# A scoped launch whose grant fails is REFUSED rather than stripped — the
# account boundary cannot become a second door to repository content.
set_override "./codex"
(cd "$FIXTURE" && env -i \
    HOME="$HOME" \
    PATH="$BASE_PATH" \
    AGENT_FACTORY_HOME="$(leg_home branch-scoped)" \
    AGENT_FACTORY_AUTO_UPDATE=false \
    "$AF_BRANCH" accounts add codex work) >"$EVID/branch-scoped-accounts.log" 2>&1 || true
mkdir -p "$EVID/branch-scoped"
tally run_refused_leg branch-scoped "cannot attribute the launch executable" --account work

# An operator's own global override keeps its documented env-assignment
# power: ANTHROPIC_BASE_URL=… claude both runs and receives the claude grant
# (ANTHROPIC_API_KEY is the only sentinel name in claude's set — want 1).
write_config <<'EOF'
default_program = "claude"
EOF
write_global_config global-claude <<'EOF'
[program_overrides]
claude = "ANTHROPIC_BASE_URL=https://example.invalid claude"
EOF
mkdir -p "$EVID/global-claude"
tally run_leg global-claude "$AF_BRANCH" "$FAKEBIN:/usr/bin:/bin" 1
if [ -f "$EVID/global-claude/agent.env" ] \
    && ! grep -q '^ANTHROPIC_BASE_URL=https://example\.invalid$' "$EVID/global-claude/agent.env"; then
    record "global-claude: FAIL — grant landed but ANTHROPIC_BASE_URL=https://example.invalid missing from the agent env"
    FAILS=$((FAILS + 1))
elif [ -f "$EVID/global-claude/agent.env" ]; then
    record "global-claude: ANTHROPIC_BASE_URL=https://example.invalid present in agent env (operator override honored)"
fi

# opencode launches EXACTLY as on master: the OPENCODE_CONFIG prefix rides the
# command string, not an injected-env channel — run both binaries and compare
# the normalized program argv.
write_config <<'EOF'
default_program = "opencode"
EOF
tally run_leg master-opencode "$AF_MASTER" "$FAKEBIN:/usr/bin:/bin" 2
write_config <<'EOF'
default_program = "opencode"
EOF
tally run_leg branch-opencode "$AF_BRANCH" "$FAKEBIN:/usr/bin:/bin" 2
opencode_argv() { # <label> — the opencode invocation from the pane tree, home-normalized.
    grep -o 'OPENCODE_CONFIG=[^ ]*opencode' "$EVID/$1/pane-tree.txt" 2>/dev/null \
        | sed "s|$SANDBOX/leg-[^/]*/home|<AFHOME>|g" | head -1
}
master_oc="$(opencode_argv master-opencode)"
branch_oc="$(opencode_argv branch-opencode)"
echo "master opencode argv: ${master_oc:-<none>}" >>"$EVID/opencode-parity.txt"
echo "branch opencode argv: ${branch_oc:-<none>}" >>"$EVID/opencode-parity.txt"
if [ -n "$master_oc" ] && [ "$master_oc" = "$branch_oc" ]; then
    record "opencode-parity: PASS (both launch 'OPENCODE_CONFIG=<AFHOME>/opencode/af-config.jsonc opencode')"
else
    record "opencode-parity: FAIL — master='${master_oc:-<none>}' branch='${branch_oc:-<none>}'"
    FAILS=$((FAILS + 1))
fi

# --- report -------------------------------------------------------------------
{
    echo
    echo "================ #5265 play-test summary ================"
    cat "$SUMMARY"
    echo "=========================================================="
    echo "evidence under $EVID:"
    find "$EVID" -type f | sort
} >&2

record "play-test complete: $FAILS failed leg(s)"
[ "$FAILS" -eq 0 ]
