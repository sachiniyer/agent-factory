#!/usr/bin/env bash
# Runs INSIDE the web-selftest container (see scripts/testbox.sh web-selftest):
# build af from the mounted source, bring up a REAL af daemon on a throwaway home
# with a loopback HTTP listener, seed a couple of sessions in a mock repo, then drive
# the embedded SPA in a headless Chromium via Playwright and assert the core flows
# (web/selftest/web-driver.spec.ts).
#
# The daemon binds 127.0.0.1, so under #1696 the browser is a LOOPBACK peer the
# daemon exempts from the bearer token: the SPA auto-connects with no credential and
# the harness asserts that tokenless flow (and that every action works on it). No
# token is pasted or exported.
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
# A SECOND mock repo (redesign PR2): the single-project IA scopes the rail to one
# project, so the harness needs a second project to prove sessions from another
# project are hidden and that switching projects swaps the rail. Its one session
# (SESSION_C) is created BEFORE the first repo's sessions so the most-recently-active
# default still lands on the first repo (probe-a/b/web), keeping the A/B-driven flows
# on the default project.
MOCK2=/work/mock-repo-2
# A THIRD mock repo (redesign PR2, Greptile Fix 1): a TASK-ONLY project — a scheduled
# task but NO session — to prove a repo with tasks and no sessions still lists in the
# switcher and its tasks stay reachable (the Tasks view scopes to it, the rail is the
# empty state). It has no session, so it is never the most-recently-active default.
MOCK3=/work/mock-repo-3
BIN=/work/bin/af
LISTEN=127.0.0.1:8899
BASE_URL="http://${LISTEN}"
READY_MARKER=AF_SELFTEST_READY
SESSION_A=probe-a
SESSION_B=probe-b
SESSION_C=probe-c
SESSION_WEB=probe-web
# A session taken through the #1809 repro — web tab → archive → restore — so the
# harness proves a RESTORED web tab still renders live through the daemon proxy.
# Deliberately NOT "probe-web-…": the spec's row() filters by substring, so a name
# containing SESSION_WEB would match probe-web's row too and wedge the web-tab
# tests on a strict-mode violation.
SESSION_WEB_RESTORED=probe-restored
SEEDED_TASK=probe-task
# The task-only project's task name (MOCK3), kept distinct from SEEDED_TASK so the
# scoped Tasks assertions never collide on a substring match.
TASK3_NAME=mock3-task
# A throwaway loopback HTTP server the web-tab test points a local web tab at, so
# the daemon reverse-proxy + iframe render is exercised end to end against real
# content. The external web tab points at a host the Playwright test intercepts.
WEBTAB_PORT=8890
WEBTAB_LOCAL_MARKER=AF_WEBTAB_LOCAL_OK
WEBTAB_EXTERNAL_URL=https://blocked.example.test/
export AGENT_FACTORY_HOME="$HOME_DIR"
# A container binary is built at the branch version (typically behind the latest
# release); without this it would self-update on boot and restart the daemon
# mid-run, racing session creation (#1596). Real users are unaffected.
export AGENT_FACTORY_AUTO_UPDATE=false
mkdir -p "$HOME_DIR" /work/bin

echo ">>> building af from /work ..."
go build -buildvcs=false -o "$BIN" .

# --- throwaway AF home: HTTP listener + a fake agent ------------------------
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

# listen_addr turns on the plain-HTTP TCP listener that serves the SPA + /v1 API and
# generates the bearer token (daemon/tcpserver.go). Global-only key, so it lives
# in the home config.json.
cat >"$HOME_DIR/config.json" <<EOF
{
  "default_program": "claude",
  "program_overrides": { "claude": "$HOME_DIR/fake-agent.sh" },
  "listen_addr": "$LISTEN"
}
EOF

# --- mock project repos (never real repos) ----------------------------------
for repo in "$MOCK" "$MOCK2" "$MOCK3"; do
    if [ ! -d "$repo" ]; then
        mkdir -p "$repo"
        (
            cd "$repo"
            git init -q -b master
            printf '# mock\nA throwaway repo for the web-driver-selftest.\n' >README.md
            git add -A
            git commit -qm "initial project"
        )
    fi
done

# --- start the daemon + wait for the HTTP listener --------------------------
echo ">>> starting af daemon (listen_addr=$LISTEN) ..."
"$BIN" --daemon >/work/daemon.log 2>&1 &
DAEMON_PID=$!
WEBTAB_SERVER_PID=""

cleanup() {
    rc=$?
    echo ">>> tearing down (rc=$rc) ..."
    "$BIN" sessions kill "$SESSION_A" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_B" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_C" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_WEB" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_WEB_RESTORED" >/dev/null 2>&1 || true
    kill "$WEBTAB_SERVER_PID" >/dev/null 2>&1 || true
    kill "$DAEMON_PID" >/dev/null 2>&1 || true
    if [ "$rc" -ne 0 ]; then
        echo "===== daemon.log (tail) =====" >&2
        tail -n 40 /work/daemon.log >&2 || true
    fi
}
trap cleanup EXIT

# Poll the HTTP port until it serves the SPA shell (200). Once it binds, the token
# file exists (EnsureToken runs before the port opens).
echo ">>> waiting for the HTTP listener ..."
for i in $(seq 1 60); do
    if curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/" 2>/dev/null | grep -q '^200$'; then
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

# The token file still exists (EnsureToken runs before the port opens) even though
# the loopback browser never uses it — read it purely as a daemon-health signal.
TOKEN="$(cat "$HOME_DIR/daemon-token")"
if [ -z "$TOKEN" ]; then
    echo "no daemon token at $HOME_DIR/daemon-token" >&2
    exit 1
fi
echo ">>> daemon up at $BASE_URL (loopback ⇒ tokenless browser; token ${#TOKEN} bytes exists for network peers)"

# --- seed a session in the SECOND repo (redesign PR2) -----------------------
# Created first so the most-recently-active default lands on the FIRST repo below.
# This is the "other project" the scoping tests assert is hidden by default.
echo ">>> creating session $SESSION_C in the second repo ..."
"$BIN" sessions create --repo "$MOCK2" --name "$SESSION_C" --program claude >/dev/null

# --- seed two sessions in the (default) mock repo ---------------------------
echo ">>> creating sessions $SESSION_A, $SESSION_B ..."
"$BIN" sessions create --repo "$MOCK" --name "$SESSION_A" --program claude >/dev/null
"$BIN" sessions create --repo "$MOCK" --name "$SESSION_B" --program claude >/dev/null

# Give the workspaces a moment to launch (worktree + tmux pane) so the rail lists
# them and the fake agent's marker is on-screen for the attach flow.
for i in $(seq 1 30); do
    count="$("$BIN" sessions list 2>/dev/null | grep -c '"title"' || true)"
    if [ "${count:-0}" -ge 3 ]; then
        break
    fi
    sleep 1
done

# --- seed a scheduled task (#1592 Phase 5 PR8) ------------------------------
# So the tasks view is non-empty on load. A cron task needs a prompt (there is no
# event line to fall back to); the schedule never actually fires in the test window,
# it just has to exist for the list + the enable/disable/trigger/remove flows.
echo ">>> seeding task $SEEDED_TASK ..."
"$BIN" tasks add --repo "$MOCK" --name "$SEEDED_TASK" --prompt "echo scheduled" --cron "0 9 * * *" >/dev/null

# A task in the THIRD repo, which has NO session (redesign PR2, Greptile Fix 1): this
# makes MOCK3 a TASK-ONLY project so the harness can prove it lists in the switcher
# and its tasks scope correctly.
echo ">>> seeding task-only project $MOCK3 (task $TASK3_NAME, no session) ..."
"$BIN" tasks add --repo "$MOCK3" --name "$TASK3_NAME" --prompt "echo mock3" --cron "0 9 * * *" >/dev/null

# --- seed a web-tab session (feat: web/iframe tabs) -------------------------
# A tiny loopback HTTP server serves a deterministic marker; a LOCAL web tab
# points at it (exercising the daemon reverse-proxy + iframe render), and an
# EXTERNAL web tab points at a host the Playwright test intercepts (exercising the
# direct-iframe + blocked-embedding fallback).
echo ">>> starting web-tab preview server on 127.0.0.1:$WEBTAB_PORT ..."
cat >/work/webtab-server.js <<EOF
const http = require("http");
http
  .createServer((req, res) => {
    res.setHeader("content-type", "text/html");
    res.end('<!doctype html><html><body><h1 id="marker">$WEBTAB_LOCAL_MARKER</h1><p>path=' + req.url + "</p></body></html>");
  })
  .listen($WEBTAB_PORT, "127.0.0.1");
EOF
node /work/webtab-server.js &
WEBTAB_SERVER_PID=$!

echo ">>> creating web-tab session $SESSION_WEB ..."
"$BIN" sessions create --repo "$MOCK" --name "$SESSION_WEB" --program claude >/dev/null
for i in $(seq 1 30); do
    if "$BIN" sessions get "$SESSION_WEB" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
# A local web tab (daemon-proxied) named "preview" and an external one named
# "external", so the Playwright test can address each by its tab label.
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_WEB" --kind web --port "$WEBTAB_PORT" --name preview >/dev/null
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_WEB" --kind web --url "$WEBTAB_EXTERNAL_URL" --name external >/dev/null

# --- #1809: a web tab must survive archive -> restore ------------------------
# Drive the issue's exact CLI repro against the real daemon: a session with a web
# tab AND a process tab is archived (the documented restorable reap path) and then
# restored. Archive used to truncate the roster to the agent tab, silently erasing
# the web tab's URL with no way to recover it. The Playwright test then proves the
# restored web tab still renders LIVE through the daemon proxy — not merely that a
# row survived in JSON.
echo ">>> creating archive/restore web-tab session $SESSION_WEB_RESTORED (#1809) ..."
"$BIN" sessions create --repo "$MOCK" --name "$SESSION_WEB_RESTORED" --program claude >/dev/null
for i in $(seq 1 30); do
    if "$BIN" sessions get "$SESSION_WEB_RESTORED" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_WEB_RESTORED" --kind web --port "$WEBTAB_PORT" --name webpreview >/dev/null
# A process tab alongside it: it must NOT come back (its tmux is torn down at
# archive time, #1028) — the fix is kind-aware, not a blanket "keep everything".
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_WEB_RESTORED" --command "sleep 300" --name watcher >/dev/null
"$BIN" sessions archive "$SESSION_WEB_RESTORED" --repo "$MOCK" >/dev/null
"$BIN" sessions restore "$SESSION_WEB_RESTORED" --repo "$MOCK" >/dev/null
# Fail loudly HERE if the roster did not survive, so a regression reads as "the
# archive dropped the web tab" rather than a downstream Playwright selector miss.
if ! "$BIN" sessions get "$SESSION_WEB_RESTORED" | grep -q webpreview; then
    echo "FATAL: the web tab did not survive archive -> restore (#1809)" >&2
    "$BIN" sessions get "$SESSION_WEB_RESTORED" >&2
    exit 1
fi

# --- run the Playwright harness ---------------------------------------------
echo ">>> installing web deps + running the Playwright harness ..."
cd /work/web
# The image already carries the matching Chromium build; skip npm's browser
# download and use the bundled one.
export PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
npm ci --no-audit --no-fund

export AF_WEB_BASE_URL="$BASE_URL"
export AF_WEB_SESSION_A="$SESSION_A"
export AF_WEB_SESSION_B="$SESSION_B"
export AF_WEB_SESSION_C="$SESSION_C"
export AF_WEB_SESSION_WEB="$SESSION_WEB"
export AF_WEB_SESSION_WEB_RESTORED="$SESSION_WEB_RESTORED"
export AF_WEB_READY_MARKER="$READY_MARKER"
export AF_WEB_TASK_NAME="$SEEDED_TASK"
export AF_WEB_TASK3_NAME="$TASK3_NAME"
export AF_WEBTAB_LOCAL_MARKER="$WEBTAB_LOCAL_MARKER"
export AF_WEBTAB_EXTERNAL_URL="$WEBTAB_EXTERNAL_URL"

npx playwright test
