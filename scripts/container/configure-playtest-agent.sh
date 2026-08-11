#!/usr/bin/env bash
# Configure the program a play-test pane launches. The default remains a cheap
# shell, but the shell identifies itself inside every pane. Set
# AF_PLAYTEST_AGENT=codex to install and launch a real Codex CLI instead.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SANDBOX="${AF_PLAYTEST_SANDBOX:-$HOME/sandbox}"
AF_HOME="${AGENT_FACTORY_HOME:-$SANDBOX/home}"
BIN_DIR="$HOME/bin"
EVIDENCE_FILE="$SANDBOX/playtest-agent.txt"
KIND_FILE="$SANDBOX/playtest-agent-kind"
AGENT="${AF_PLAYTEST_AGENT:-standin}"

mkdir -p "$AF_HOME" "$BIN_DIR" "$SANDBOX"

json_quote() {
    local value="$1"
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    value="${value//$'\b'/\\b}"
    value="${value//$'\f'/\\f}"
    value="${value//$'\n'/\\n}"
    value="${value//$'\r'/\\r}"
    value="${value//$'\t'/\\t}"
    printf '"%s"' "$value"
}

write_config() {
    local program="$1" command="$2"
    local program_json command_json
    program_json="$(json_quote "$program")"
    command_json="$(json_quote "$command")"
    printf '{\n  "default_program": %s,\n  "program_overrides": {\n    %s: %s\n  }\n}\n' \
        "$program_json" "$program_json" "$command_json" >"$AF_HOME/config.json"
}

case "$AGENT" in
standin | bash)
    standin="$BIN_DIR/af-playtest-standin"
    cat >"$standin" <<'STANDIN'
#!/usr/bin/env bash
cat >&2 <<'NOTICE'

===============================================================
Play-test stand-in
This pane is bash, not an agent.
Evidence from this pane does not cover agent UI behavior such as
the composer, modal state, paste handling, status footer, or turns.
Use AF_PLAYTEST_AGENT=codex for a real-agent play-test.
===============================================================

NOTICE
if [ "$#" -gt 0 ]; then
    exec bash "$@"
fi
export PS1='[Play-test stand-in: bash] \w \$ '
exec bash --noprofile --norc -i
STANDIN
    chmod +x "$standin"
    write_config claude "$standin"
    printf '%s\n' 'bash stand-in: not a real agent; every pane prints its own caveat' \
        >"$EVIDENCE_FILE"
    printf '%s\n' standin >"$KIND_FILE"
    ;;
codex)
    AF_PLAYTEST_CODEX_RELEASE="${AF_PLAYTEST_CODEX_RELEASE:-latest}" \
        bash "$SCRIPT_DIR/install-playtest-codex.sh"
    codex_bin="$BIN_DIR/codex"
    if [ ! -x "$codex_bin" ]; then
        echo "play-test: Codex installer did not create $codex_bin" >&2
        exit 1
    fi
    version="$($codex_bin --version)"
    write_config codex "$codex_bin"
    printf 'real agent: %s\n' "$version" >"$EVIDENCE_FILE"
    printf '%s\n' real >"$KIND_FILE"
    ;;
*)
    echo "play-test: unknown AF_PLAYTEST_AGENT '$AGENT' (want standin, bash, or codex)" >&2
    exit 2
    ;;
esac
