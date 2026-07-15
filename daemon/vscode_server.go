package daemon

import (
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The VS Code tab (TabKindVSCode) is served by a code-server / openvscode-server
// process the DAEMON owns, one per session, rooted at that session's worktree and
// bound to loopback on an ephemeral port. The BROWSER reaches it only through the
// daemon's /v1/webtab/ reverse proxy, which is what makes the editor visible to a
// REMOTE viewer (Tailscale/SSH) and what applies the daemon's auth policy.
//
// The proxy is NOT the editor's only route, and nothing here should be read as
// claiming it is. The editor runs with auth disabled, so its own ephemeral
// loopback port is a SECOND, UNGUARDED route: any process on the box can connect
// to it directly and get worktree read/write and terminal execution as the af
// user. Under af's defaults that is consistent with the daemon rather than a hole
// in it — require_token and require_loopback_token both default to false, so the
// daemon itself already serves an unauthenticated control plane, which is strictly
// more power, to every local process. It becomes a real gap only for an operator
// who deliberately set BOTH on a shared host to demand a token from local peers:
// the editor's listener does not honor that, so spawnLocked warns when it sees
// that configuration. The structural fix — serving both flavors on a 0600 unix
// socket in the daemon's runtime dir, where file permissions do the authenticating
// — is tracked as a follow-up; it also removes the port TOCTOU freeLoopbackPort
// documents.
//
// af NEVER bundles or installs the editor: the binary is DETECTED (an explicit
// vscode_server_binary, else code-server, else openvscode-server on PATH). When
// none is present the pane renders an install hint rather than an error — a
// missing editor is a normal, actionable state, not a failure.

// vscodeStartGrace bounds how long ensureServer BLOCKS waiting for a freshly
// spawned editor to listen. It is deliberately short: it exists only to let a
// warm start be served inline, not to wait out a cold one. code-server is a Node
// app that routinely needs several seconds, and blocking a proxy request (and the
// browser) that long is worse than answering immediately — so exceeding the grace
// is NOT a failure. The process keeps starting, ensureServer reports
// errVSCodeStarting, and the notice page re-requests until the editor answers.
const vscodeStartGrace = 3 * time.Second

// vscodeRespawnCooldown is the minimum gap between spawn attempts for one session
// after a failure. The notice page auto-refreshes, so without a cooldown an
// editor that dies on startup (a broken install, an unreadable worktree) would be
// respawned on every refresh forever — a spawn loop driven by our own UI. Within
// the cooldown the recorded error is replayed instead of spawning again.
const vscodeRespawnCooldown = 5 * time.Second

// vscodeProbeInterval is how often ensureServer re-probes the port while waiting
// for a starting editor to accept connections.
const vscodeProbeInterval = 100 * time.Millisecond

// vscodeStopGrace is how long a stopping editor gets to exit on SIGTERM before
// the process GROUP is SIGKILLed, mirroring the watcher supervisor's escalation.
const vscodeStopGrace = 5 * time.Second

// vscodeSpawnAttempts bounds the port-race retry. The port is chosen by binding
// :0, reading the kernel's pick, and closing the listener before handing the
// number to the child — an unavoidable TOCTOU window in which another process can
// take it. Losing that race is rare and self-correcting: retry with a fresh port.
const vscodeSpawnAttempts = 3

// errVSCodeBinaryMissing reports that no editor binary could be found. It is a
// SENTINEL, not a failure: callers render the install hint for it and must not
// log it as an error — a machine without code-server installed is expected.
var errVSCodeBinaryMissing = errors.New("no VS Code server binary found")

// errVSCodeStarting reports that the editor is live but not yet accepting
// connections. Also a sentinel and also not a failure: callers render the
// self-refreshing "starting" notice, which resolves into the editor on its own.
var errVSCodeStarting = errors.New("the VS Code server is still starting")

// errVSCodeStartExited reports an editor that exited without ever serving a
// request — a broken start rather than a crash to heal from. It is what the
// respawn cooldown replays, so a broken install cannot be respawned by every
// auto-refresh of the "starting" notice.
var errVSCodeStartExited = errors.New("the VS Code server exited before it finished starting")

// vscodeFlavor distinguishes the two supported editors, whose CLIs disagree on
// every flag that matters (bind address, auth, base path).
type vscodeFlavor int

const (
	// flavorCodeServer is coder/code-server: `--bind-addr host:port`, `--auth none`.
	flavorCodeServer vscodeFlavor = iota
	// flavorOpenVSCode is gitpod-io/openvscode-server: `--host`/`--port`,
	// `--without-connection-token`.
	flavorOpenVSCode
)

// vscodeBinaryNames are the binaries probed on PATH, in preference order, when
// no explicit vscode_server_binary is configured.
var vscodeBinaryNames = []string{"code-server", "openvscode-server"}

// vscodeInstallHint is the user-facing text shown wherever the editor is
// unavailable (the web pane, the TUI placeholder, the proxy fallback page). It
// names the fix rather than the failure.
const vscodeInstallHint = "code-server is not installed — install it: https://github.com/coder/code-server#install (or set vscode_server_binary in config.toml)"

// flavorForBinary infers the CLI dialect from the binary's file name. Name-based
// detection is deliberate: both editors' `--help` is slow (Node startup) and
// neither offers a cheap version probe worth paying for on every spawn. An
// unrecognized name (a wrapper script, a symlink under another name) falls back
// to the code-server dialect, which is both the documented default and the one
// vscode_server_binary users are overwhelmingly pointing at.
func flavorForBinary(path string) vscodeFlavor {
	if strings.Contains(strings.ToLower(filepath.Base(path)), "openvscode") {
		return flavorOpenVSCode
	}
	return flavorCodeServer
}

// resolveVSCodeBinary finds the editor binary: an explicit configured path wins
// (and, when set, its absence is a hard error naming the key — a user who
// configured a path deserves to be told it is wrong rather than silently falling
// back to a different editor), otherwise PATH is probed in preference order.
// Returns errVSCodeBinaryMissing when nothing is installed.
func resolveVSCodeBinary(configured string) (string, error) {
	if c := strings.TrimSpace(configured); c != "" {
		expanded := config.ExpandTilde(c)
		path, err := exec.LookPath(expanded)
		if err != nil {
			return "", fmt.Errorf("vscode_server_binary %q is not executable: %w", c, err)
		}
		return path, nil
	}
	for _, name := range vscodeBinaryNames {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errVSCodeBinaryMissing
}

// vscodeArgs builds the child's argv for flavor, serving worktree at addr.
//
// NOTE on base paths: code-server ALWAYS listens at the root and emits RELATIVE
// URLs derived from the request path's depth, so it needs no base-path flag and
// the proxy must simply strip its prefix (which webTabProxyHandler does).
// `--abs-proxy-base-path` is NOT that flag — it only prefixes code-server's own
// /absproxy/<port> feature and has no effect on serving code-server itself under
// a sub-path (coder/code-server#6770). Passing it here would be a no-op at best.
// openvscode-server DOES have a real base-path flag (--server-base-path), and it
// is still the wrong tool: it bakes ONE prefix into the process, while a single
// per-SESSION editor is reached under a different prefix per tab. The proxy sends
// X-Forwarded-Prefix instead, which openvscode reads per request (and takes in
// PRECEDENCE over --server-base-path), so it composes with a shared editor.
//
// NOTE on the worktree: the flavors take it DIFFERENTLY, and the difference is
// invisible if you get it wrong. code-server reads a POSITIONAL path.
// openvscode-server does not: its webClientServer resolves the workbench folder
// only from --default-folder / --default-workspace, and although its parser
// accepts '_', it never reads it — so a positional worktree was ACCEPTED SILENTLY
// AND IGNORED, and the fallback editor came up empty (or on the last workspace)
// rather than on the session. cmd.Dir does not rescue it either; the web client
// server never derives the folder from cwd. ('folder' is a third spelling, but it
// is deprecated upstream in favor of exactly this one.)
func vscodeArgs(flavor vscodeFlavor, host string, port int, worktree string) []string {
	switch flavor {
	case flavorOpenVSCode:
		return []string{
			"--host", host,
			"--port", strconv.Itoa(port),
			// openvscode-server's --auth none equivalent: no token on any request.
			// Same posture, and the same caveat, as code-server's below.
			"--without-connection-token",
			"--default-folder", worktree,
		}
	default:
		return []string{
			"--bind-addr", net.JoinHostPort(host, strconv.Itoa(port)),
			// No auth on the editor itself. It listens on loopback, but the daemon
			// proxy is NOT its only route — see this file's header comment for what
			// that does and does not expose, and for the unix-socket follow-up.
			"--auth", "none",
			// code-server AUTO-GENERATES ~/.config/code-server/config.yaml (with a
			// random password) when no config exists. Point it at /dev/null so a
			// daemon-spawned editor neither depends on nor mutates the user's
			// config file, and every setting comes from this argv.
			"--config", os.DevNull,
			"--disable-telemetry",
			"--disable-update-check",
			"--disable-workspace-trust",
			worktree,
		}
	}
}

// writeVSCodeNoticePage renders a human-readable notice INTO the pane: the web
// UI iframes this route, so whatever this returns is what the user sees where the
// editor would be.
//
// It answers 503 rather than 200 because the editor genuinely is not being
// served: a 200 would tell a scripted client the editor is up and hand it a
// message. Browsers render an error status's body in an iframe, so the pane still
// shows this text — the status is honest without costing the UX. When retry is
// set the page re-requests itself, so a pane opened while the editor is still
// starting resolves into the editor on its own.
func writeVSCodeNoticePage(w http.ResponseWriter, message string) {
	writeVSCodeNoticePageRetry(w, message, false)
}

func writeVSCodeNoticePageRetry(w http.ResponseWriter, message string, retry bool) {
	refresh := ""
	if retry {
		refresh = `<meta http-equiv="refresh" content="2">`
	}
	body := fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><title>VS Code</title>%s
<style>
 html,body{margin:0;height:100%%}
 body{display:flex;align-items:center;justify-content:center;
      font:14px/1.6 ui-sans-serif,system-ui,sans-serif;
      background:#1f1f1f;color:#cccccc;padding:2rem;text-align:center}
 .m{max-width:46rem}
 a{color:#4daafc}
</style></head>
<body><div class="m">%s</div></body></html>`, refresh, htmlLinkify(message))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never cache a notice: the very next request may be the running editor.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, body)
}

// htmlLinkify escapes message for HTML and turns a bare https:// URL in it into
// a clickable link, so the install hint's URL is actionable from the pane. The
// escape happens FIRST and the anchor is built from the escaped text, so no part
// of message can inject markup.
func htmlLinkify(message string) string {
	escaped := html.EscapeString(message)
	start := strings.Index(escaped, "https://")
	if start < 0 {
		return escaped
	}
	end := start
	for end < len(escaped) && !strings.ContainsRune(" \t\n<)", rune(escaped[end])) {
		end++
	}
	url := escaped[start:end]
	return escaped[:start] + `<a href="` + url + `" target="_blank" rel="noopener noreferrer">` + url + `</a>` + escaped[end:]
}

// vscodeServer is one supervised editor process serving one session's worktree.
type vscodeServer struct {
	worktree string
	baseURL  string
	host     string
	port     int
	cmd      *exec.Cmd

	// ready latches once the editor has accepted a connection. Until then the
	// process is up but must not be proxied to, so callers see errVSCodeStarting
	// instead of a 502 from a port that is not listening yet.
	ready bool

	// exited is closed by the reaping goroutine once the child has been reaped AND
	// its process group SIGKILLed, so alive can distinguish "running" from "died"
	// without a syscall — and so a closed exited means the whole group is gone,
	// not merely the leader.
	exited chan struct{}

	// killGroup overrides the process-group signal syscall. Teardown's correctness
	// is about WHICH signals are sent and WHEN, which a test can only observe by
	// intercepting them; nil means the real syscall.
	killGroup func(pgid int, sig syscall.Signal) error
}

// signalGroup sends sig to the child's process GROUP, through the killGroup seam
// when one is set.
func (s *vscodeServer) signalGroup(pgid int, sig syscall.Signal) {
	if s.killGroup != nil {
		_ = s.killGroup(pgid, sig)
		return
	}
	_ = syscall.Kill(-pgid, sig)
}

// reap Waits the child, SIGKILLs its process GROUP, and closes exited — in that
// order, all three of which are load-bearing. Runs as one goroutine per spawn.
//
// Always Wait(): without it an exited editor stays a zombie parented to the daemon
// for the daemon's whole life (#816).
//
// The group SIGKILL lives HERE, not in stop(), because this is the ONLY moment at
// which -pgid is provably still ours. POSIX XBD 3.297: while a process group with
// a given id exists, that id is not reused. So any child that outlived the leader
// — an extension host, a pty host, a terminal that ignored SIGTERM — is itself a
// group member, and a group member PINS -pgid, so this kill cannot land on
// anything but our own. Escalating from stop() is what was unsafe: stop() runs at
// an ARBITRARILY LATER lifecycle event (nothing prunes a dead entry from
// v.servers, so a tab closed hours after a crash still reaches it), by which time
// the group is long empty, its id reusable, and kill(-pgid) hits a live, unrelated
// group. That is not theoretical on macOS — a supported platform, PID_MAX 99999,
// a busy laptop wraps in hours. It mirrors watcher.go's and hooks.go's group kills
// (#610/#769), which are safe for exactly this reason: each fires microseconds
// after its OWN Wait(), never across a later event.
//
// Killing here is also strictly stronger than killing in stop(): an editor that
// crashes on its own has its survivors killed AT CRASH TIME, rather than leaking
// until some later teardown that may never come.
//
// close(exited) goes AFTER the kill. stop() returns the moment exited closes and
// its callers act immediately — ArchiveSession MOVES the worktree. Closing first
// would let it move a directory the surviving children still hold open.
func (s *vscodeServer) reap() {
	// Read the pgid before the Wait that frees it: after this returns, the leader's
	// pid is the kernel's to give away, and only a surviving group member keeps it
	// pinned long enough for the kill below to mean what it says.
	pgid := s.cmd.Process.Pid
	_ = s.cmd.Wait()
	s.signalGroup(pgid, syscall.SIGKILL)
	close(s.exited)
}

// alive reports whether the child is still running.
func (s *vscodeServer) alive() bool {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return false
	}
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// stop tears down a LIVE editor: SIGTERM to the child's process GROUP, then
// SIGKILL to the group if it has not exited within vscodeStopGrace. The group (not
// the pid) is signalled because code-server spawns its own children — an extension
// host, a pty host, a Node worker per terminal — and signalling only the leader
// would strand them.
//
// It signals ONLY while the leader is unreaped. Escalating after the leader exits
// is reap()'s job, which does it adjacent to cmd.Wait(); see reap's doc for why
// that is the only safe moment. stop() cannot do it itself: waiting for the exit
// is what reaps it.
func (s *vscodeServer) stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	pgid := s.cmd.Process.Pid
	// A reaped leader means the reaper has ALREADY SIGKILLed the group — and that
	// -pgid is no longer provably ours, since an empty group's id is free for the
	// kernel to hand to an unrelated new group leader. Nothing left to signal, and
	// no safe way to signal it.
	if !s.alive() {
		return
	}
	// Alive ⇒ unreaped ⇒ the leader still holds the group id ⇒ -pgid is still ours.
	// (The alive()→signal gap is a microsecond TOCTOU inherent to POSIX group
	// signalling: there is no pgid handle to pin, and pidfd is per-process.)
	s.signalGroup(pgid, syscall.SIGTERM)
	select {
	case <-s.exited:
		// exited closes AFTER the reaper's group SIGKILL, so the group is clean and
		// there is nothing further to escalate to.
		return
	case <-time.After(vscodeStopGrace):
	}
	if s.alive() {
		s.signalGroup(pgid, syscall.SIGKILL)
	}
	// Wait for the reap so teardown can't return while the group is still up (#816).
	select {
	case <-s.exited:
	case <-time.After(vscodeStopGrace):
		log.WarningLog.Printf("vscode: editor for %s did not exit after SIGKILL", s.worktree)
	}
}

// vscodeSupervisor owns every daemon-spawned editor, keyed by
// daemonInstanceKey(repoID, title) — ONE editor per SESSION, shared by all of
// that session's vscode tabs and by every pane showing them.
//
// Lifetime is keyed to the SESSION and its vscode TABS, never to panes or
// viewers: a browser closing a pane notifies nobody, so a viewer refcount could
// only ever leak or over-kill. Tab close / archive / kill / daemon shutdown are
// authoritative, observable events, and an idle editor costs nothing meanwhile.
//
// It carries its OWN mutex rather than reusing Manager.mu: spawning an editor
// blocks on process start and a port probe for seconds, and Manager.mu is
// deliberately never held across slow calls (see the pausedMu precedent).
type vscodeSupervisor struct {
	mu      sync.Mutex
	servers map[string]*vscodeServer
	// failures records the last spawn failure per key, enforcing
	// vscodeRespawnCooldown so a broken editor can't be respawned on every
	// auto-refresh.
	failures map[string]vscodeFailure
	stopped  bool

	// Injection points for tests: configuredBinary substitutes the config key
	// without a config file, authPosture substitutes the daemon's auth settings,
	// and startGrace/cooldown shorten the waits.
	configuredBinary func() string
	authPosture      func() (requireToken, requireLoopbackToken bool)
	startGrace       time.Duration
	cooldown         time.Duration
	now              func() time.Time
}

// vscodeFailure is a remembered spawn failure and when it happened.
type vscodeFailure struct {
	err error
	at  time.Time
}

func newVSCodeSupervisor() *vscodeSupervisor {
	return &vscodeSupervisor{
		servers:  make(map[string]*vscodeServer),
		failures: make(map[string]vscodeFailure),
		configuredBinary: func() string {
			cfg, err := config.LoadConfig()
			if err != nil || cfg == nil {
				return ""
			}
			return cfg.VSCodeServerBinary
		},
		authPosture: func() (bool, bool) {
			cfg, err := config.LoadConfig()
			if err != nil || cfg == nil {
				return false, false
			}
			return cfg.RequireToken, cfg.RequireLoopbackToken
		},
		startGrace: vscodeStartGrace,
		cooldown:   vscodeRespawnCooldown,
		now:        time.Now,
	}
}

// ensureServer returns the loopback base URL of the editor serving worktree for
// key, spawning it if it is not running and RESPAWNING it if it died — the
// self-heal that makes a crashed editor recover on the next render rather than
// needing the tab recreated. It returns errVSCodeBinaryMissing when no editor is
// installed, which callers render as the install hint.
func (v *vscodeSupervisor) ensureServer(key, worktree string) (string, error) {
	if strings.TrimSpace(worktree) == "" {
		return "", fmt.Errorf("session has no worktree to open in VS Code")
	}

	// Hold the supervisor lock for the whole call. It serializes concurrent
	// first-renders of one session onto ONE editor (the point of keying by
	// session), and it is the supervisor's own lock — never Manager.mu — so even
	// the blocking start grace stalls only other editor spawns, not unrelated RPCs.
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return "", fmt.Errorf("daemon is shutting down")
	}

	// Reuse a live editor, but only while it still serves THIS worktree: a session
	// restored to a different path (or a key reused after a kill) must never be
	// handed an editor rooted at the old directory.
	if s := v.servers[key]; s != nil {
		switch {
		case s.alive() && s.worktree == worktree:
			if s.ready || v.probeReady(s) {
				return s.baseURL, nil
			}
			// Up, but not listening yet — keep waiting, don't respawn.
			return "", errVSCodeStarting
		default:
			// Dead, or serving a stale worktree: drop it and start clean. Stop it
			// OUT OF BAND — stop() blocks for up to the stop grace, and v.mu is held
			// across the spawn below, so stopping inline would stall every OTHER
			// session's editor behind this one teardown. It is already unregistered
			// here, so nothing else can reach it and no one needs to wait for it.
			// A DEAD leader needs nothing from stop() — its reaper already killed
			// the group at the only moment that was safe to — but stop() is correct
			// and immediate on one, so the branch stays uniform.
			neverReady := !s.ready && !s.alive()
			delete(v.servers, key)
			go s.stop()
			// An editor that died having NEVER become ready is a broken start, not a
			// crash to heal from — record it so the cooldown applies. Without this
			// only spawnLocked's own errors were recorded, so an editor that outlived
			// the start grace and THEN died would be respawned by every auto-refresh
			// of the "starting" notice: a spawn loop driven by our own UI. A server
			// that WAS ready and then died is the opposite case — a genuine crash —
			// and must still respawn at once, so it is deliberately not recorded.
			if neverReady {
				v.failures[key] = vscodeFailure{
					err: errVSCodeStartExited,
					at:  v.now(),
				}
			}
		}
	}

	// Replay a recent failure instead of respawning, so the notice page's refresh
	// can't drive a spawn loop against a broken editor.
	if f, ok := v.failures[key]; ok && v.now().Sub(f.at) < v.cooldown {
		return "", f.err
	}

	binary, err := resolveVSCodeBinary(v.configuredBinary())
	if err != nil {
		// A missing binary is a stable, cheap-to-detect state, not a failed spawn:
		// recording it would make an install invisible for the cooldown.
		return "", err
	}

	server, err := v.spawnLocked(binary, worktree)
	if err != nil {
		v.failures[key] = vscodeFailure{err: err, at: v.now()}
		return "", err
	}
	delete(v.failures, key)
	v.servers[key] = server
	if !server.ready {
		return "", errVSCodeStarting
	}
	return server.baseURL, nil
}

// probeReady re-checks a not-yet-ready editor's port and latches s.ready on
// success. Callers must hold v.mu. The dial timeout is deliberately tiny: this
// runs on a proxy request, and "not up yet" must be answered immediately rather
// than waited out.
func (v *vscodeSupervisor) probeReady(s *vscodeServer) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)), vscodeProbeInterval)
	if err != nil {
		return false
	}
	_ = conn.Close()
	s.ready = true
	return true
}

// spawnLocked starts one editor on a fresh ephemeral port and waits for it to
// listen. Callers must hold v.mu.
func (v *vscodeSupervisor) spawnLocked(binary, worktree string) (*vscodeServer, error) {
	flavor := flavorForBinary(binary)
	v.warnIfLocalAuthExpected(worktree)
	var lastErr error
	for attempt := 0; attempt < vscodeSpawnAttempts; attempt++ {
		port, err := freeLoopbackPort()
		if err != nil {
			lastErr = err
			continue
		}
		server, err := v.startOne(binary, flavor, port, worktree)
		if err != nil {
			lastErr = err
			continue
		}
		return server, nil
	}
	return nil, fmt.Errorf("starting %s failed: %w", filepath.Base(binary), lastErr)
}

// warnIfLocalAuthExpected tells an operator who asked for local-process auth that
// the editor listener does not provide it.
//
// require_token + require_loopback_token together are the documented way to lock
// down a SHARED machine: they make even loopback peers present a token, closing
// the gap that af's defaults leave open on purpose. Setting both is an explicit
// opt-in, and the daemon-spawned editor silently reopens exactly that gap on its
// own port. Nobody who set them would guess that from the tab, so say it. (Only
// for that pair: under the defaults there is nothing to bypass, and warning every
// user about a posture they never asked to change is noise.)
func (v *vscodeSupervisor) warnIfLocalAuthExpected(worktree string) {
	if v.authPosture == nil {
		return
	}
	if requireToken, requireLoopbackToken := v.authPosture(); !requireToken || !requireLoopbackToken {
		return
	}
	log.WarningLog.Printf("vscode: require_token and require_loopback_token are set, but the editor for %s listens on loopback with authentication disabled — any local process can reach it without a token. A follow-up moves the editor onto a 0600 unix socket.", worktree)
}

// startOne execs one editor and waits for its port to accept connections.
func (v *vscodeSupervisor) startOne(binary string, flavor vscodeFlavor, port int, worktree string) (*vscodeServer, error) {
	const host = "127.0.0.1"
	// Check the worktree before exec'ing. os/exec reports a missing cmd.Dir as
	// ENOENT naming the BINARY ("fork/exec /usr/bin/code-server: no such file or
	// directory"), which sends the user off debugging a code-server install that
	// is perfectly fine. Name the directory that is actually missing instead.
	if fi, err := os.Stat(worktree); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("the session worktree %s is not a directory (has it been moved or removed?): %w", worktree, err)
	}
	cmd := exec.Command(binary, vscodeArgs(flavor, host, port, worktree)...)
	cmd.Dir = worktree
	cmd.Env = vscodeChildEnv()
	// Own process group so the editor's whole tree (extension host, terminal
	// workers) can be signalled together on teardown, mirroring the watcher
	// supervisor (#610/#769).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The editor's own logs are noise on the daemon's stdout and its stdout is
	// never read; discard both streams so a chatty editor can never fill a pipe
	// buffer and wedge itself.
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec %s: %w", binary, err)
	}
	server := &vscodeServer{
		worktree: worktree,
		baseURL:  "http://" + net.JoinHostPort(host, strconv.Itoa(port)),
		host:     host,
		port:     port,
		cmd:      cmd,
		exited:   make(chan struct{}),
	}
	// Reap the child AND clean up its process group; see reap's doc for why the
	// group kill belongs there rather than in stop().
	go server.reap()

	switch err := waitForPort(host, port, server.exited, v.startGrace); {
	case err == nil:
		server.ready = true
		return server, nil
	case errors.Is(err, errVSCodeStarting):
		// Still coming up. Hand the caller a live, not-ready server rather than
		// killing it — a cold code-server legitimately exceeds the grace, and
		// killing it here would restart the clock forever and never start.
		return server, nil
	default:
		// The child exited (bad binary, bad worktree) or the port could not be
		// probed: never leave it running — it would hold the port and be adopted
		// by the next ensureServer as if it were healthy.
		server.stop()
		return nil, err
	}
}

// vscodeChildEnv builds the editor's environment from the daemon's, with the tmux
// ancestry markers and VSCODE_IPC_HOOK_CLI REMOVED.
//
// This is load-bearing, not hygiene. The daemon inherits its environment from
// whatever autostarted it — often a TUI running inside an af_ tmux pane — so it
// can be carrying AF_SESSION/AF_HOME, and every child it spawns inherits them
// too. /proc/<pid>/environ is fixed at exec and can never be shed, so a
// code-server stamped with a session marker is attributed to that session
// forever: once that session dies, `af doctor --fix` matches the marker plus the
// home and KILLS the editor as a leaked process (doctor/checks.go). Scrubbing the
// markers keeps a daemon-owned editor out of that attribution entirely.
//
// (The tmux teardown reaper is a separate mechanism and never sees this child at
// all: it captures only a tmux pane's descendants and its pane-SID members, and a
// daemon child is neither — the daemon is its own session leader via Setsid.)
//
// VSCODE_IPC_HOOK_CLI is scrubbed for the same reason and it is just as
// load-bearing. code-server's shouldOpenInExistingInstance checks it
// UNCONDITIONALLY, before it starts any server, and when it is set the CLI hands
// the folder to that existing editor over the IPC socket and EXITS — --bind-addr
// is never honored. So a daemon started from any VS Code / code-server integrated
// terminal inherits the var, and then every editor it ever spawns dies during
// startup (the pane shows a broken-editor notice despite a perfectly good
// install) while the worktree pops open in the USER's own window instead. The var
// is fixed in the daemon's environ at exec, so this is sticky for the daemon's
// whole life — and af's own VS Code tab has an integrated terminal that sets it,
// which makes `af` run from inside an af VS Code tab poison the daemon.
//
// Only what breaks the spawn is scrubbed. The git-askpass family
// (VSCODE_GIT_ASKPASS_*, VSCODE_GIT_IPC_HANDLE, GIT_ASKPASS) also inherits stale
// handles, but code-server overwrites those for its own terminals, so removing
// them buys nothing; the shell-integration markers (VSCODE_INJECTION, VSCODE_PID,
// TERM_PROGRAM, …) the editor resets itself. Blanket-scrubbing VSCODE_* would
// trade a filter you can audit against upstream for one that merely looks tidy.
var vscodeScrubbedEnv = []string{
	tmux.EnvMarkerSession,
	tmux.EnvMarkerHome,
	"VSCODE_IPC_HOOK_CLI",
}

func vscodeChildEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if hasAnyEnvPrefix(kv, vscodeScrubbedEnv) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// hasAnyEnvPrefix reports whether the KEY=VALUE entry kv names any of keys.
func hasAnyEnvPrefix(kv string, keys []string) bool {
	for _, k := range keys {
		if strings.HasPrefix(kv, k+"=") {
			return true
		}
	}
	return false
}

// freeLoopbackPort asks the kernel for an unused loopback port by binding :0 and
// reading back the pick.
//
// The listener must be closed before the port is handed to the child, which opens
// a TOCTOU window where another process can take it. There is no way to avoid it
// while spawning a child that binds for itself; losing the race surfaces as a
// failed start, which spawnLocked retries with a fresh port.
func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocating a loopback port failed: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if cerr := ln.Close(); cerr != nil {
		return 0, fmt.Errorf("releasing the probed loopback port failed: %w", cerr)
	}
	return port, nil
}

// waitForPort blocks until host:port accepts a connection (nil), the child exits
// (a real error), or grace elapses (errVSCodeStarting — still coming up).
//
// Watching exited is what separates the two failure shapes: a child that dies
// instantly (a bad binary, an unreadable worktree) is reported immediately and
// accurately instead of being mistaken for a slow start and waited out.
func waitForPort(host string, port int, exited <-chan struct{}, grace time.Duration) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(grace)
	for {
		select {
		case <-exited:
			// Wrapped, not bare: this is the errVSCodeStartExited case, and callers
			// (the proxy's notice page, the respawn cooldown) match the SENTINEL to
			// render a styled notice rather than surfacing a raw error.
			return fmt.Errorf("%w (check that it runs correctly: it was asked to serve %s)", errVSCodeStartExited, addr)
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, vscodeProbeInterval)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return errVSCodeStarting
		}
		time.Sleep(vscodeProbeInterval)
	}
}

// stopFor tears down the editor for key, if any. Safe to call for a session that
// never had one, so lifecycle paths can call it unconditionally.
func (v *vscodeSupervisor) stopFor(key string) {
	v.mu.Lock()
	server := v.servers[key]
	delete(v.servers, key)
	v.mu.Unlock()
	// Stop OUTSIDE the lock: it blocks for up to the stop grace, and holding the
	// supervisor lock across it would stall every unrelated session's editor.
	if server != nil {
		server.stop()
	}
}

// Stop tears down every editor and refuses further spawns, so daemon shutdown
// leaves no orphaned code-server behind. It mirrors watcherSupervisor.Stop:
// snapshot under the lock, then stop outside it.
func (v *vscodeSupervisor) Stop() {
	v.mu.Lock()
	v.stopped = true
	servers := make([]*vscodeServer, 0, len(v.servers))
	for _, s := range v.servers {
		servers = append(servers, s)
	}
	v.servers = make(map[string]*vscodeServer)
	v.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s *vscodeServer) {
			defer wg.Done()
			s.stop()
		}(s)
	}
	wg.Wait()
}
