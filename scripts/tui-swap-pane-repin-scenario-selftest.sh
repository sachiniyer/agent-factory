#!/usr/bin/env bash
# Host-safe control for scripts/tui-swap-pane-repin-scenario.sh (#5730): no
# tmux, no container, no af. It extracts run_leg_once and its helpers from the
# scenario and runs them against stubs, in the SAME call shape the scenario
# uses (`run_leg_once ... || ...`, where bash ignores set -e), to prove that an
# inner step failing now fails the leg instead of being swallowed.
#
#   scripts/tui-swap-pane-repin-scenario-selftest.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
scenario="$here/tui-swap-pane-repin-scenario.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Pull the function definitions under test out of the scenario verbatim.
defs="$(awk '/^leg_step\(\) \{/{p=1} p{print} /^run_leg_once\(\) \{/{r=1} r && /^\}/{exit}' "$scenario")"
[ -n "$defs" ] || { echo "selftest: could not extract run_leg_once from $scenario" >&2; exit 2; }

# Stubs for everything the leg calls that needs a TUI.
_af_fail() { echo "  (leg failure) $*" >&2; }
_af_log() { :; }
af_reset_sandbox() { :; }
af_set_config() { :; }
af_boot() { :; }
af_new_instance() { :; }
af_select() { :; }
af_focus_tree() { :; }
af_wait_for() { :; }
af_send() { :; }
af_open_pane() { :; }
tui_provenance() { :; }
selected_name() { echo dup; }
active_tab_label() { echo a; }
af_capture() { echo screen; }
AF_DRIVER_TIMEOUT=1 AF_DRIVER_POLL=0
export AGENT_FACTORY_HOME="$work/home" AF_DRIVER_REPO="$work"
mkdir -p "$AGENT_FACTORY_HOME"
: >"$AGENT_FACTORY_HOME/agent-factory.log"
EVID="$work/evid"
mkdir -p "$EVID"
sleep() { :; }

# shellcheck disable=SC1090
eval "$defs"

# A fake af that fails the Nth invocation matching FAKE_FAIL_ON ("" = never).
fake="$work/af"
cat >"$fake" <<'F'
#!/usr/bin/env bash
if [ -n "${FAKE_FAIL_ON:-}" ] && [[ "$*" == *"$FAKE_FAIL_ON"* ]]; then
    n=$(( $(cat "$FAKE_COUNT" 2>/dev/null || echo 0) + 1 ))
    echo "$n" >"$FAKE_COUNT"
    [ "$n" -ge "${FAKE_FAIL_NTH:-1}" ] && exit 1
fi
exit 0
F
chmod +x "$fake"
export FAKE_COUNT="$work/count"

run() { # runs the leg exactly as run_leg does: under ||, where errexit is off
    local rc=0
    rm -f "$EVID/x-results.tsv" "$FAKE_COUNT"
    run_leg_once x "$fake" || rc=$?
    echo "$rc"
}

fail=0
check() { # <name> <want-rc: 0|nonzero> <got-rc>
    if { [ "$2" = 0 ] && [ "$3" = 0 ]; } || { [ "$2" = nonzero ] && [ "$3" != 0 ]; }; then
        echo "ok   $1"
    else
        echo "FAIL $1 (rc=$3, want $2)"
        fail=1
    fi
}

# Baseline: nothing fails -> the leg succeeds and records its result row.
rc="$(FAKE_FAIL_ON="" run 2>/dev/null)"
check "healthy leg passes" 0 "$rc"
[ -s "$EVID/x-results.tsv" ] || { echo "FAIL healthy leg wrote no results row"; fail=1; }

# Deliberate-failure control: the replacement's FINAL `tab-create ... --name a`
# fails (the 2nd 'name a' call; the 1st is the corpse roster). Before the fix the
# leg swallowed this and still recorded a result.
rc="$(FAKE_FAIL_ON="--name a" FAKE_FAIL_NTH=2 run 2>/dev/null)"
check "failed replacement tab-create a fails the leg" nonzero "$rc"
[ ! -s "$EVID/x-results.tsv" ] || { echo "FAIL failing leg still recorded a result"; fail=1; }

# Corpse roster step failing (the 1st 'name a').
rc="$(FAKE_FAIL_ON="--name a" FAKE_FAIL_NTH=1 run 2>/dev/null)"
check "failed corpse tab-create a fails the leg" nonzero "$rc"

# A failing setup helper (af_new_instance) must fail the leg too.
af_new_instance() { return 1; }
rc="$(FAKE_FAIL_ON="" run 2>/dev/null)"
check "failed af_new_instance fails the leg" nonzero "$rc"

# A failing capture must fail the leg.
af_new_instance() { :; }
af_capture() { return 1; }
rc="$(FAKE_FAIL_ON="" run 2>/dev/null)"
check "failed af_capture fails the leg" nonzero "$rc"

exit "$fail"
