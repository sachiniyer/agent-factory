#!/usr/bin/env bash
# Runs INSIDE the web-selftest container (see scripts/testbox.sh web-selftest):
# build af from the mounted source, bring up a REAL af daemon on a throwaway home
# with a loopback TLS+token listener, seed a couple of sessions in a mock repo,
# then drive the embedded SPA in a headless Chromium via Playwright and assert the
# core flows (web/selftest/web-driver.spec.ts).
#
# Everything here — the tmux server, the daemon, the AF home, the sessions, the
# browser — lives and dies with the container. Teardown is `docker rm -f`, not a
# checklist. Nothing touches the host tmux server or the real ~/.agent-factory.
set -euo pipefail

# --- writable working copy (the /src bind mount is read-only) ---------------
# Copy without .git: dev checkouts are often linked worktrees whose .git is a
# pointer to a host path absent in the container (mirrors run-tests.sh).
mkdir -p /work
(cd /src && tar -c --exclude=.git --exclude=web/node_modules --exclude=web/test-results .) | tar -x -C /work
cd /work

HOME_DIR=/work/afhome
MOCK=/work/mock-repo
BIN=/work/bin/af
LISTEN=127.0.0.1:8899
BASE_URL="https://${LISTEN}"
READY_MARKER=AF_SELFTEST_READY
SESSION_A=probe-a
SESSION_B=probe-b
export AGENT_FACTORY_HOME="$HOME_DIR"
# A container binary is built at the branch version (typically behind the latest
# release); without this it would self-update on boot and restart the daemon
# mid-run, racing session creation (#1596). Real users are unaffected.
export AGENT_FACTORY_AUTO_UPDATE=false
mkdir -p "$HOME_DIR" /work/bin

echo ">>> building af from /work ..."
go build -buildvcs=false -o "$BIN" .

# --- throwaway AF home: TLS listener + a fake agent -------------------------
# The fake agent prints a deterministic ready marker (the "live output" the
# terminal flow asserts on) then execs `cat`, so typed input echoes back — the
# same shape the WS PTY broker round-trip uses. Because the override is a custom
# script (not literally "claude"), af appends no agent flags and counts the pane
# ready as soon as it shows output (#1116/#1131).
cat >"$HOME_DIR/fake-agent.sh" <<EOF
#!/bin/sh
printf '%s\n' "$READY_MARKER"
exec cat
EOF
chmod +x "$HOME_DIR/fake-agent.sh"

# listen_addr turns on the TLS TCP listener that serves the SPA + /v1 API and
# generates the bearer token (daemon/tcpserver.go). Global-only key, so it lives
# in the home config.json.
cat >"$HOME_DIR/config.json" <<EOF
{
  "default_program": "claude",
  "program_overrides": { "claude": "$HOME_DIR/fake-agent.sh" },
  "listen_addr": "$LISTEN"
}
EOF

# --- mock project repo (never a real repo) ----------------------------------
if [ ! -d "$MOCK" ]; then
    mkdir -p "$MOCK"
    (
        cd "$MOCK"
        git init -q -b master
        printf '# mock\nA throwaway repo for the web-driver-selftest.\n' >README.md
        git add -A
        git commit -qm "initial project"
    )
fi

# --- start the daemon + wait for the TLS listener ---------------------------
echo ">>> starting af daemon (listen_addr=$LISTEN) ..."
"$BIN" --daemon >/work/daemon.log 2>&1 &
DAEMON_PID=$!

cleanup() {
    rc=$?
    echo ">>> tearing down (rc=$rc) ..."
    "$BIN" sessions kill "$SESSION_A" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_B" >/dev/null 2>&1 || true
    kill "$DAEMON_PID" >/dev/null 2>&1 || true
    if [ "$rc" -ne 0 ]; then
        echo "===== daemon.log (tail) =====" >&2
        tail -n 40 /work/daemon.log >&2 || true
    fi
}
trap cleanup EXIT

# Poll the TLS port until it serves the SPA shell (200). Once it binds, the token
# file exists (EnsureToken runs before the port opens).
echo ">>> waiting for the TLS listener ..."
for i in $(seq 1 60); do
    if curl -sk -o /dev/null -w '%{http_code}' "$BASE_URL/" 2>/dev/null | grep -q '^200$'; then
        break
    fi
    if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
        echo "daemon exited before binding the listener; see log:" >&2
        cat /work/daemon.log >&2
        exit 1
    fi
    sleep 1
    if [ "$i" -eq 60 ]; then
        echo "timed out waiting for $BASE_URL" >&2
        cat /work/daemon.log >&2
        exit 1
    fi
done

TOKEN="$(cat "$HOME_DIR/daemon-token")"
if [ -z "$TOKEN" ]; then
    echo "no daemon token at $HOME_DIR/daemon-token" >&2
    exit 1
fi
echo ">>> daemon up at $BASE_URL (token ${#TOKEN} bytes)"

# --- seed two sessions in the mock repo -------------------------------------
echo ">>> creating sessions $SESSION_A, $SESSION_B ..."
"$BIN" sessions create --repo "$MOCK" --name "$SESSION_A" --program claude >/dev/null
"$BIN" sessions create --repo "$MOCK" --name "$SESSION_B" --program claude >/dev/null

# Give the workspaces a moment to launch (worktree + tmux pane) so the rail lists
# them and the fake agent's marker is on-screen for the attach flow.
for i in $(seq 1 30); do
    count="$("$BIN" sessions list 2>/dev/null | grep -c '"title"' || true)"
    if [ "${count:-0}" -ge 2 ]; then
        break
    fi
    sleep 1
done

# --- run the Playwright harness ---------------------------------------------
echo ">>> installing web deps + running the Playwright harness ..."
cd /work/web
# The image already carries the matching Chromium build; skip npm's browser
# download and use the bundled one.
export PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
npm ci --no-audit --no-fund

export AF_WEB_BASE_URL="$BASE_URL"
export AF_WEB_TOKEN="$TOKEN"
export AF_WEB_SESSION_A="$SESSION_A"
export AF_WEB_SESSION_B="$SESSION_B"
export AF_WEB_READY_MARKER="$READY_MARKER"

npx playwright test
