#!/usr/bin/env bash
# Container play-test for #5265: a repository that spells its own executable
# in program_overrides must NOT receive the operator's model-provider
# credential grant.
#
#   scripts/testbox.sh scenario scripts/tmux-5265-scenario.sh
#
# The scenario builds af.master from the master tar the harness injects and
# runs every leg inside THIS sandbox against real daemons, a real tmux server,
# and a fixture repo whose .agent-factory/config.toml sets program_overrides.
# Each leg gets a fresh AGENT_FACTORY_HOME (its own daemon, recorded by pid +
# /proc exe + sha256) and the daemon's environment is built with `env -i`:
# the ONLY credential-shaped names anywhere are the three fake sentinels, so a
# leak is impossible to misread and no real key can ever ride along.
#
# Legs (want = the three sentinels in the agent process's real environment):
#   master-rel        af.master + ./codex                  PRESENT (vuln control)
#   branch-rel        af        + ./codex                  ABSENT
#   branch-binrel     af        + bin/codex                ABSENT
#   branch-baretrail  af        + codex, PATH=<bin>:/usr/bin:/bin:   PRESENT
#   branch-barelead   af        + codex, PATH=:<bin>:/usr/bin:/bin   ABSENT
#   branch-abs        af        + <bin>/codex (abs, outside repo)  PRESENT
#   branch-absinrepo  af        + <repo>/bin/codex (abs, in repo)  create REFUSED
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
SENTINEL_ENV=(
    "OPENAI_API_KEY=af5265-sentinel-openai-not-a-real-key"
    "CODEX_API_KEY=af5265-sentinel-codexapi-not-a-real-key"
    "CODEX_ACCESS_TOKEN=af5265-sentinel-codextok-not-a-real-key"
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
# The fake agent records WHICH file ran ($0), its cwd and pid, dumps the whole
# inherited environment — the env the REAL agent process carries — then prints
# the line-leading '›' codex composer glyph so the daemon's waitForReady sees
# the session as ready (ResolvedAgent classifies every spelling here as codex),
# and parks on `sleep` so the pane stays alive for the process-tree record.
write_agent() {
    mkdir -p "$(dirname "$1")"
    cat >"$1" <<'EOS'
#!/bin/sh
{
    printf 'AF_5265_SELF=%s\n' "$0"
    printf 'AF_5265_PWD=%s\n' "$PWD"
    printf 'AF_5265_PID=%s\n' "$$"
    env | sort
} > /tmp/af-5265-dump.env || exit 42
echo "af-5265 fake agent started"
printf '› \n'
exec sleep 3600
EOS
    chmod +x "$1"
}

mkdir -p "$FIXTURE/.agent-factory" "$FIXTURE/bin" "$FAKEBIN"
write_agent "$FIXTURE/codex"
write_agent "$FIXTURE/bin/codex"
write_agent "$FAKEBIN/codex"
if [ ! -d "$FIXTURE/.git" ]; then
    (cd "$FIXTURE" && git init -q -b master \
        && : >.af-throwaway-sandbox \
        && git add -A && git commit -qm "fixture for #5265")
fi

# set_override <command> — point the repo's checked-in program_overrides.codex
# at <command>. --here sessions read the live file, so no commit needed per leg.
set_override() {
    printf 'default_program = "codex"\n\n[program_overrides]\ncodex = "%s"\n' "$1" \
        >"$FIXTURE/.agent-factory/config.toml"
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

# leg_daemon_home <label>
leg_home() { printf '%s/leg-%s/home' "$SANDBOX" "$1"; }

# leg_create <label> <binary> <path> — run `af sessions create <label> --here`
# in the fixture with a hermetic environment: env -i, per-leg PATH, fake
# sentinels only, per-leg AGENT_FACTORY_HOME (=> per-leg daemon). Prints the
# combined output; returns the command's exit code.
leg_create() {
    local label="$1" bin="$2" legpath="$3" home
    home="$(leg_home "$label")"
    mkdir -p "$home"
    (cd "$FIXTURE" && env -i \
        HOME="$HOME" \
        PATH="$legpath" \
        AGENT_FACTORY_HOME="$home" \
        AGENT_FACTORY_AUTO_UPDATE=false \
        "${SENTINEL_ENV[@]}" \
        "$bin" sessions create "$label" --repo "$FIXTURE" --here 2>&1)
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

# run_leg <label> <binary> <override> <path> <want:present|absent>
run_leg() {
    local label="$1" bin="$2" override="$3" legpath="$4" want="$5"
    local evidir="$EVID/$label" create_out rc=0 dpid="" sname="" hits hits_proc verdict self
    mkdir -p "$evidir"
    set_override "$override"
    rm -f "$DUMP"
    _af_log "=== $label: bin=$bin override='$override' PATH='$legpath' want=$want"

    create_out="$(leg_create "$label" "$bin" "$legpath")" || rc=$?
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
    case "$want" in
    present)
        if [ "$hits" -eq "${#SENTINEL_ENV[@]}" ] && [ "$hits_proc" -eq "${#SENTINEL_ENV[@]}" ]; then verdict=PASS; else verdict="FAIL — $hits/${#SENTINEL_ENV[@]} sentinels in env, $hits_proc in /proc environ"; fi
        ;;
    absent)
        if [ "$hits" -eq 0 ] && [ "$hits_proc" -eq 0 ]; then verdict=PASS; else verdict="FAIL — $hits sentinels in env, $hits_proc in /proc environ"; fi
        ;;
    esac
    record "$label: $verdict (want $want, agent=$self, daemon pid $dpid)"
    leg_teardown "$label" "$dpid" "$sname"
    [ "$verdict" = PASS ]
}

# run_refused_leg — an ABSOLUTE override that names a file inside the repo is
# refused at config load: create must fail naming the refusal, with no session
# and no dump ever appearing.
run_refused_leg() {
    local label=branch-absinrepo
    local evidir="$EVID/$label" create_out rc=0 stray
    mkdir -p "$evidir"
    set_override "$FIXTURE/bin/codex"
    rm -f "$DUMP"
    _af_log "=== $label: bin=$AF_BRANCH override='$FIXTURE/bin/codex' want=refused"

    create_out="$(leg_create "$label" "$AF_BRANCH" "$BASE_PATH")" || rc=$?
    printf '%s\n' "$create_out" >"$evidir/create.log"
    if [ "$rc" -eq 0 ]; then
        record "$label: FAIL — create SUCCEEDED for an absolute in-repo override"
        dpid="$(leg_daemon_provenance "$label" "$AF_BRANCH" 2>/dev/null || true)"
        leg_teardown "$label" "${dpid:-}" "$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E "_${label}$" | head -1 || true)"
        return 1
    fi
    if ! printf '%s' "$create_out" | grep -q "inside this repository"; then
        record "$label: FAIL — create refused but without the in-repo refusal message: $(printf '%s' "$create_out" | tail -1)"
        return 1
    fi
    stray="$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -E "_${label}$" || true)"
    if [ -n "$stray" ]; then
        record "$label: FAIL — a tmux session exists despite the refusal: $stray"
        return 1
    fi
    record "$label: PASS (create refused: $(printf '%s' "$create_out" | grep -o 'inside this repository[^.]*' | head -1))"
    local dpid=""
    dpid="$(cat "$(leg_home "$label")/daemon.pid" 2>/dev/null || true)"
    leg_teardown "$label" "${dpid:-}" ""
}

# --- legs -------------------------------------------------------------------
FAILS=0
run_leg master-rel       "$AF_MASTER" "./codex"          "$BASE_PATH"                    present || FAILS=$((FAILS + 1))
run_leg branch-rel       "$AF_BRANCH" "./codex"          "$BASE_PATH"                    absent  || FAILS=$((FAILS + 1))
run_leg branch-binrel    "$AF_BRANCH" "bin/codex"        "$BASE_PATH"                    absent  || FAILS=$((FAILS + 1))
run_leg branch-baretrail "$AF_BRANCH" "codex"            "$FAKEBIN:/usr/bin:/bin:"       present || FAILS=$((FAILS + 1))
run_leg branch-barelead  "$AF_BRANCH" "codex"            ":$FAKEBIN:/usr/bin:/bin"       absent  || FAILS=$((FAILS + 1))
run_leg branch-abs       "$AF_BRANCH" "$FAKEBIN/codex"   "$BASE_PATH"                    present || FAILS=$((FAILS + 1))
run_refused_leg                                                                              || FAILS=$((FAILS + 1))

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
