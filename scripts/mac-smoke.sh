#!/usr/bin/env bash
# mac-smoke.sh — run the real `af` binary through its core journey on a Mac and
# print a pass/fail report.
#
# WHY THIS EXISTS (#6384)
#
# GitHub's hosted macOS runners have SIP disabled; every user's Mac has it
# enabled, so a class of real-device behaviour (e.g. #3584: process ownership
# unreadable for Apple system binaries) is invisible to CI. This script is how
# SIP-on behaviour is covered: run it on a real Mac and paste the report on
# #6384. The macOS CI job (.github/workflows/mac-e2e.yml) runs the same script.
#
# WHAT IT TOUCHES
#
# Nothing of your real af install. Everything lives under one throwaway root
# (mktemp, kept short: macOS unix sockets are limited to 104 bytes):
#   - AGENT_FACTORY_HOME and HOME point inside it, so your config, state, daemon
#     and ~/Library/LaunchAgents are not read or written;
#   - tmux runs on a private server (TMUX_TMPDIR inside the root, $TMUX unset);
#   - the web UI binds a free loopback port, not 8443;
#   - the "agent" is a stand-in shell script, no credentials involved.
# On exit it stops only the daemon and tmux server it started, then deletes the
# root (use --keep to keep it for inspection).
#
# Usage:
#   scripts/mac-smoke.sh [--keep] [--af /path/to/af]
#
# Needs: bash 3.2+, git, tmux (3.2+), curl. Exit 0 = every step passed.
# Written for bash 3.2 (/bin/bash on macOS): no associative arrays, mapfile, or
# ${var,,}.

set -u

KEEP=0
AF_BIN="${AF_BIN:-}"
while [ $# -gt 0 ]; do
	case "$1" in
	--keep) KEEP=1 ;;
	--af)
		AF_BIN="${2:-}"
		shift
		;;
	-h | --help)
		awk 'NR==1{next} /^#/{sub(/^# ?/,""); print; next} {exit}' "$0"
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		exit 2
		;;
	esac
	shift
done

if [ -z "$AF_BIN" ]; then
	AF_BIN="$(command -v af || true)"
fi
if [ -z "$AF_BIN" ] || [ ! -x "$AF_BIN" ]; then
	echo "mac-smoke: no af binary (pass --af /path/to/af or put af on PATH)" >&2
	exit 2
fi
case "$AF_BIN" in /*) ;; *) AF_BIN="$(cd "$(dirname "$AF_BIN")" && pwd)/$(basename "$AF_BIN")" ;; esac
for tool in git tmux curl; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "mac-smoke: $tool is required but not on PATH" >&2
		exit 2
	}
done

# --- throwaway root ---------------------------------------------------------
# /tmp, not $TMPDIR: $TMPDIR on macOS is /var/folders/xx/yyyy/T/ and the af and
# tmux socket paths under it plus our names must stay under 104 bytes.
ROOT="$(mktemp -d /tmp/afms.XXXXXX)" || exit 2
ROOT="$(cd -P "$ROOT" && pwd)" # /tmp -> /private/tmp on macOS; compare real paths
export AGENT_FACTORY_HOME="$ROOT/home"
export HOME="$ROOT/user"
export TMUX_TMPDIR="$ROOT/tmux"
export GIT_CONFIG_GLOBAL="$ROOT/gitconfig"
export GIT_AUTHOR_NAME="af smoke" GIT_AUTHOR_EMAIL="smoke@example.invalid"
export GIT_COMMITTER_NAME="af smoke" GIT_COMMITTER_EMAIL="smoke@example.invalid"
unset TMUX TMUX_PANE AGENT_FACTORY_SESSION AF_SESSION AF_DAEMON_URL AF_DAEMON_TOKEN
mkdir -p "$AGENT_FACTORY_HOME" "$HOME" "$TMUX_TMPDIR" "$ROOT/bin" "$ROOT/repo"
chmod 700 "$TMUX_TMPDIR"
TMUX_SOCK="$TMUX_TMPDIR/tmux-$(id -u)/default"
: >"$GIT_CONFIG_GLOBAL"

# Stand-in agent: prints a ready prompt (af waits for one) then reads stdin.
# Named "claude" and found on PATH because an account-scoped agent is refused
# when its program is written as a path.
cat >"$ROOT/bin/claude" <<'EOF'
#!/bin/sh
printf '❯ '
exec cat
EOF
chmod +x "$ROOT/bin/claude"
export PATH="$ROOT/bin:$PATH"

PORT="$(
	# A free loopback port, picked by the kernel via a throwaway listener is not
	# available in bare bash; probe a few candidates instead.
	p=18443
	while [ "$p" -lt 18543 ]; do
		if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
			echo "$p"
			break
		fi
		p=$((p + 1))
	done
)"
[ -n "$PORT" ] || {
	echo "mac-smoke: no free loopback port in 18443-18542" >&2
	exit 2
}

# --- reporting --------------------------------------------------------------
PASSED=0
FAILED=0
REPORT=""
LOG="$ROOT/smoke.log"
: >"$LOG"

record() { # record <PASS|FAIL> <name> [detail]
	if [ "$1" = PASS ]; then PASSED=$((PASSED + 1)); else FAILED=$((FAILED + 1)); fi
	REPORT="$REPORT
$1  $2"
	[ -n "${3:-}" ] && REPORT="$REPORT
      $3"
	printf '%s  %s\n' "$1" "$2"
	[ "$1" = FAIL ] && [ -n "${3:-}" ] && printf '      %s\n' "$3"
	return 0
}

# step <name> <cmd...>: run, log output, PASS on exit 0.
step() {
	local name="$1"
	shift
	{
		echo "=== $name: $*"
	} >>"$LOG"
	if "$@" >>"$LOG" 2>&1; then
		record PASS "$name"
		return 0
	fi
	record FAIL "$name" "$(tail -n 12 "$LOG" | tr '\n' '|' | cut -c1-600)"
	return 1
}

af() { "$AF_BIN" "$@"; }
tm() { env -u TMUX tmux -S "$TMUX_SOCK" "$@"; }

# wait_for <seconds> <cmd...>: poll until the command succeeds.
wait_for() {
	local n="$1"
	shift
	local i=0
	while [ "$i" -lt $((n * 4)) ]; do
		"$@" >/dev/null 2>&1 && return 0
		sleep 0.25
		i=$((i + 1))
	done
	return 1
}

cleanup() {
	local rc=$?
	# Stop only what this script started: its daemon (pid file in the throwaway
	# home) and its private tmux server (named by socket, never the default).
	if [ -f "$AGENT_FACTORY_HOME/daemon.pid" ]; then
		dp="$(tr -dc '0-9' <"$AGENT_FACTORY_HOME/daemon.pid" | head -c 10)"
		[ -n "$dp" ] && kill "$dp" 2>/dev/null
	fi
	if [ -S "$TMUX_SOCK" ]; then
		tm kill-server >/dev/null 2>&1
	fi
	sleep 0.5
	if [ "$KEEP" = 1 ]; then
		echo "kept: $ROOT (log: $LOG)"
	else
		rm -rf "$ROOT"
	fi
	return $rc
}
trap cleanup EXIT

# --- environment banner (the part you paste) --------------------------------
echo "== environment"
{
	echo "date:     $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "uname:    $(uname -srm)"
	command -v sw_vers >/dev/null 2>&1 && echo "macOS:    $(sw_vers -productVersion) ($(sw_vers -buildVersion))"
	command -v csrutil >/dev/null 2>&1 && echo "SIP:      $(csrutil status 2>&1 | head -1)"
	echo "bash:     $BASH_VERSION (running $0)"
	echo "tmux:     $(tmux -V)  ($(command -v tmux))"
	echo "af:       $("$AF_BIN" version 2>&1 | head -1)  ($AF_BIN)"
	echo "TMPDIR:   ${TMPDIR:-unset} (${#TMPDIR} bytes)"
	echo "root:     $ROOT"
	echo "web port: $PORT"
} | tee -a "$LOG"
echo "== journey"

# 1. the binary itself ---------------------------------------------------------
if [ "$(uname -s)" = Darwin ]; then
	step "codesign -v on the af binary" codesign -v --verbose=2 "$AF_BIN"
	step "af binary is a Mach-O for this machine" sh -c "lipo -archs '$AF_BIN' | tr ' ' '\n' | grep -qx \"\$(uname -m)\""
fi
step "af version runs" af version

# 2. config: stand-in agent, loopback port, no self-update ---------------------
step "config: stand-in agent" af config set program_overrides.claude claude
step "config: default program" af config set default_program claude
step "config: web UI port" af config set network.listen_addr "127.0.0.1:$PORT"
step "config: auto_update off" af config set auto_update false

# 3. a repo to work in ---------------------------------------------------------
(
	cd "$ROOT/repo" &&
		git init -q -b main . &&
		echo hello >README.md &&
		git add README.md &&
		git commit -q -m init
) >>"$LOG" 2>&1
record_repo=$?
[ "$record_repo" = 0 ] && record PASS "test repo created" || record FAIL "test repo created"
REPO="$ROOT/repo"

# 4. daemon --------------------------------------------------------------------
# The first command that needs the daemon starts it (the documented behaviour).
step "daemon starts on first use" af sessions --repo "$REPO" list
step "daemon status answers" af daemon status
step "web UI answers 200 on loopback" wait_for 20 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/)\" = 200 ]"

# 5. a plain session -------------------------------------------------------------
step "create a session" af sessions --repo "$REPO" create --name plain --program claude
tmux_name_of() { af sessions --repo "$REPO" get "$1" --json 2>/dev/null | grep -o '"tmux_name": *"[^"]*"' | head -1 | sed 's/.*: *"\(.*\)"/\1/'; }
PLAIN_TMUX="$(tmux_name_of plain)"
step "session is live on the private tmux server" sh -c "env -u TMUX tmux -S '$TMUX_SOCK' has-session -t '=$PLAIN_TMUX'"

# 6. account-scoped session through the launch-capability shim (#6373) -------------
ACCT_DIR="$(af accounts add claude work 2>>"$LOG" | tail -1)"
ACCT_DIR_REAL="$(cd -P "$ACCT_DIR" 2>/dev/null && pwd)"
step "account registered" test -d "$ACCT_DIR"
step "create an account-scoped session" af sessions --repo "$REPO" create --name acct --program claude --account work
ACCT_TMUX="$(tmux_name_of acct)"
ACCT_TARGET="=$ACCT_TMUX:"

default_command_has_shim() {
	tm show-options -v -t "$ACCT_TARGET" default-command 2>/dev/null | grep -q '__af-session-env-exec-account-environment'
}
step "account session's default-command is af's scoped-shell shim" default_command_has_shim

# new_window_is_scoped <stage>: open a window the way prefix-c does and ask the
# shell it starts which account it is under.
new_window_is_scoped() {
	local stage="$1" pane answer got
	pane="$(tm new-window -d -P -F '#{pane_id}' -t "$ACCT_TARGET" 2>>"$LOG")" || return 1
	answer="$ROOT/answer-$RANDOM"
	sleep 1
	tm send-keys -t "$pane" "printf %s \"\$CLAUDE_CONFIG_DIR\" > '$answer'" Enter || return 1
	wait_for 15 test -s "$answer" || {
		echo "$stage: window never answered; screen:" >>"$LOG"
		tm capture-pane -p -t "$pane" >>"$LOG" 2>&1
		return 1
	}
	got="$(cat "$answer")"
	got_real="$(cd -P "$got" 2>/dev/null && pwd)"
	[ "$got_real" = "$ACCT_DIR_REAL" ] || {
		echo "$stage: window reports account dir '$got', want '$ACCT_DIR'" >>"$LOG"
		return 1
	}
}
step "new window in the account session runs under the account" new_window_is_scoped "fresh"

# 7. tabs ---------------------------------------------------------------------------
count_tabs() { af sessions --repo "$REPO" get acct --json 2>/dev/null | grep -c '"kind"'; }
TABS_BEFORE="$(count_tabs)"
step "open a terminal (shell) tab" af sessions --repo "$REPO" tab-create acct --kind shell --name smoke-shell
tab_added() { [ "$(count_tabs)" -gt "$TABS_BEFORE" ]; }
step "the terminal tab is listed" tab_added

# 8. daemon restart keeps the account session usable (upgrade shape) ------------------
step "daemon restart" af daemon restart
step "web UI answers 200 after restart" wait_for 30 sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/)\" = 200 ]"
step "new window still scoped after daemon restart" new_window_is_scoped "after restart"

# 9. archive and restore ----------------------------------------------------------------
step "archive the account session" af sessions --repo "$REPO" archive acct
gone() { ! tm has-session -t "=$ACCT_TMUX" 2>/dev/null; }
step "archived session's tmux session is gone" wait_for 15 gone
step "restore the account session" af sessions --repo "$REPO" restore acct
ACCT_TMUX="$(tmux_name_of acct)"
ACCT_TARGET="=$ACCT_TMUX:"
step "restored session is live again" wait_for 20 sh -c "env -u TMUX tmux -S '$TMUX_SOCK' has-session -t '=$ACCT_TMUX'"
step "new window scoped after restore" new_window_is_scoped "after restore"

# 10. doctor ---------------------------------------------------------------------------------
step "af doctor --setup" af doctor --setup
step "af doctor" af doctor

# --- report ---------------------------------------------------------------------------------
echo
echo "== report: $PASSED passed, $FAILED failed"
if [ "$FAILED" -gt 0 ]; then
	echo "Paste everything above on https://github.com/sachiniyer/agent-factory/issues/6384"
	echo "(re-run with --keep to leave the log at \$ROOT/smoke.log)"
	exit 1
fi
exit 0
