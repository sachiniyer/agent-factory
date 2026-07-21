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
#   * macOS/launchd. Assertion #4 is already written for it (lc_unit_active /
#     lc_unit_main_pid have Darwin branches, so the mac leg asserts the SAME
#     property against launchd). The blocker for turning on the macos-latest leg
#     is assertion 2: lc_daemon_version reads /proc/<pid>/exe, which macOS has no
#     equivalent of — `ps` reports the path, whose bytes are the NEW binary after
#     an upgrade, i.e. exactly the inference that assertion refuses to make. That
#     leg wants #1920's daemon-reported version.
#   * Assertion #4 (supervision) on container runs: no systemd inside, so it is
#     SKIPped there and only really runs on the CI runner. A SKIP is not a pass —
#     the run exits non-zero unless AF_LIFECYCLE_ALLOW_PARTIAL=1 acknowledges it.
#
# A NOTE ON ASSERTIONS THAT CANNOT FAIL
#
# This gate's whole thesis is that a green run which proves nothing is worse than
# no run. That standard applies to the gate itself, and it has already caught
# itself twice: an unscoped `sessions list` counted the wrong project's sessions
# and passed locally by accident, and the Lost check tested a write-never enum
# value so it could never match. Both are fixed. Every assertion here must be
# demonstrable via AF_LIFECYCLE_INJECT — if you add one, add the injection that
# makes it fail, and watch it fail.
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
LC_API_BASE="https://api.github.com/repos/$LC_REPO_SLUG/releases"
LC_DL="https://github.com/$LC_REPO_SLUG/releases/download"

# INJECTIONS — deliberately break the machine to prove an assertion fires.
# The gate's value is the bugs it catches NEXT, so it has to be shown catching
# one now. See the PR body / docs/lifecycle-testing.md.
#
#   skip-daemon-restart  swap the binary to N but never restart the daemon —
#                        exactly the #1921 skew condition (new client, old
#                        daemon). Assertions 1 and 2 must FAIL.
#   unhealthy-session    leave a session in a non-healthy state (archive it →
#                        Archived(6)/LiveArchived(5)). Assertion 5 must FAIL.
#                        NOT "kill its tmux": the daemon restores a killed pane
#                        within ~4s (#1108), so that injects nothing.
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
    local auth=() page=1 json n stable=() tag response status curl_rc
    [ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")

    # PAGINATE. A single ?per_page=100 fetch makes "100 is enough" a silent
    # assumption: this repo interleaves -preview-N prereleases with stables, so a
    # busy preview cycle can push the second-newest stable off page one. Walk
    # pages until two stables are found, and FAIL if the set cannot be
    # enumerated — never guess from page one and call it an upgrade matrix.
    while [ "$page" -le "${LC_RELEASE_MAX_PAGES:-10}" ]; do
        curl_rc=0
        response=$(curl -sSL --max-time 60 "${auth[@]}" \
            --write-out $'\n%{http_code}' \
            "$LC_API_BASE?per_page=100&page=$page" 2>/dev/null) || curl_rc=$?
        # Transport failures and GitHub availability/quota responses mean the
        # release boundary could not be verified. Status 2 is deliberately
        # distinct from a malformed release set (status 1), which is a real
        # gate failure. scenario_b records 2 as SKIP, never PASS (#2262).
        [ "$curl_rc" = 0 ] || return 2
        status="${response##*$'\n'}"
        json="${response%$'\n'*}"
        lc_release_http_unavailable "$status" && return 2
        [ "$status" = 200 ] || return 1
        # A non-array (rate-limit object, error envelope) must not read as "no
        # more releases" — that would end the walk and look like exhaustion.
        [ "$(printf '%s' "$json" | jq -r 'type' 2>/dev/null)" = "array" ] || return 1
        n=$(printf '%s' "$json" | jq -r 'length' 2>/dev/null)
        [ -n "$n" ] || return 1
        while IFS= read -r tag; do
            [ -n "$tag" ] && stable+=("$tag")
        done < <(printf '%s' "$json" |
            jq -r '.[] | select(.draft==false and .prerelease==false) | .tag_name' 2>/dev/null)
        [ "${#stable[@]}" -ge 2 ] && break
        # A short page is the last page.
        [ "$n" -lt 100 ] && break
        page=$((page + 1))
    done

    # Two distinct releases are required: with fewer, "upgrade from N-1 to N" has
    # nothing to upgrade across.
    [ "${#stable[@]}" -ge 2 ] || return 1
    # The API lists newest-first; the caller wants "N-1<TAB>N".
    printf '%s\t%s\n' "${stable[1]}" "${stable[0]}"
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
    # coped with a clean one. Machine-level facts here; each probe re-asserts its
    # OWN home is virgin immediately before touching it (lc_assert_virgin).
    if lc_supervisor_available && [ "$(lc_unit_active)" = "active" ]; then
        lc_fail "precondition: an autostart unit is already active"
    else
        lc_pass "precondition: no active autostart unit"
    fi

    # A fresh home has no throttle record, so the launch path would try to
    # self-update and re-exec mid-scenario. Scenario A is about the clean boot,
    # not the update; scenario B owns that path explicitly.
    export AGENT_FACTORY_AUTO_UPDATE=false

    # ---- probe 1: doctor, on a machine NOTHING has touched --------------------
    #
    # Its own home, because AN OBSERVATION THAT CREATES THE STATE IT OBSERVES IS
    # NOT AN OBSERVATION. `af doctor` materializes the home directory and
    # agent-factory.log (measured: `af version` touches nothing, doctor and
    # `daemon status` both create home/ + the log). Running doctor and then the
    # TUI against ONE home means the TUI's "virgin home" claim is measuring a
    # home doctor already built — and the preconditions asserted at the top of
    # the scenario only ever covered the first probe.
    #
    # Two pristine homes, each with its precondition checked IMMEDIATELY before
    # the probe that depends on it, is the only version of this that cannot rot:
    # it holds no matter which command materializes what, on either side of the
    # upgrade boundary.
    local dhome="$ws/home-doctor"
    export AGENT_FACTORY_HOME="$dhome"
    lc_assert_virgin "$dhome" "doctor probe"

    # af version must not even create the home (it is the one command that can
    # be run before anything exists).
    if "$bin" version >/dev/null 2>&1; then
        lc_pass "af version exits 0 in a virgin environment"
    else
        lc_fail "af version failed in a virgin environment"
    fi
    if [ ! -e "$dhome" ]; then
        lc_pass "af version did not materialize the home (nothing to observe yet)"
    else
        lc_fail "af version created $dhome — the doctor probe below is no longer virgin"
    fi

    # doctor is the new user's first stop: it must run, and must not report a
    # FAIL that a brand-new user cannot act on. (Missing agents are WARNs with a
    # fix line, which is correct — they are not FAILs.)
    local fails
    fails="$(lc_doctor_fail_count "$bin")"
    if [ -z "$fails" ]; then
        lc_fail "scenario-a: could not read a FAIL count out of af doctor"
    else
        lc_assert_eq "0" "$fails" "af doctor reports no FAIL on a brand-new machine"
        [ "$fails" = "0" ] || lc_doctor_dump "$bin" | sed 's/^/[lifecycle]   | /' >&2
    fi
    lc_teardown_home "$dhome"

    # ---- probe 2: the TUI, on a SECOND machine nothing has touched ------------
    #
    # A new user's literal first act. Pristine again — doctor's home above is
    # not reused, so "virgin" here is a fact rather than a hope.
    export AGENT_FACTORY_HOME="$home"
    lc_assert_virgin "$home" "TUI probe"
    if lc_boot_tui "$bin" "$home" "$repo" lc-a; then
        lc_pass "the TUI renders its first frame on a virgin home"
        local screen
        screen="$(lc_capture lc-a)"
        # AUDIT: "grep found no panic" passes on an EMPTY screen — a dead pane
        # would sail through the no-panic check. Prove there is a screen to
        # search before concluding anything about what is not on it.
        if ! printf '%s' "$screen" | grep -q 'Agent Factory'; then
            lc_fail "no-panic check has nothing to read: the captured screen is empty/not the TUI"
        elif printf '%s' "$screen" | grep -qiE 'panic:|fatal error|runtime error'; then
            lc_fail "the TUI first frame contains a panic/fatal error"
            printf '%s' "$screen" | sed 's/^/[lifecycle]   | /' >&2
        else
            lc_pass "no panic on the virgin first frame (searched $(printf '%s' "$screen" | wc -l) captured lines)"
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

    # AUDIT: an exit status is NOT evidence a session exists. `af sessions create`
    # can answer 0 while its JSON body carries a refusal ("claude is not
    # installed…") — observed on this very harness. So assert the OUTCOME: the
    # daemon lists it.
    local c_out c_rc=0
    c_out="$(cd "$repo" && "$bin" sessions create --name clean1 2>&1)" || c_rc=$?
    local n_after
    n_after="$(lc_wait_session_count "$bin" "$repo" 1 30)"
    if [ "$n_after" = "1" ]; then
        lc_pass "first session created on a clean machine (daemon lists it)"
    else
        lc_fail "first session not created on a clean machine (create rc=$c_rc, daemon lists '$n_after')"
        printf '%s\n' "$c_out" | head -3 | sed 's/^/[lifecycle]   | /' >&2
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

    local tags old_tag new_tag tags_rc=0
    tags="$(lc_two_newest_stable)" || tags_rc=$?
    if [ "$tags_rc" = 2 ]; then
        lc_skip "scenario-b/$mode: could not verify the release boundary — GitHub release API unavailable or rate-limited"
        return 0
    elif [ "$tags_rc" != 0 ]; then
        lc_fail "scenario-b/$mode: could not resolve two usable stable releases"
        return 1
    fi
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
        # AUDIT: trusting `daemon install`'s exit code would let a no-op install
        # set supervised=yes, and assertion 4 would then "pass" against a unit
        # that was never registered. The unit file has to actually exist.
        if "$bin" daemon install >/dev/null 2>&1 &&
            [ "$(lc_status_field "$bin" '.data.autostart_unit')" = "true" ]; then
            supervised=yes
            lc_pass "autostart unit installed (this machine can supervise)"
        else
            lc_fail "af daemon install did not register a unit on a machine with a service manager"
            "$bin" daemon install 2>&1 | head -3 | sed 's/^/[lifecycle]   | /' >&2
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

    # Wait for the sessions to actually EXIST rather than reading the list the
    # instant create returns. `sessions create` answers with the record, which
    # is not the same fact as "the daemon has persisted it and will list it" —
    # so a read here is a race, and it is the kind that passes on a fast box and
    # fails on a loaded CI runner. Same rule the TUI driver follows (#1161):
    # wait on the condition, never on a sleep.
    local before_sessions before_pid before_daemon_ver
    before_sessions="$(lc_wait_session_count "$bin" "$repo" 2 30)"
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
        "$bin" sessions list --repo "$repo" 2>&1 | head -5 | sed 's/^/[lifecycle]   | /' >&2
        lc_say "config.toml [program_overrides] (config get does not know this key on older releases):"
        grep -A2 'program_overrides' "$home/config.toml" 2>/dev/null |
            head -3 | sed 's/^/[lifecycle]   | /' >&2
        # The daemon's own log is the only witness that can explain a session
        # that was created and then vanished from the list.
        lc_say "daemon log (tail):"
        tail -25 "$home/agent-factory.log" 2>/dev/null | sed 's/^/[lifecycle]   | /' >&2
        lc_say "tmux sessions the daemon owns:"
        tmux ls 2>&1 | head -5 | sed 's/^/[lifecycle]   | /' >&2
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
    local upgrade_rc=0
    lc_do_upgrade "$mode" "$bin" "$home" "$repo" "$new_tag" || upgrade_rc=$?
    if [ "$upgrade_rc" = 2 ]; then
        lc_skip "scenario-b/$mode: could not verify the upgrade — GitHub release lookup unavailable or rate-limited"
        lc_teardown_home "$home" "$supervised"
        return 0
    elif [ "$upgrade_rc" != 0 ]; then
        lc_fail "scenario-b/$mode: the upgrade step itself failed"
        lc_teardown_home "$home" "$supervised"
        return 1
    fi

    # Fault injection: leave a session in a state that is NOT healthy, to prove
    # assertion 5 can actually FIRE. The check it replaced tested status/liveness
    # == 4 (Dead/LiveDead) — both write-never since #1108 — so it matched nothing
    # on any machine and passed forever. An assertion nobody has watched fail is
    # not evidence, so this is the fire-drill for it.
    #
    # Archive rather than "kill the tmux and call it Lost": a killed pane does
    # NOT produce Lost, because the daemon RESTORES it within ~4s (measured:
    # Running→Ready, and it heals even with the worktree deleted — #1108's
    # restore loop). Lost is therefore not artificially reachable against a live
    # daemon, and an injection that quietly fails to inject would be the very
    # vacuity this gate exists to prevent. Archive is durable, and it lands on
    # Archived(6)/LiveArchived(5) — a real non-healthy state the predicate must
    # catch, which also confirms the enum ordinals end-to-end.
    if [ "$LC_INJECT" = "unhealthy-session" ]; then
        lc_say "INJECT: archiving 'pre1' so it is no longer in a healthy state"
        local arc_out arc_rc=0
        arc_out="$("$bin" sessions archive pre1 --repo "$repo" 2>&1)" || arc_rc=$?
        local j applied=1
        for ((j = 0; j < 40; j++)); do
            [ "$(lc_unhealthy_session_count "$bin" "$repo")" != "0" ] && {
                applied=0
                break
            }
            sleep 0.5
        done
        # A fault injection whose SETUP silently no-ops is the worst case of all:
        # every assertion downstream then reports on a scenario that never
        # existed, and the run looks like proof. errexit is off here (assertions
        # must accumulate), so the archive's status is checked explicitly rather
        # than assumed — and the OUTCOME is confirmed, not just the exit code: a
        # CLI that returns 0 without archiving would still be caught.
        if [ "$applied" != 0 ]; then
            lc_fail "INJECT unhealthy-session: the injection DID NOT APPLY (archive rc=$arc_rc) — refusing to report on a scenario that was never set up"
            printf '%s\n' "$arc_out" | head -3 | sed 's/^/[lifecycle]   | /' >&2
            lc_say "session states: $(lc_session_states "$bin" "$repo")"
            lc_teardown_home "$home" "$supervised"
            return 1
        fi
        lc_note_injection_applied
        lc_say "INJECT: applied — session states now: $(lc_session_states "$bin" "$repo")"
    fi

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
        #
        #    AUDIT: `lc_assert_eq "" ""` PASSES. If lc_daemon_version could not
        #    read /proc/<pid>/exe (a permission change, a macOS runner, a dead
        #    pid) and lc_client_version also came back empty, the single most
        #    important assertion in this gate would report "no skew" having
        #    compared nothing against nothing. Both operands must exist first.
        if [ -z "$after_daemon_ver" ] || [ -z "$client_ver" ]; then
            lc_fail "assertion 2: cannot compare versions (daemon='$after_daemon_ver', client='$client_ver') — refusing to report 'no skew' from unreadable values"
        else
            lc_assert_eq "$client_ver" "$after_daemon_ver" \
                "assertion 2: no version skew (client == daemon)"
        fi

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
    after_sessions="$(lc_wait_session_count "$bin" "$repo" "$before_sessions" 30)"
    lc_assert_eq "$before_sessions" "$after_sessions" "assertion 5: sessions survive the upgrade"
    local unhealthy
    unhealthy="$(lc_unhealthy_session_count "$bin" "$repo")"
    lc_assert_eq "0" "$unhealthy" "assertion 5: every session is still healthy (none Lost/Dead) across the upgrade"
    if [ "$unhealthy" != "0" ]; then
        lc_say "session states: $(lc_session_states "$bin" "$repo")"
    fi

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
        lc_install_release "$new_tag" "$bin" || {
            lc_fail "INJECT skip-daemon-restart: could not install $new_tag — the injection did not apply"
            return 1
        }
        # Confirm the OUTCOME, not just that the download returned 0: if the
        # binary on disk is not actually N, there is no skew to detect and every
        # assertion below would report on a machine that was never broken.
        local got
        got="$(lc_client_version "$bin")"
        if [ "$got" != "${new_tag#v}" ]; then
            lc_fail "INJECT skip-daemon-restart: binary is '$got', expected '${new_tag#v}' — the injection did not apply"
            return 1
        fi
        lc_note_injection_applied
        lc_say "INJECT: applied — client is now $got, daemon deliberately left on the old image"
        return 0
    fi

    case "$mode" in
    upgrade-cmd)
        lc_say "running: af upgrade"
        local out rc=0
        out="$("$bin" upgrade 2>&1)" || rc=$?
        printf '%s\n' "$out" | sed 's/^/[lifecycle]   | /' >&2
        if [ "$rc" != 0 ] && lc_release_lookup_unavailable "$out"; then
            lc_say "GitHub release lookup was unavailable; upgrade behavior was not exercised"
            return 2
        fi
        [ "$rc" = 0 ] || return 1
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
            if [ -f "$home/agent-factory.log" ] && \
                lc_release_lookup_unavailable "$(tail -40 "$home/agent-factory.log" 2>/dev/null)"; then
                lc_say "GitHub release lookup was unavailable; launch-time upgrade behavior was not exercised"
                tmux kill-session -t lc-b 2>/dev/null
                return 2
            fi
            sleep 0.5
        done
        if [ "$ok" != 0 ]; then
            lc_say "launch-time auto-update never installed $new_tag; screen was:"
            lc_capture lc-b | sed 's/^/[lifecycle]   | /' >&2
            tmux kill-session -t lc-b 2>/dev/null
            return 1
        fi
        # The re-exec'd TUI must actually come back up. A wait that merely
        # stops waiting is not an assertion: if the re-exec left the user with
        # no TUI, giving up quietly here would let every downstream check run
        # against a machine we never verified and report green.
        ok=1
        for ((i = 0; i < 120; i++)); do
            if lc_capture lc-b | grep -q 'Agent Factory'; then
                ok=0
                break
            fi
            sleep 0.5
        done
        if [ "$ok" != 0 ]; then
            lc_say "the re-exec'd TUI never rendered after the launch-time update; screen was:"
            lc_capture lc-b | sed 's/^/[lifecycle]   | /' >&2
            tmux kill-session -t lc-b 2>/dev/null
            return 1
        fi

        # The daemon must come back. Timing out here means the launch path left
        # the machine with NO daemon — the loudest possible outcome, not a
        # reason to proceed and measure nothing.
        ok=1
        for ((i = 0; i < 60; i++)); do
            if [ "$(lc_status_field "$bin" '.data.running')" = "true" ]; then
                ok=0
                break
            fi
            sleep 0.5
        done
        if [ "$ok" != 0 ]; then
            lc_say "no daemon is running 30s after the launch-time update finished"
            "$bin" daemon status 2>&1 | head -5 | sed 's/^/[lifecycle]   | /' >&2
            tmux kill-session -t lc-b 2>/dev/null
            return 1
        fi
        tmux kill-session -t lc-b 2>/dev/null
        ;;
    *)
        lc_say "unknown upgrade mode '$mode'"
        return 1
        ;;
    esac
}

# lc_session_count <bin> <repo> — sessions the daemon knows about FOR THIS REPO.
#
# `--repo` is not decoration. `af sessions list` resolves its project from the
# CURRENT DIRECTORY when the flag is absent, and this script runs from the af
# checkout — so an unscoped list asks "what sessions exist in agent-factory?",
# gets the correct answer [], and reports zero sessions on a machine that has
# two. That read cost a whole debugging cycle: it passed locally, where /src is
# an af WORKTREE whose .git file points outside the container, so git fails,
# no project resolves, and af falls back to listing everything — the right
# number for the wrong reason. On a clean CI checkout the resolution works and
# the bug appears. The flag exists on both sides of the upgrade boundary
# (verified against v1.0.193), so scope it explicitly and depend on nothing.
#
# Prints "unreadable" rather than 0 when the list cannot be parsed. A silent 0
# fallback would be indistinguishable from a real empty machine, turning
# assertion 5 into "0 sessions survived: PASS" — the vacuous green this gate
# exists to prevent.
lc_session_count() {
    local bin="$1" repo="$2" out n
    out="$("$bin" sessions list --repo "$repo" 2>/dev/null)" || {
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

# lc_wait_session_count <bin> <want> <timeout-s> — poll until the daemon lists
# <want> sessions, then print the count (or the last count seen on timeout, so
# the caller reports what was actually there rather than a fabricated number).
lc_wait_session_count() {
    local bin="$1" repo="$2" want="$3" timeout="${4:-30}" i n=""
    for ((i = 0; i < timeout * 2; i++)); do
        n="$(lc_session_count "$bin" "$repo")"
        [ "$n" = "$want" ] && break
        sleep 0.5
    done
    printf '%s\n' "$n"
}

# The enum ordinals af serializes, from session/instance.go and
# session/liveness.go. Status: Running=0 Ready=1 Loading=2 Deleting=3 Dead=4
# Lost=5 Archived=6. Liveness: Unset=0 LiveRunning=1 LiveReady=2 LiveLost=3
# LiveDead=4 LiveArchived=5 LiveLimitReached=6.
#
# These are safe to hardcode: Status serializes as an int, and the enum's own
# contract is "appending, never renumbering, is what keeps old records
# readable" (session/instance.go). They are NOT safe to guess — the first cut of
# this helper tested 4 on both axes, which is Dead/LiveDead, and #1108 made BOTH
# of those write-never (observed deaths record Lost; FromInstanceData rewrites
# persisted Dead to Lost on load). So the check could not match anything, ever:
# a vacuous assertion reporting "0 sessions Lost: PASS" on any machine at all,
# including one where every session had been destroyed.
LC_STATUS_HEALTHY='0,1'   # Running, Ready
LC_LIVENESS_HEALTHY='1,2' # LiveRunning, LiveReady

# lc_unhealthy_session_count <bin> <repo> — sessions NOT in a healthy state.
#
# Deliberately fail-CLOSED: it counts anything that is not provably healthy,
# rather than enumerating the bad values. Enumerating bad values fails OPEN — if
# the enum ever shifts, or a state we never thought of appears, a
# "select(status == 5)" check silently matches nothing and passes. Asking "is
# every session in a state I recognise as healthy?" turns that same drift into a
# LOUD failure instead of a quiet green. Lost(5) is the state this is really
# hunting (an upgrade that strands sessions is data loss, not cosmetics), but it
# is caught as "not healthy", not as "== 5".
lc_unhealthy_session_count() {
    local bin="$1" repo="$2" out n
    out="$("$bin" sessions list --repo "$repo" 2>/dev/null)" || {
        printf 'unreadable\n'
        return 0
    }
    # `. as $r` is load-bearing: inside `$st | index(.status)` the dot rebinds to
    # $st, so the row has to be captured before the pipe or jq errors out.
    n="$(printf '%s' "$out" | jq -r --argjson st "[$LC_STATUS_HEALTHY]" \
        --argjson lv "[$LC_LIVENESS_HEALTHY]" \
        '[.[]? | . as $r | select(($st | index($r.status)) == null or ($lv | index($r.liveness)) == null)] | length' \
        2>/dev/null)" || {
        printf 'unreadable\n'
        return 0
    }
    [ -n "$n" ] || {
        printf 'unreadable\n'
        return 0
    }
    printf '%s\n' "$n"
}

# lc_session_states <bin> <repo> — "title:status/liveness" per session, for the
# log when the check above trips. A bare count cannot tell you WHICH state.
lc_session_states() {
    "$1" sessions list --repo "$2" 2>/dev/null |
        jq -r '.[]? | "\(.title):status=\(.status)/liveness=\(.liveness)"' 2>/dev/null |
        tr '\n' ' '
}

# lc_teardown_home <home> [supervised] — stop what this scenario started so the
# next scenario's daemon count starts from zero. The container reaps everything
# anyway; a CI runner that runs several scenarios in one job does not.
lc_teardown_home() {
    local home="$1" supervised="${2:-no}" pid
    # Belt to lc_daemon_pids' braces: this function sends SIGKILL, so it states
    # its own precondition rather than inheriting one.
    if [ -z "$home" ]; then
        lc_fail "lc_teardown_home called with an empty home — refusing to signal anything"
        return 1
    fi
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
    # Validate BEFORE running anything: an unrecognized injection injects
    # nothing, and a green run would then claim we survived a fault never applied.
    lc_validate_injection "$LC_INJECT" || exit 2
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

    # An injection asked for but never reached is a harness defect, not a pass.
    lc_assert_injection_ran "$LC_INJECT"

    lc_summary
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    main "$@"
fi
