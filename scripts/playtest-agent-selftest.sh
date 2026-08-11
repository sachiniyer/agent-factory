#!/usr/bin/env bash
# Cheap contract tests for the play-test sandbox's agent selection (#3177).
# No container or network is used: the Codex installer is replaced by a local
# fixture, while the bash stand-in is executed directly to prove its pane output.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIGURE="${AF_PLAYTEST_AGENT_CONFIGURE:-$HERE/container/configure-playtest-agent.sh}"
MARK_READY="${AF_PLAYTEST_MARK_READY:-$HERE/container/mark-playtest-ready.sh}"
PASS=0
FAIL=0

ok() {
    PASS=$((PASS + 1))
    printf '  PASS  %s\n' "$*"
}
no() {
    FAIL=$((FAIL + 1))
    printf '  FAIL  %s\n' "$*"
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# These shims fail if the self-test accidentally reaches for tools supplied by
# Dockerfile.test rather than by an ordinary host. The local-installer fixture
# below must stay network-free without requiring host curl; config generation
# must stay shell-only without requiring host jq.
HOST_TOOL_LOG="$WORK/host-tool.log"
POISON_BIN="$WORK/poison-bin"
mkdir -p "$POISON_BIN"
for tool in curl jq; do
    printf '#!/usr/bin/env sh\nprintf "%%s\\n" "%s" >>"$AF_PLAYTEST_HOST_TOOL_LOG"\nexit 97\n' \
        "$tool" >"$POISON_BIN/$tool"
    chmod +x "$POISON_BIN/$tool"
done

run_configure() {
    local home="$1"
    shift
    env HOME="$home" \
        AGENT_FACTORY_HOME="$home/sandbox/home" \
        AF_PLAYTEST_SANDBOX="$home/sandbox" \
        AF_PLAYTEST_HOST_TOOL_LOG="$HOST_TOOL_LOG" \
        PATH="$POISON_BIN:$PATH" \
        "$@" bash "$CONFIGURE"
}

printf '\n=== the default stand-in marks its own pane evidence ===\n'

STANDIN_HOME="$WORK/standin"
if run_configure "$STANDIN_HOME"; then
    config="$STANDIN_HOME/sandbox/home/config.json"
    standin="$STANDIN_HOME/bin/af-playtest-standin"
    if grep -qF '"claude": "'"$standin"'"' "$config"; then
        ok "the default config launches the marked stand-in"
    else
        no "the default config does not name the marked stand-in"
    fi
    pane_output="$("$standin" -c 'exit 0' 2>&1)"
    if grep -qF 'This pane is bash, not an agent.' <<<"$pane_output" &&
        grep -qF 'does not cover agent UI behavior' <<<"$pane_output"; then
        ok "captured stand-in pane output carries the caveat"
    else
        no "the stand-in pane can be captured without its caveat"
    fi
    pane_output="$(printf 'seq 1 80\nexit\n' | "$standin" 2>&1)"
    if tail -n 8 <<<"$pane_output" | grep -qF '[Play-test stand-in: bash]'; then
        ok "the stand-in marker remains visible after a screenful of output"
    else
        no "ordinary pane output can scroll the stand-in marker away"
    fi
    if grep -qF 'bash stand-in' "$STANDIN_HOME/sandbox/playtest-agent.txt"; then
        ok "the sandbox records that it is using a stand-in"
    else
        no "the sandbox does not record its evidence level"
    fi
else
    no "the default stand-in could not be configured"
fi

printf '\n=== a real Codex install is scriptable ===\n'

CODEX_HOME="$WORK/codex"
FAKE_INSTALLER="$WORK/install-codex.sh"
cat >"$FAKE_INSTALLER" <<'INSTALLER'
#!/usr/bin/env sh
set -eu
test "${CODEX_NON_INTERACTIVE:-}" = true
test "${CODEX_RELEASE:-}" = 0.147.0
mkdir -p "$CODEX_INSTALL_DIR"
cat >"$CODEX_INSTALL_DIR/codex" <<'CODEX'
#!/usr/bin/env sh
printf 'codex-cli 0.147.0\n'
CODEX
chmod +x "$CODEX_INSTALL_DIR/codex"
INSTALLER
chmod +x "$FAKE_INSTALLER"

if run_configure "$CODEX_HOME" \
    AF_PLAYTEST_AGENT=codex \
    AF_PLAYTEST_CODEX_RELEASE=0.147.0 \
    AF_PLAYTEST_CODEX_INSTALLER_URL="file://$FAKE_INSTALLER"; then
    config="$CODEX_HOME/sandbox/home/config.json"
    if grep -qF '"default_program": "codex"' "$config" &&
        grep -qF '"codex": "'"$CODEX_HOME"'/bin/codex"' "$config"; then
        ok "the real-agent config launches the installed Codex binary"
    else
        no "the real-agent config does not launch installed Codex"
    fi
    if grep -qF 'real agent: codex-cli 0.147.0' "$CODEX_HOME/sandbox/playtest-agent.txt"; then
        ok "the sandbox records the real agent and version"
    else
        no "the sandbox does not record the real Codex version"
    fi
else
    no "the scripted Codex install could not be configured"
fi

if [ ! -s "$HOST_TOOL_LOG" ]; then
    ok "the host self-test does not require jq or curl"
else
    no "the host self-test invoked container-only tools: $(tr '\n' ' ' <"$HOST_TOOL_LOG")"
fi

printf '\n=== detached readiness joins the complete scaffold ===\n'

READY_HOME="$WORK/ready"
mkdir -p "$READY_HOME/bin" "$READY_HOME/sandbox/home"
printf '#!/usr/bin/env sh\n' >"$READY_HOME/bin/af"
chmod +x "$READY_HOME/bin/af"
printf '{}\n' >"$READY_HOME/sandbox/home/config.json"
printf 'bash stand-in\n' >"$READY_HOME/sandbox/playtest-agent.txt"

if env HOME="$READY_HOME" bash "$MARK_READY" >/dev/null 2>&1; then
    no "an incomplete sandbox can publish readiness"
elif [ -e "$READY_HOME/sandbox/playtest-ready" ]; then
    no "a failed readiness check leaves a stale marker"
else
    ok "an incomplete sandbox cannot publish readiness"
fi

mkdir -p "$READY_HOME/sandbox/mock-repo/.git"
if env HOME="$READY_HOME" bash "$MARK_READY" &&
    grep -qF ready "$READY_HOME/sandbox/playtest-ready"; then
    ok "the complete scaffold publishes readiness atomically"
else
    no "the complete scaffold does not publish readiness"
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
