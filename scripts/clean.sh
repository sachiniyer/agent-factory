#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/clean.sh (-L <socket-name> | -S <socket-path> | --yes-really)

Remove legacy worktrees and ~/.agent-factory after safely stopping the target
tmux server. Cleanup is refused if that server has any live af_* sessions.
--yes-really selects the default tmux server; it does not override that guard.
EOF
}

fail() {
  printf 'clean.sh: %s\n' "$*" >&2
  exit 1
}

tmux_socket_args=()
explicit_socket=false
yes_really=false

set_socket() {
  local flag="$1"
  local value="$2"
  if [[ -z "$value" ]]; then
    fail "$flag requires a non-empty socket"
  fi
  if (( ${#tmux_socket_args[@]} != 0 )); then
    fail "choose exactly one tmux socket with -L or -S"
  fi
  tmux_socket_args=("$flag" "$value")
  explicit_socket=true
}

while (( $# > 0 )); do
  case "$1" in
    -L|-S)
      if (( $# < 2 )); then
        fail "$1 requires a socket"
      fi
      set_socket "$1" "$2"
      shift 2
      ;;
    -L?*)
      set_socket -L "${1#-L}"
      shift
      ;;
    -S?*)
      set_socket -S "${1#-S}"
      shift
      ;;
    --yes-really)
      yes_really=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument '$1' (expected -L, -S, or --yes-really)"
      ;;
  esac
done

if [[ "$explicit_socket" != true && "$yes_really" != true ]]; then
  fail "refusing an implicit default socket; pass -L <name>, -S <path>, or --yes-really"
fi

refuse_live_af_sessions() {
  local sessions="$1"
  local session
  while IFS= read -r session; do
    case "$session" in
      af_*)
        fail "refusing cleanup: live Agent Factory session '$session' exists on the target tmux socket"
        ;;
    esac
  done <<< "$sessions"
}

list_target_sessions() {
  tmux "${tmux_socket_args[@]}" list-sessions -F '#{session_name}' 2>/dev/null
}

# A missing server is already stopped. A reachable server is checked twice,
# immediately before teardown, so an af session observed on either side of the
# default-socket resolution aborts before any files are removed.
if sessions="$(list_target_sessions)"; then
  refuse_live_af_sessions "$sessions"

  if [[ "$explicit_socket" != true ]]; then
    socket_path="$(tmux display-message -p '#{socket_path}' 2>/dev/null)"
    if [[ -z "$socket_path" ]]; then
      fail "tmux did not report the default server's socket path"
    fi
    tmux_socket_args=(-S "$socket_path")
  fi

  if sessions="$(list_target_sessions)"; then
    refuse_live_af_sessions "$sessions"
    if (( ${#tmux_socket_args[@]} != 2 )); then
      fail "internal error: refusing to stop tmux without an explicit socket"
    fi
    tmux "${tmux_socket_args[@]}" kill-server 2>/dev/null || true
  fi
fi

: "${HOME:?HOME must be set to clean the Agent Factory state directory}"
rm -rf -- worktree*
rm -rf -- "$HOME/.agent-factory"
