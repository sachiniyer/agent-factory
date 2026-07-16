#!/usr/bin/env bash
# lifecycle.sh — the clean-environment install + upgrade gate.
#
# WHY THIS EXISTS
#
# Every other gate in this repo tests CODE IN ISOLATION. `make test-container`,
# the *-roundtrip harnesses, the TUI driver self-test — all of them build the
# current tree and exercise it. None of them ever installs a release, upgrades
# it, and checks what the machine looks like afterwards. So the failures that
# reached users were all the ones that live BETWEEN versions:
#
#   #1921  a NEW client against an OLD daemon hard-failed ("unknown field
#          tab_id"). The daemon is upgraded independently of its clients, so
#          this is only reachable across a version boundary — no single-version
#          test can construct it.
#   #1916  `af reset` SIGKILLed a wedged daemon and the service manager
#          relaunched it mid-wipe. Observed by a user, not by us.
#   #796   the daemon demoted from supervised (a systemd unit) to an ad-hoc
#          child after a restart: a daemon still runs and still answers, while
#          the unit sits inactive/dead. Happened on the maintainer's box.
#
# A dev box is the opposite of the environment those bugs need: it has an
# already-installed unit, a live daemon, months of state. "Works here" says
# nothing about a new machine, which has NO config, NO unit, NO home, NO daemon.
# This harness builds that machine from nothing and then upgrades it.
#
# SCENARIOS
#
#   A) clean first run   — nothing pre-exists; af must come up anyway.
#   B) install -> upgrade — install a REAL previous release, put state on it,
#                           upgrade the way a user actually does, then assert
#                           the machine is coherent afterwards.
#
# ISOLATION
#
# This harness is destructive by design (it installs binaries, registers
# autostart units, upgrades af over itself, stops daemons). It refuses to run
# outside a container or CI runner — see lc_guard_disposable. On a dev box the
# only entry point is `make lifecycle-container`.
#
# WHAT THIS DOES NOT COVER YET
#
#   * macOS/launchd. lc_daemon_version reads /proc/<pid>/exe, which macOS does
#     not have, and the supervision assertion would need launchctl. A macOS
#     matrix leg lands once the macos-latest CI job does; the scenarios are
#     written to be OS-agnostic apart from those two helpers.
#   * Assertion #4 (supervision) on container runs: no systemd inside, so it is
#     SKIPped loudly there and only really runs on the CI runner.
#
# Usage:
#   AF_LIFECYCLE_DISPOSABLE=1 scripts/lifecycle.sh [scenario-a|scenario-b|all]
#
# Environment:
#   AF_LIFECYCLE_AF_BIN      af built from this tree (scenario A's "new user
#                            installs today's af"). Required for scenario A.
#   AF_LIFECYCLE_WORKSPACE   throwaway scratch root (default: $HOME/af-lifecycle)
#   AF_LIFECYCLE_INJECT      fault injection for proving the harness detects a
#                            known-bad machine. See INJECTIONS below.
#   GITHUB_TOKEN             optional; only to raise the release-API rate limit.

set -uo pipefail

LC_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LC_WORKSPACE="${AF_LIFECYCLE_WORKSPACE:-$HOME/af-lifecycle}"
# shellcheck source=scripts/lifecycle-lib.sh
. "$LC_HERE/lifecycle-lib.sh"

LC_REPO_SLUG="sachiniyer/agent-factory"
LC_API="https://api.github.com/repos/$LC_REPO_SLUG/releases?per_page=100"
LC_DL="https://github.com/$LC_REPO_SLUG/releases/download"

# INJECTIONS — deliberately break the machine to prove an assertion fires.
# The gate's value is the bugs it catches NEXT, so it has to be shown catching
# one now. See the PR body / docs/lifecycle-testing.md.
#
#   skip-daemon-restart  swap the binary to N but never restart the daemon —
#                        exactly the #1921 skew condition (new client, old
#                        daemon). Assertions 1 and 2 must FAIL.
LC_INJECT="${AF_LIFECYCLE_INJECT:-}"

# ----------------------------------------------------------------------------
# Fixtures
# ----------------------------------------------------------------------------

# lc_mock_repo <dir> — a small real git repo to run sessions against. Built
# here rather than reused from the playtest sandbox so this harness is
# self-contained: it runs identically in the container and on a CI runner,
# which has no sandbox scaffolding.
lc_mock_repo() {
    local dir="$1"
    [ -d "$dir/.git" ] && return 0
    mkdir -p "$dir" || return 1
    (
        cd "$dir" || exit 1
        git init -q -b master
        printf '# mock\nA throwaway project for the lifecycle gate.\n' >README.md
        git add -A
        git -c user.name="AF Lifecycle" -c user.email="lifecycle@localhost" \
            commit -qm "initial"
    )
}

# lc_cheap_agent <bin> — stand in for "the user has an agent installed": run
# `bash` instead of a real agent, so a session is a real session (worktree,
# tmux pane, daemon record) without shipping claude into the image. The
# play-test sandbox does the same thing (#1116/#1131: af keys flag injection
# and readiness off the program the override actually runs, so bare `bash` gets
# no claude flags appended).
#
# Set through `af config set` rather than by writing a config file, for two
# reasons learned the hard way:
#   * af materializes config.toml on its FIRST run, and config.toml then wins
#     over a hand-written config.json — so a file dropped in afterwards is
#     silently ignored and every session create fails "claude not on PATH".
#   * `af config set` is the supported surface and exists on both sides of the
#     upgrade boundary (verified against v1.0.192), so scenario B can use the
#     same helper on the old release.
#
# The daemon reads config at startup, so callers must set this BEFORE a daemon
# starts, or restart it afterwards — af says so itself when you run it.
lc_cheap_agent() {
    local bin="$1"
    "$bin" config set program_overrides.claude bash >/dev/null 2>&1
}

# lc_goos_goarch — the release asset suffix for this machine.
lc_goos_goarch() {
    local os arch
    case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *)
        lc_say "unsupported OS: $(uname -s)"
        return 1
        ;;
    esac
    case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *)
        lc_say "unsupported arch: $(uname -m)"
        return 1
        ;;
    esac
    printf '%s-%s\n' "$os" "$arch"
}

# lc_two_newest_stable — "N-1<TAB>N": the two newest non-prerelease releases.
#
# Resolved from the API rather than pinned so the gate keeps testing the real
# current boundary as releases cut. Nothing here asserts a specific version —
# the assertions are about daemon/client coherence, which is version-agnostic.
lc_two_newest_stable() {
    local auth=() json tags
    [ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
    json=$(curl -sSL --max-time 60 "${auth[@]}" "$LC_API" 2>/dev/null) || return 1
    tags=$(printf '%s' "$json" |
        jq -r '[.[] | select(.draft==false and .prerelease==false) | .tag_name] | .[0:2] | @tsv' 2>/dev/null)
    [ -n "$tags" ] || return 1
    # Two distinct releases are required: with fewer, "upgrade from N-1 to N"
    # has nothing to upgrade across.
    [ "$(printf '%s' "$tags" | awk -F'\t' '{print NF}')" = "2" ] || return 1
    # @tsv gives newest-first; the caller wants N-1 first.
    printf '%s\t%s\n' "$(printf '%s' "$tags" | cut -f2)" "$(printf '%s' "$tags" | cut -f1)"
}

# lc_install_release <tag> <dest-bin> — install a REAL release binary, the way
# a user gets one: download the published tarball and put the binary on PATH.
lc_install_release() {
    local tag="$1" dest="$2" sfx tgz tmp
    sfx="$(lc_goos_goarch)" || return 1
    tmp="$(mktemp -d "${TMPDIR:-/tmp}/lc-rel.XXXXXX")" || return 1
    tgz="$tmp/af.tar.gz"
    if ! curl -sSL --max-time 180 -o "$tgz" \
        "$LC_DL/$tag/agent-factory-$sfx.tar.gz"; then
        lc_say "failed to download release $tag ($sfx)"
        rm -rf "$tmp"
        return 1
    fi
    if ! tar -xzf "$tgz" -C "$tmp" 2>/dev/null; then
        lc_say "failed to extract release $tag"
        rm -rf "$tmp"
        return 1
    fi
    mkdir -p "$(dirname "$dest")"
    # Write through a temp name + mv so dest is never a half-written binary.
    mv "$tmp/agent-factory" "$dest.new" || {
        rm -rf "$tmp"
        return 1
    }
    chmod +x "$dest.new"
    mv "$dest.new" "$dest"
    rm -rf "$tmp"
}

# lc_boot_tui <bin> <home> <repo> <tmux-session> [marker-timeout]
#
# Boot the real TUI in a private tmux session and wait for its first frame.
# Waits on a screen marker rather than sleeping, the same rule the TUI driver
# follows (#1161) — a fixed sleep here would flake on a loaded CI box.
lc_boot_tui() {
    local bin="$1" home="$2" repo="$3" sess="$4" timeout="${5:-60}"
    local i deadline
    tmux kill-session -t "$sess" 2>/dev/null
    tmux new-session -d -s "$sess" -x 100 -y 30 || return 1
    tmux send-keys -t "$sess" \
        "export AGENT_FACTORY_HOME=$home; cd $repo && $bin" Enter
    deadline=$((timeout * 2))
    for ((i = 0; i < deadline; i++)); do
        if tmux capture-pane -p -t "$sess" 2>/dev/null | grep -q 'Agent Factory'; then
            return 0
        fi
        sleep 0.5
    done
    lc_say "TUI did not render a first frame within ${timeout}s; screen was:"
    tmux capture-pane -p -t "$sess" 2>/dev/null | sed 's/^/[lifecycle]   | /' >&2
    return 1
}

lc_capture() { tmux capture-pane -p -t "$1" 2>/dev/null; }

# ----------------------------------------------------------------------------
# Scenario A — clean first run.
#
# The machine a new user has: no config.toml, no AF home, no autostart unit, no
# daemon. Nothing in af may assume any of them already exist.
# ----------------------------------------------------------------------------
scenario_a() {
    lc_section "scenario A — clean first run"

    local bin="${AF_LIFECYCLE_AF_BIN:-}"
    if [ -z "$bin" ] || [ ! -x "$bin" ]; then
        lc_fail "scenario-a: AF_LIFECYCLE_AF_BIN is not an executable af binary ('$bin')"
        return 1
    fi

    # Separate statements on purpose: `local a=1 b=$a` expands every argument
    # BEFORE assigning any of them, so $a would still be unbound in b.
    local ws="$LC_WORKSPACE/a"
    local home="$ws/home"
    local repo="$ws/repo"
    rm -rf "$ws"
    mkdir -p "$ws"
    lc_mock_repo "$repo" || {
        lc_fail "scenario-a: could not build the mock repo"
        return 1
    }

    # Preconditions: prove the environment really is clean before we claim af
    # coped with a clean one.
    if [ ! -e "$home" ]; then
        lc_pass "precondition: no AF home at $home"
    else
        lc_fail "precondition: AF home already exists at $home"
    fi
    if [ ! -e "$home/config.toml" ]; then
        lc_pass "precondition: no config.toml"
    else
        lc_fail "precondition: config.toml already exists"
    fi
    lc_assert_eq "0" "$(lc_daemon_count "$home")" "precondition: no daemon for this home"
    if lc_supervisor_available && [ "$(lc_unit_active)" = "active" ]; then
        lc_fail "precondition: an autostart unit is already active"
    else
        lc_pass "precondition: no active autostart unit"
    fi

    export AGENT_FACTORY_HOME="$home"
    # A fresh home has no throttle record, so the launch path would try to
    # self-update and re-exec mid-scenario. Scenario A is about the clean boot,
    # not the update; scenario B owns that path explicitly.
    export AGENT_FACTORY_AUTO_UPDATE=false

    # 1. af runs at all.
    if "$bin" version >/dev/null 2>&1; then
        lc_pass "af version exits 0 in a virgin environment"
    else
        lc_fail "af version failed in a virgin environment"
    fi

    # 2. doctor is the new user's first stop: it must run, and must not report a
    #    FAIL that a brand-new user cannot act on. (Missing agents are WARNs
    #    with a fix line, which is correct — they are not FAILs.)
    local fails
    fails="$(lc_doctor_fail_count "$bin")"
    if [ -z "$fails" ]; then
        lc_fail "scenario-a: could not read a FAIL count out of af doctor"
    else
        lc_assert_eq "0" "$fails" "af doctor reports no FAIL on a brand-new machine"
        [ "$fails" = "0" ] || lc_doctor_dump "$bin" | sed 's/^/[lifecycle]   | /' >&2
    fi

    # 3. The TUI itself comes up on a virgin home — no config, no state.json,
    #    every first-run overlay unseen. This is a new user's literal first act.
    if lc_boot_tui "$bin" "$home" "$repo" lc-a; then
        lc_pass "the TUI renders its first frame on a virgin home"
        local screen
        screen="$(lc_capture lc-a)"
        if printf '%s' "$screen" | grep -qiE 'panic:|fatal error|runtime error'; then
            lc_fail "the TUI first frame contains a panic/fatal error"
            printf '%s' "$screen" | sed 's/^/[lifecycle]   | /' >&2
        else
            lc_pass "no panic on the virgin first frame"
        fi
    else
        lc_fail "the TUI did not boot on a virgin home"
    fi

    # 4. The daemon comes up exactly ONCE. Launching the TUI is what starts it
    #    on a clean machine (measured: 0 daemons before the first launch, 1
    #    after), so this is the moment to count — a second daemon racing up here
    #    is the orphan/duplicate class.
    local i
    for ((i = 0; i < 40; i++)); do
        [ "$(lc_daemon_count "$home")" -ge 1 ] && break
        sleep 0.5
    done
    lc_assert_eq "1" "$(lc_daemon_count "$home")" "the daemon came up exactly once on first launch"
    lc_assert_eq "true" "$(lc_status_field "$bin" '.data.running')" "daemon reports running"
    lc_assert_eq "true" "$(lc_status_field "$bin" '.data.pid_verified')" "daemon pid is verified"
    tmux kill-session -t lc-a 2>/dev/null

    # 5. Stand in for "the user installs an agent", then use the machine. The
    #    daemon is already up and read its config at startup, so it needs the
    #    restart af itself prescribes — and that restart must not leave two.
    lc_cheap_agent "$bin"
    "$bin" daemon restart >/dev/null 2>&1
    lc_assert_eq "1" "$(lc_daemon_count "$home")" "still exactly one daemon after a config-change restart"

    if (cd "$repo" && "$bin" sessions create --name clean1 >/dev/null 2>&1); then
        lc_pass "first session created on a clean machine"
    else
        lc_fail "could not create the first session on a clean machine"
        (cd "$repo" && "$bin" sessions create --name clean1 2>&1 | head -3 |
            sed 's/^/[lifecycle]   | /' >&2)
    fi

    # A second client must adopt the running daemon, never race up a rival.
    (cd "$repo" && "$bin" sessions create --name clean2 >/dev/null 2>&1)
    lc_assert_eq "1" "$(lc_daemon_count "$home")" "still exactly one daemon after a second client"

    lc_teardown_home "$home"
}

# ----------------------------------------------------------------------------
# Scenario B — install a real N-1 release, then upgrade to N.
#
# $1: upgrade mode — "upgrade-cmd" (af upgrade) or "launch" (the launch-time
#     auto-update path, which is a different code path into the same restart).
# ----------------------------------------------------------------------------
scenario_b() {
    local mode="$1"
    lc_section "scenario B — install -> upgrade (mode: $mode)"

    # Separate statements: see the note in scenario_a.
    local ws="$LC_WORKSPACE/b-$mode"
    local home="$ws/home"
    local repo="$ws/repo"
    local bin="$ws/bin/af"
    rm -rf "$ws"
    mkdir -p "$ws/bin"
    lc_mock_repo "$repo" || {
        lc_fail "scenario-b/$mode: could not build the mock repo"
        return 1
    }

    local tags old_tag new_tag
    tags="$(lc_two_newest_stable)" || {
        lc_fail "scenario-b/$mode: could not resolve the two newest stable releases (network? rate limit?)"
        return 1
    }
    old_tag="$(printf '%s' "$tags" | cut -f1)"
    new_tag="$(printf '%s' "$tags" | cut -f2)"
    lc_say "upgrading across the real release boundary: $old_tag -> $new_tag"

    # --- install the REAL previous release -----------------------------------
    lc_install_release "$old_tag" "$bin" || {
        lc_fail "scenario-b/$mode: could not install $old_tag"
        return 1
    }
    export AGENT_FACTORY_HOME="$home"
    export AGENT_FACTORY_AUTO_UPDATE=false

    local old_ver
    old_ver="$(lc_client_version "$bin")"
    lc_assert_eq "${old_tag#v}" "$old_ver" "installed the real previous release"

    # Configure the cheap agent BEFORE any daemon exists, so the daemon reads it
    # at startup and no restart is needed to apply it.
    lc_cheap_agent "$bin" || lc_fail "scenario-b/$mode: could not set program_overrides on $old_tag"

    # --- supervise it, when the environment can ------------------------------
    local supervised=no
    if lc_supervisor_available; then
        if "$bin" daemon install >/dev/null 2>&1; then
            supervised=yes
            lc_pass "autostart unit installed (this machine can supervise)"
        else
            lc_fail "af daemon install failed on a machine with a service manager"
        fi
    else
        # Loud, not silent: a container run does NOT cover assertion #4.
        lc_skip "assertion #4 (unit stays the supervisor): no user service manager here (container has no systemd)"
    fi

    # --- put state on the old daemon ----------------------------------------
    # Errors are surfaced, not swallowed: if the sessions never get created the
    # rest of this scenario is vacuous ("0 sessions survived the upgrade" is a
    # PASS that tested nothing), so this has to be loud and fatal.
    # Capture create output unconditionally. `af sessions create` can report a
    # refusal in its JSON body ({"error": "... is not installed or not on
    # PATH"}) — so an exit status alone is not evidence that a session exists,
    # and a diagnostic that only fires on a non-zero exit prints nothing at all
    # in exactly the case you need it.
    local n rc create_log=""
    for n in pre1 pre2; do
        local out
        rc=0
        out="$(cd "$repo" && "$bin" sessions create --name "$n" 2>&1)" || rc=$?
        create_log+="[$n rc=$rc] $(printf '%s' "$out" | head -3)"$'\n'
    done

    local before_sessions before_pid before_daemon_ver
    before_sessions="$(lc_session_count "$bin")"
    before_pid="$(lc_daemon_pids "$home" | head -1)"
    if [ -z "$before_pid" ]; then
        lc_fail "scenario-b/$mode: no daemon came up on the old release; nothing to upgrade"
        return 1
    fi
    before_daemon_ver="$(lc_daemon_version "$before_pid")"
    lc_say "before upgrade: daemon pid=$before_pid version=$before_daemon_ver sessions=$before_sessions"

    # Hard gate. Scenario B's whole premise is "there is state to preserve"; if
    # we could not put any on the machine, ABORT rather than run assertion 5
    # against zero sessions and report a vacuous pass. This is the difference
    # between a gate and a green light.
    if [ "$before_sessions" != "2" ]; then
        lc_fail "scenario-b/$mode: expected 2 sessions before the upgrade, got '$before_sessions' — aborting (assertion 5 would be vacuous)"
        lc_say "what 'sessions create' actually said:"
        printf '%s' "$create_log" | sed 's/^/[lifecycle]   | /' >&2
        lc_say "session list was:"
        "$bin" sessions list 2>&1 | head -5 | sed 's/^/[lifecycle]   | /' >&2
        lc_say "configured program_overrides:"
        "$bin" config get program_overrides.claude 2>&1 | head -3 | sed 's/^/[lifecycle]   | /' >&2
        lc_teardown_home "$home" "$supervised"
        return 1
    fi
    lc_pass "two sessions exist before the upgrade (= $before_sessions)"

    local before_unit_pid=""
    if [ "$supervised" = yes ]; then
        before_unit_pid="$(lc_unit_main_pid)"
        lc_assert_eq "active" "$(lc_unit_active)" "the unit supervises the daemon before the upgrade"
    fi

    # --- the upgrade ---------------------------------------------------------
    lc_do_upgrade "$mode" "$bin" "$home" "$repo" "$new_tag" || {
        lc_fail "scenario-b/$mode: the upgrade step itself failed"
        return 1
    }

    # --- assertions ----------------------------------------------------------
    local after_pid after_daemon_ver client_ver after_sessions
    after_pid="$(lc_daemon_pids "$home" | head -1)"
    client_ver="$(lc_client_version "$bin")"

    # (0) the client really did move to N — otherwise every assertion below is
    #     vacuously true and the gate would pass while testing nothing.
    lc_assert_eq "${new_tag#v}" "$client_ver" "the client binary is now the new release"

    if [ -z "$after_pid" ]; then
        lc_fail "assertion 1/2/3: NO daemon is running after the upgrade"
    else
        after_daemon_ver="$(lc_daemon_version "$after_pid")"
        lc_say "after upgrade: daemon pid=$after_pid version=$after_daemon_ver client=$client_ver"

        # 1. the daemon is RESTARTED onto the NEW binary — not left on the old.
        lc_assert_ne "$before_pid" "$after_pid" "assertion 1: the daemon was restarted (pid)"
        lc_assert_eq "false" "$(lc_status_field "$bin" '.data.binary_stale')" \
            "assertion 1: the daemon is not running a since-replaced binary"

        # 2. client version == daemon version. Queried, not inferred: the
        #    daemon's version comes from the image it is ACTUALLY executing
        #    (/proc/<pid>/exe), so a daemon left on the old bytes cannot hide
        #    behind a new binary on disk. This is the #1921 skew condition.
        lc_assert_eq "$client_ver" "$after_daemon_ver" \
            "assertion 2: no version skew (client == daemon)"

        # 3. exactly ONE daemon; no orphan/second survived the restart.
        lc_assert_eq "1" "$(lc_daemon_count "$home")" "assertion 3: exactly one daemon afterwards"
    fi

    # 4. if a unit was installed, it is STILL the supervisor.
    if [ "$supervised" = yes ]; then
        lc_assert_eq "active" "$(lc_unit_active)" "assertion 4: the unit is still active"
        lc_assert_eq "true" "$(lc_status_field "$bin" '.data.autostart_unit')" \
            "assertion 4: af still sees the autostart unit"
        local unit_pid
        unit_pid="$(lc_unit_main_pid)"
        # The demotion this catches: a daemon still runs and still answers, but
        # it is an ad-hoc child systemd does not own — the unit's MainPID stops
        # matching the daemon that is actually serving (#796).
        if [ -n "$after_pid" ] && [ "$unit_pid" = "$after_pid" ]; then
            lc_pass "assertion 4: the running daemon IS the unit's child (MainPID=$unit_pid) — not demoted"
        else
            lc_fail "assertion 4: daemon DEMOTED to an ad-hoc child (unit MainPID=$unit_pid, running daemon=$after_pid, was $before_unit_pid)"
        fi
    fi

    # 5. pre-existing sessions survive.
    after_sessions="$(lc_session_count "$bin")"
    lc_assert_eq "$before_sessions" "$after_sessions" "assertion 5: sessions survive the upgrade"
    local lost
    lost="$(lc_lost_session_count "$bin")"
    lc_assert_eq "0" "$lost" "assertion 5: no session went Lost across the upgrade"

    # 6. doctor is the oracle: zero FAILs on the upgraded machine.
    local fails
    fails="$(lc_doctor_fail_count "$bin")"
    if [ -z "$fails" ]; then
        lc_fail "assertion 6: could not read a FAIL count out of af doctor"
    else
        lc_assert_eq "0" "$fails" "assertion 6: af doctor reports zero FAILs after the upgrade"
        [ "$fails" = "0" ] || lc_doctor_dump "$bin" | sed 's/^/[lifecycle]   | /' >&2
    fi

    lc_teardown_home "$home" "$supervised"
}

# lc_do_upgrade <mode> <bin> <home> <repo> <new-tag> — perform the upgrade the
# way the named path does it.
lc_do_upgrade() {
    local mode="$1" bin="$2" home="$3" repo="$4" new_tag="$5"

    # Fault injection: swap the binary to N and never restart the daemon. This
    # is what a broken upgrade looks like from the outside — and it is exactly
    # the machine #1921 shipped: new client, old daemon, everything "running".
    if [ "$LC_INJECT" = "skip-daemon-restart" ]; then
        lc_say "INJECT: swapping the binary to $new_tag WITHOUT restarting the daemon"
        lc_install_release "$new_tag" "$bin" || return 1
        return 0
    fi

    case "$mode" in
    upgrade-cmd)
        lc_say "running: af upgrade"
        local out
        out="$("$bin" upgrade 2>&1)"
        printf '%s\n' "$out" | sed 's/^/[lifecycle]   | /' >&2
        printf '%s' "$out" | grep -q 'Upgraded successfully' || return 1
        ;;
    launch)
        # The launch-time auto-update path (commands/root.go -> autoUpdateOnLaunch)
        # is gated on stdout being a TTY: it deliberately never fires for a
        # script or a pipe. So it can only be exercised through a real terminal
        # — a tmux pane — not a plain exec.
        lc_say "booting the TUI with auto-update enabled (launch-time path)"
        AGENT_FACTORY_AUTO_UPDATE=true \
            tmux new-session -d -s lc-b -x 100 -y 30 || return 1
        tmux send-keys -t lc-b \
            "export AGENT_FACTORY_HOME=$home AGENT_FACTORY_AUTO_UPDATE=true; cd $repo && $bin" Enter
        # Wait for the NEW version to be on disk: the launch path downloads,
        # installs, restarts the daemon, then re-execs into the new TUI.
        local i ok=1
        for ((i = 0; i < 240; i++)); do
            if [ "$(lc_client_version "$bin")" = "${new_tag#v}" ]; then
                ok=0
                break
            fi
            sleep 0.5
        done
        if [ "$ok" != 0 ]; then
            lc_say "launch-time auto-update never installed $new_tag; screen was:"
            lc_capture lc-b | sed 's/^/[lifecycle]   | /' >&2
            tmux kill-session -t lc-b 2>/dev/null
            return 1
        fi
        # The re-exec'd TUI must actually come back up.
        for ((i = 0; i < 120; i++)); do
            lc_capture lc-b | grep -q 'Agent Factory' && break
            sleep 0.5
        done
        # Give the restart-daemon step room to finish before we measure it.
        for ((i = 0; i < 60; i++)); do
            [ "$(lc_status_field "$bin" '.data.running')" = "true" ] && break
            sleep 0.5
        done
        tmux kill-session -t lc-b 2>/dev/null
        ;;
    *)
        lc_say "unknown upgrade mode '$mode'"
        return 1
        ;;
    esac
}

# lc_session_count <bin> — sessions the daemon knows about.
#
# Prints "unreadable" rather than 0 when the list cannot be parsed. A silent 0
# fallback here would be indistinguishable from a real empty machine, which
# would turn assertion 5 into "0 sessions survived: PASS" — the exact vacuous
# green this gate exists to prevent.
lc_session_count() {
    local out n
    out="$("$1" sessions list 2>/dev/null)" || {
        printf 'unreadable\n'
        return 0
    }
    n="$(printf '%s' "$out" | jq -r '[.[]?] | length' 2>/dev/null)" || {
        printf 'unreadable\n'
        return 0
    }
    [ -n "$n" ] || {
        printf 'unreadable\n'
        return 0
    }
    printf '%s\n' "$n"
}

# lc_lost_session_count <bin> — sessions whose worktree/tmux vanished. An
# upgrade that strands sessions is a data-loss bug, not a cosmetic one.
lc_lost_session_count() {
    "$1" sessions list 2>/dev/null |
        jq -r '[.[]? | select((.status|tostring) == "4" or (.liveness|tostring) == "4")] | length' 2>/dev/null ||
        printf '0\n'
}

# lc_teardown_home <home> [supervised] — stop what this scenario started so the
# next scenario's daemon count starts from zero. The container reaps everything
# anyway; a CI runner that runs several scenarios in one job does not.
lc_teardown_home() {
    local home="$1" supervised="${2:-no}" pid
    if [ "$supervised" = yes ] && lc_supervisor_available; then
        systemctl --user stop "$LC_UNIT_NAME" >/dev/null 2>&1 || true
        systemctl --user disable "$LC_UNIT_NAME" >/dev/null 2>&1 || true
        rm -f "$HOME/.config/systemd/user/$LC_UNIT_NAME" 2>/dev/null || true
        systemctl --user daemon-reload >/dev/null 2>&1 || true
    fi
    # Only ever signal daemons proven to serve THIS throwaway home.
    for pid in $(lc_daemon_pids "$home"); do
        kill -TERM "$pid" 2>/dev/null || true
    done
    sleep 1
    for pid in $(lc_daemon_pids "$home"); do
        kill -KILL "$pid" 2>/dev/null || true
    done
}

# ----------------------------------------------------------------------------
main() {
    local what="${1:-all}"

    lc_guard_disposable || exit 2
    mkdir -p "$LC_WORKSPACE"

    for t in curl tar jq git tmux; do
        command -v "$t" >/dev/null 2>&1 || {
            lc_say "missing required tool: $t"
            exit 2
        }
    done

    lc_say "af lifecycle gate — $(uname -s)/$(uname -m)"
    [ -n "$LC_INJECT" ] && lc_say "FAULT INJECTION ACTIVE: $LC_INJECT (assertions are EXPECTED to fail)"

    case "$what" in
    scenario-a) scenario_a ;;
    scenario-b)
        scenario_b upgrade-cmd
        scenario_b launch
        ;;
    all)
        scenario_a
        scenario_b upgrade-cmd
        scenario_b launch
        ;;
    *)
        lc_say "unknown scenario '$what' (want: scenario-a | scenario-b | all)"
        exit 2
        ;;
    esac

    lc_summary
}

main "$@"
