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
# A session left ARCHIVED with a preserved web tab (#1809 follow-up), so the harness
# can assert an archived session is inert: its web tab renders a placeholder instead
# of proxying, and its tab is not deletable. Same substring caveat as above.
SESSION_WEB_SHELVED=probe-shelved
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
# The malformed/older web-tab record (a web tab with NO target url) the harness
# seeds straight into the daemon's store, since no API can mint one (#1818).
NOURL_TAB=nourl
# A SECOND, VITE-SHAPED preview server (#1806/#1810/#1811): unlike the single
# self-contained document above, it serves a SUBDIRECTORY document with real
# sub-resources — a sibling, a PARENT-relative one, and an ABSOLUTE-path one. That
# combination is what the mirror-path URL model exists for and what the old fixture
# structurally could not fail on (it had no sub-resources at all).
VITE_PORT=8891
VITE_MARKER=AF_VITE_OK
# Set as document.title by the ABSOLUTE-path script IF it ever executes. It must
# not: an absolute path escapes the tab prefix and cannot be attributed back to a
# tab (#1811), so it has to 404 rather than be answered with the SPA shell.
VITE_ABS_TITLE=AF_ABS_ASSET_EXECUTED
# The web-tab session for the mirror-path / asset tests, kept SEPARATE from
# SESSION_WEB so it is immune to the tab-consuming order of the tests there.
# Substring caveat as above: no name may contain another session's name.
SESSION_VITE=probe-vite
# The #1810 misroute test gets its OWN session, visited by exactly one test, so its
# close-a-tab assertions are independent of what any earlier test left selected.
SESSION_MIS=probe-misroute
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
# Both are functions because the harness restarts the daemon once, to seed a record
# no API can mint (see the URL-less web tab below); the restart must reuse this exact
# readiness poll rather than a second, drifting copy of it.
start_daemon() {
    "$BIN" --daemon >>/work/daemon.log 2>&1 &
    DAEMON_PID=$!
}

# Poll the HTTP port until it serves the SPA shell (200). Once it binds, the token
# file exists (EnsureToken runs before the port opens).
wait_for_listener() {
    for i in $(seq 1 60); do
        if curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/" 2>/dev/null | grep -q '^200$'; then
            return 0
        fi
        if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
            echo "daemon exited before binding the listener; see log:" >&2
            cat /work/daemon.log >&2
            exit 1
        fi
        sleep 1
    done
    echo "timed out waiting for $BASE_URL" >&2
    cat /work/daemon.log >&2
    exit 1
}

echo ">>> starting af daemon (listen_addr=$LISTEN) ..."
start_daemon
WEBTAB_SERVER_PID=""
VITE_SERVER_PID=""

cleanup() {
    rc=$?
    echo ">>> tearing down (rc=$rc) ..."
    "$BIN" sessions kill "$SESSION_A" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_B" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_C" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_WEB" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_WEB_RESTORED" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_WEB_SHELVED" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_VITE" >/dev/null 2>&1 || true
    "$BIN" sessions kill "$SESSION_MIS" >/dev/null 2>&1 || true
    kill "$WEBTAB_SERVER_PID" >/dev/null 2>&1 || true
    kill "$VITE_SERVER_PID" >/dev/null 2>&1 || true
    kill "$DAEMON_PID" >/dev/null 2>&1 || true
    if [ "$rc" -ne 0 ]; then
        echo "===== daemon.log (tail) =====" >&2
        tail -n 40 /work/daemon.log >&2 || true
    fi
}
trap cleanup EXIT

echo ">>> waiting for the HTTP listener ..."
wait_for_listener

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

# A VITE-SHAPED app: a document under /app/ whose assets are referenced the three
# ways real dev servers reference them.
echo ">>> starting vite-shaped preview server on 127.0.0.1:$VITE_PORT ..."
cat >/work/vite-server.js <<EOF
const http = require("http");
const PAGE = [
  "<!doctype html><html><head>",
  // SIBLING: resolves to /app/x.css through the mirrored prefix.
  '<link rel="stylesheet" href="x.css">',
  // PARENT-RELATIVE: resolves to /shared.css — inside the prefix only because the
  // proxy URL mirrors the target's DEPTH.
  '<link rel="stylesheet" href="../shared.css">',
  // ABSOLUTE: resolves against the ORIGIN ROOT and escapes the prefix entirely.
  // Nothing can route it back (#1811) — it must 404, not get the SPA shell.
  '<script src="/assets/app.js"></script>',
  '</head><body><h1 id="marker">$VITE_MARKER</h1>',
  '<p id="sib">sibling</p><p id="par">parent</p></body></html>',
].join("");
http
  .createServer((req, res) => {
    const path = req.url.split("?")[0];
    if (path === "/app/viewer.html") {
      res.setHeader("content-type", "text/html");
      res.end(PAGE);
      return;
    }
    if (path === "/app/x.css") {
      res.setHeader("content-type", "text/css");
      res.end("#sib{color:rgb(1,2,3)}");
      return;
    }
    if (path === "/shared.css") {
      res.setHeader("content-type", "text/css");
      res.end("#par{color:rgb(4,5,6)}");
      return;
    }
    if (path === "/assets/app.js") {
      res.setHeader("content-type", "application/javascript");
      res.end('document.title=" $VITE_ABS_TITLE ";');
      return;
    }
    res.statusCode = 404;
    res.end("404 File not found: " + path);
  })
  .listen($VITE_PORT, "127.0.0.1");
EOF
node /work/vite-server.js &
VITE_SERVER_PID=$!

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

# --- #1809 follow-up: an ARCHIVED session's preserved web tab is INERT --------
# A session left ARCHIVED with a preserved web tab, so the harness can assert the
# two gates the preservation made reachable: the daemon must refuse to proxy the
# stored loopback URL (an archived session is inert — the port may host something
# else now), and the tab must not be deletable before the restore that was supposed
# to bring it back.
echo ">>> creating archived-web-tab session $SESSION_WEB_SHELVED (#1809 follow-up) ..."
"$BIN" sessions create --repo "$MOCK" --name "$SESSION_WEB_SHELVED" --program claude >/dev/null
for i in $(seq 1 30); do
    if "$BIN" sessions get "$SESSION_WEB_SHELVED" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_WEB_SHELVED" --kind web --port "$WEBTAB_PORT" --name shelvedweb >/dev/null
"$BIN" sessions archive "$SESSION_WEB_SHELVED" --repo "$MOCK" >/dev/null
# The CLI half of the tab-delete gate, end to end through the real daemon: deleting
# the preserved tab while archived must be REFUSED with an actionable message, and
# the record must still carry the URL afterwards.
if "$BIN" sessions tab-delete --repo "$MOCK" "$SESSION_WEB_SHELVED" --name shelvedweb >/dev/null 2>&1; then
    echo "FATAL: tab-delete succeeded on an archived session; the preserved URL is strippable (#1809)" >&2
    exit 1
fi
if ! "$BIN" sessions get "$SESSION_WEB_SHELVED" | grep -q shelvedweb; then
    echo "FATAL: the archived session's preserved web tab is gone after a refused tab-delete (#1809)" >&2
    "$BIN" sessions get "$SESSION_WEB_SHELVED" >&2
    exit 1
fi

# --- the mirror-path (#1806/#1811) session -----------------------------------
# One web tab targeting a SUBDIRECTORY document with sibling / parent-relative /
# absolute assets.
echo ">>> creating web-tab session $SESSION_VITE ..."
"$BIN" sessions create --repo "$MOCK" --name "$SESSION_VITE" --program claude >/dev/null
for i in $(seq 1 30); do
    if "$BIN" sessions get "$SESSION_VITE" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_VITE" --kind web \
    --url "http://127.0.0.1:$VITE_PORT/app/viewer.html" --name vite >/dev/null

# --- the #1810 misroute session ---------------------------------------------
# The tab layout is load-bearing, reproducing #1810's SILENT wrong-server shape:
#
#   0 agent
#   1 lower  -> :WEBTAB_PORT  (AF_WEBTAB_LOCAL_OK)
#   2 mis    -> :VITE_PORT/app/viewer.html  (AF_VITE_OK)   <-- a pane opens on this
#   3 after  -> :WEBTAB_PORT  (AF_WEBTAB_LOCAL_OK)
#
# Closing "lower" shifts mis 2->1 and after 3->2. An ORDINAL-keyed iframe minted at
# index 2 would then serve "after" — a DIFFERENT app, HTTP 200, no error, no reload.
# Keyed by the stable tab id, the pane keeps showing mis's own dev server.
echo ">>> creating web-tab session $SESSION_MIS ..."
"$BIN" sessions create --repo "$MOCK" --name "$SESSION_MIS" --program claude >/dev/null
for i in $(seq 1 30); do
    if "$BIN" sessions get "$SESSION_MIS" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_MIS" --kind web --port "$WEBTAB_PORT" --name lower >/dev/null
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_MIS" --kind web \
    --url "http://127.0.0.1:$VITE_PORT/app/viewer.html" --name mis >/dev/null
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_MIS" --kind web --port "$WEBTAB_PORT" --name after >/dev/null

# --- seed the URL-less web tab through the daemon's OWN store (#1818) -------
# ORDER MATTERS: this block STOPS the daemon to write its store, so it must come
# after every seeding step that drives the live daemon over the CLI (the #1809
# archive/restore sessions above). The archived + restored records persist, and the
# restarted daemon reloads them — which the assertions below rely on.
#
# A web tab with no target is a MALFORMED/older record, and three live guards
# refuse to mint one (CreateTab's "requires a target", NormalizeWebTabURL, and
# AddWebTab's "requires a non-empty URL"), so there is no API/CLI call that can
# stage it — and weakening a real user-facing validation to let a test in would be
# the wrong trade. The honest way to stage a legacy record is to write it the way an
# older version would have and let the daemon load it: `url` is omitempty, so a tab
# record with no url field IS that shape, and restoreLocalTabs backfills its stable
# id on load.
#
# Seeding it in the daemon's own store — rather than rewriting a Snapshot inside the
# browser, which is what this used to do — is the point of #1818. The daemon then
# serves the tab on BOTH planes, so the Snapshot and every session.updated agree
# about it. The old browser-side mock only patched the Snapshot, so any
# session.updated (which tab changes now emit, #1812) overwrote the projection with
# the real roster and silently deleted the injected tab mid-test: a flake. There is
# nothing left to race here, and no sleep or retry propping it up.
#
# The daemon is the single writer of instances.json (#960), so it must be stopped
# before the file is touched and restarted to pick the record up. `wait` blocks until
# it is really gone — no sleep. Nothing else is in flight at this point (all seeding
# is done, Playwright has not started), so this cannot race session creation the way
# an auto-update restart once did (#1596).
echo ">>> seeding the URL-less web tab ($NOURL_TAB) into the daemon's store ..."
kill "$DAEMON_PID" 2>/dev/null || true
wait "$DAEMON_PID" 2>/dev/null || true

cat >/work/seed-nourl.js <<'EOF'
const fs = require("fs");
const path = require("path");
const [home, title, tab] = process.argv.slice(2);
// Per-repo store: $AGENT_FACTORY_HOME/instances/<repoID>/instances.json. Find the
// file holding the session by title rather than deriving the repo id.
const dir = path.join(home, "instances");
let patched = false;
for (const repoID of fs.readdirSync(dir)) {
  const file = path.join(dir, repoID, "instances.json");
  if (!fs.existsSync(file)) continue;
  const raw = JSON.parse(fs.readFileSync(file, "utf8"));
  // v1 is an envelope ({schema_version, instances}); v0 was a bare array. Keep
  // whichever shape is on disk so the daemon's own migrator still sees what it wrote.
  const instances = Array.isArray(raw) ? raw : raw.instances;
  for (const inst of instances ?? []) {
    if (inst.title !== title) continue;
    // kind 3 = TabKindWeb. No `url` key at all — the legacy shape, not url:"".
    (inst.tabs = inst.tabs ?? []).push({ name: tab, kind: 3 });
    patched = true;
  }
  if (patched) {
    fs.writeFileSync(file, JSON.stringify(raw));
    break;
  }
}
if (!patched) {
  console.error(`seed-nourl: no session titled ${title} under ${dir}`);
  process.exit(1);
}
EOF
node /work/seed-nourl.js "$HOME_DIR" "$SESSION_WEB" "$NOURL_TAB"

echo ">>> restarting the daemon so it loads the seeded record ..."
start_daemon
wait_for_listener

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
export AF_WEB_SESSION_WEB_SHELVED="$SESSION_WEB_SHELVED"
export AF_WEB_READY_MARKER="$READY_MARKER"
export AF_WEB_TASK_NAME="$SEEDED_TASK"
export AF_WEB_TASK3_NAME="$TASK3_NAME"
export AF_WEBTAB_LOCAL_MARKER="$WEBTAB_LOCAL_MARKER"
export AF_WEBTAB_EXTERNAL_URL="$WEBTAB_EXTERNAL_URL"
export AF_WEB_SESSION_VITE="$SESSION_VITE"
export AF_WEB_SESSION_MIS="$SESSION_MIS"
export AF_VITE_MARKER="$VITE_MARKER"
export AF_VITE_ABS_TITLE="$VITE_ABS_TITLE"
export AF_WEBTAB_NOURL_NAME="$NOURL_TAB"
# The af CLI + the mock repo it targets, so a test can mutate state OUT-OF-BAND —
# as an agent or a script does, from outside the browser — and assert the open SPA
# reacts to the daemon's event (#1812). AGENT_FACTORY_HOME is already exported
# above, so the CLI resolves the same throwaway home as the daemon under test.
export AF_BIN="$BIN"
export AF_MOCK_REPO="$MOCK"

npx playwright test
