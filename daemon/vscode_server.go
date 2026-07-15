package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
// bound to a 0600 UNIX SOCKET in a 0700 directory. The browser never reaches it
// directly: every request arrives through the daemon's /v1/webtab/ reverse proxy,
// which is what makes the editor visible to a REMOTE viewer (Tailscale/SSH) and
// what carries the daemon's auth policy in front of an --auth none editor.
//
// THE SOCKET IS THE AUTH (#1873). The editor runs with authentication disabled,
// so whatever can reach its listener has worktree read/write and terminal exec as
// the af user. It used to listen on a loopback TCP port, which made it a SECOND,
// UNGUARDED route: any local process could scan the port and connect. That is a
// real bypass for the operator who sets require_token + require_loopback_token —
// config_types.go documents those as exactly the control that makes local peers
// authenticate — because a daemon-spawned editor silently reopened the gap they
// closed. Filesystem permissions on a unix socket are the fix, and they give the
// editor precisely the posture af's own control socket already has: 0600, so only
// the owning user can connect.
//
// It also removes the port TOCTOU BY CONSTRUCTION. Choosing a TCP port meant
// bind(:0) → read → close → hand the number to the child, a window in which
// another process could take the port; the readiness probe would then dial the
// FOREIGN listener, latch ready, and the proxy would relay a stranger's service
// into the editor iframe under af's auth. There is no bind-close-rebind here.
// The daemon names the socket itself — unguessably, and inside a directory only
// it can write — then unlinks the path before the child binds it, so a dial can
// only ever reach our own child. The readiness probe did not get smarter (it
// still cannot tell who is behind a path); the race it could lose stopped
// existing.
//
// Socket names are per-PROCESS, not per session, and that is load-bearing rather
// than incidental — see vscodeSocketPath.
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

// vscodeSocketDirName is the directory, under the af home, holding every editor
// socket. It is kept SHORT deliberately: every byte of it counts against the
// sockaddr_un path limit (see vscodeSocketPath).
const vscodeSocketDirName = "vscode"

// vscodeSocketExt is the socket file's extension. The sweep matches on it, so a
// file the daemon did not create is never removed.
const vscodeSocketExt = ".sock"

// vscodeSocketMode is the socket's file mode, passed to code-server's
// --socket-mode and applied by startOne for flavors that have no mode flag. 0600:
// the owning user only, matching the daemon's own control socket.
const vscodeSocketMode = "0600"

// maxUnixSocketPathLen bounds a socket path. sockaddr_un.sun_path is 108 bytes on
// Linux and 104 on macOS; 103 is the portable ceiling once the NUL terminator is
// counted. Exceeding it fails inside the kernel as an opaque "invalid argument",
// so the limit is checked up front where the message can name the cause.
const maxUnixSocketPathLen = 103

// vscodeUpstreamHost is the Host the proxy presents to a socket-bound editor.
//
// A unix socket has no host, but HTTP still requires one, and it must be STABLE
// and ours: the editor echoes it into any self-redirect, and rewriteUpstreamRef
// only pulls a redirect back under the tab prefix when its host matches the
// target's. A .invalid name (RFC 2606) can never resolve, so it cannot be
// confused with a real origin and no stray DNS lookup can escape.
const vscodeUpstreamHost = "vscode.invalid"

// vscodeUpstreamURL is the dummy base URL of a socket-bound editor. The transport
// dials the socket and ignores the host entirely; only the path and the Host
// header survive to the child.
const vscodeUpstreamURL = "http://" + vscodeUpstreamHost

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
	// flavorCodeServer is coder/code-server: `--socket`/`--socket-mode`,
	// `--auth none`.
	flavorCodeServer vscodeFlavor = iota
	// flavorOpenVSCode is gitpod-io/openvscode-server: `--socket-path` (no mode
	// flag), `--without-connection-token`.
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

// vscodeArgs builds the child's argv for flavor, serving worktree on socketPath.
//
// The unix socket is the ONLY listener both dialects agree on, which is why it is
// the transport (#1873): their auth mechanisms do not converge (code-server's
// --auth password is cookie-based with no openvscode equivalent; openvscode's
// --connection-token has no code-server equivalent), but both bind a socket.
//
// The two differ on MODE, and it decides how the socket gets its permissions:
// code-server takes --socket-mode and chmods the socket itself (after listen —
// see the 0700 directory note on vscodeSocketDir), while openvscode-server's
// --socket-path has NO mode flag at all, so its socket lands under the daemon's
// umask and could be world-connectable. The 0700 PARENT DIRECTORY is therefore
// the load-bearing guard for both, not a belt-and-braces extra: it fences the
// socket regardless of the mode any flavor happens to give it, and it closes the
// window between the child's bind() and any chmod. startOne additionally chmods
// the socket to 0600 once it appears, so the mode is right in its own right.
//
// NOTE on base paths: code-server ALWAYS listens at the root and emits RELATIVE
// URLs derived from the request path's depth, so it needs no base-path flag and
// the proxy must simply strip its prefix (which webTabProxyHandler does).
// `--abs-proxy-base-path` is NOT that flag — it only prefixes code-server's own
// /absproxy/<port> feature and has no effect on serving code-server itself under
// a sub-path (coder/code-server#6770). Passing it here would be a no-op at best.
func vscodeArgs(flavor vscodeFlavor, socketPath, worktree string) []string {
	switch flavor {
	case flavorOpenVSCode:
		return []string{
			"--socket-path", socketPath,
			// openvscode-server's --auth-none equivalent. Safe here because the
			// only route to the socket is the daemon proxy, which applies the
			// daemon's auth policy, and the socket's 0600-in-0700 perms restrict
			// it to the owning user.
			"--without-connection-token",
			// The worktree must be a FLAG, not a positional path: openvscode-server
			// resolves the workbench folder only from --default-folder, and its
			// parser ACCEPTS a positional argument while never reading it — so a
			// positional worktree is silently ignored and the editor opens empty
			// (cmd.Dir does not rescue it either; the web client server never
			// derives the folder from cwd). Verified against openvscode-server
			// 1.109.5: positional → no folderUri in the workbench, --default-folder
			// → the worktree opens. code-server, by contrast, DOES read a
			// positional path, which is why the two branches differ.
			//
			// Found by the #1817 post-merge review and fixed in flight by #1880;
			// carried here because this rewrite owns the line.
			"--default-folder", worktree,
		}
	default:
		return []string{
			// code-server ignores --bind-addr entirely when --socket is set, so
			// this replaces the TCP listener rather than adding to it.
			"--socket", socketPath,
			"--socket-mode", vscodeSocketMode,
			// Safe ONLY because the sole route to the socket is the daemon proxy,
			// which applies the daemon's own auth policy, and the socket's perms
			// restrict it to the owning user.
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
	// socketPath is the editor's ONLY endpoint: a 0600 unix socket in a 0700
	// directory. There is no host or port — the proxy's transport dials this.
	socketPath string
	// transport dials socketPath, ignoring the dummy host in the URL. It is owned
	// by THIS server, not shared and not per-request, and that is a correctness
	// choice at both ends:
	//
	//   - per-request would pool nothing (a fresh pool per request) while leaking
	//     idle sockets, since a zero-value Transport never times an idle conn out;
	//   - one shared transport would pool by HOST, and every editor presents the
	//     same dummy host — so a request for one session could be answered over a
	//     pooled connection to ANOTHER session's editor.
	//
	// Tying it to the process also makes respawn clean: a new process gets a new
	// transport with an empty pool, so no connection to the dead socket survives.
	transport *http.Transport
	cmd       *exec.Cmd

	// ready latches once the editor has accepted a connection. Until then the
	// process is up but must not be proxied to, so callers see errVSCodeStarting
	// instead of a 502 from a socket that is not listening yet.
	ready bool

	// exited is closed by the reaping goroutine when the child exits, so alive
	// can distinguish "running" from "died" without a syscall.
	exited chan struct{}
}

// newVSCodeTransport builds the transport that reaches an editor on socketPath.
//
// DialContext DISCARDS the network and address it is handed: those come from the
// dummy vscode.invalid URL, which exists only to satisfy HTTP and must never be
// resolved. The socket path is captured here instead, so the target of every dial
// is fixed at spawn by the daemon and cannot be influenced by a request.
func newVSCodeTransport(socketPath string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		// A cold editor loads a few hundred assets, so keep-alive matters; the
		// idle timeout just stops a closed pane holding sockets open forever.
		MaxIdleConns:    32,
		IdleConnTimeout: 90 * time.Second,
	}
}

// vscodeEndpoint is how the proxy reaches one running editor: the socket to dial
// and the transport that dials it. The two travel together because neither is
// usable alone — the path names an endpoint no URL can express, and the transport
// is the only thing that knows how to reach it.
type vscodeEndpoint struct {
	SocketPath string
	Transport  *http.Transport
}

// endpoint is how the proxy reaches this editor.
func (s *vscodeServer) endpoint() vscodeEndpoint {
	return vscodeEndpoint{SocketPath: s.socketPath, Transport: s.transport}
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

// stop SIGTERMs the child's process GROUP, then SIGKILLs the group if it has not
// exited within vscodeStopGrace. The group (not the pid) is signalled because
// code-server spawns its own children — an extension host and a Node worker per
// terminal — and signalling only the leader would strand them.
func (s *vscodeServer) stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	pgid := s.cmd.Process.Pid
	if s.alive() {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-s.exited:
		case <-time.After(vscodeStopGrace):
		}
	}
	// SIGKILL the GROUP unconditionally, even once the leader is gone. The leader
	// exiting proves nothing about the rest of the group: code-server spawns an
	// extension host, a pty host, and a process per integrated terminal, and any of
	// them can ignore SIGTERM or outlive their parent — returning at leader-exit
	// would strand exactly those, which is the leak this whole supervisor exists to
	// prevent. Mirrors the watcher supervisor's unconditional group kill (#610/#769).
	//
	// Signalling the group after the leader is reaped is still safe: a process group
	// stays allocated while ANY member lives, so -pgid cannot be recycled out from
	// under us while there is something to kill, and an empty group is a harmless
	// ESRCH.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	// Reap so the group-kill can't leave a zombie parented to the daemon (#816).
	select {
	case <-s.exited:
	case <-time.After(vscodeStopGrace):
		log.WarningLog.Printf("vscode: editor for %s did not exit after SIGKILL", s.worktree)
	}
	// Unlink the socket. A SIGKILLed process never cleans up after itself, and an
	// editor that exits on its own is not guaranteed to either, so without this the
	// 0700 directory would accumulate a dead socket per session for the daemon's
	// life. Spawning re-unlinks the path anyway (a stale socket is inert, not a
	// hazard: nothing listens on it, so a dial gets ECONNREFUSED), which is why the
	// failure is only logged at debug volume — this is hygiene, not correctness.
	if s.socketPath != "" {
		if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
			log.WarningLog.Printf("vscode: removing the editor socket %s failed: %v", s.socketPath, err)
		}
	}
	// Drop pooled connections to the socket we just unlinked. The child is dead so
	// the kernel has closed them anyway; this releases the descriptors now rather
	// than leaving them for the transport to discover lazily on a request that
	// will never come.
	if s.transport != nil {
		s.transport.CloseIdleConnections()
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

	// sweepOnce gates the one-time cleanup of sockets abandoned by a previous
	// daemon. It fires on the first spawn, when this daemon owns no editors and
	// everything in the socket directory is therefore stale.
	sweepOnce sync.Once

	// Injection points for tests: configuredBinary substitutes the config key
	// without a config file, and startGrace/cooldown shorten the waits.
	configuredBinary func() string
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
		startGrace: vscodeStartGrace,
		cooldown:   vscodeRespawnCooldown,
		now:        time.Now,
	}
}

// ensureServer returns the ENDPOINT of the editor serving worktree for key,
// spawning it if it is not running and RESPAWNING it if it died — the self-heal
// that makes a crashed editor recover on the next render rather than needing the
// tab recreated. It returns errVSCodeBinaryMissing when no editor is installed,
// which callers render as the install hint.
func (v *vscodeSupervisor) ensureServer(key, worktree string) (vscodeEndpoint, error) {
	if strings.TrimSpace(worktree) == "" {
		return vscodeEndpoint{}, fmt.Errorf("session has no worktree to open in VS Code")
	}

	// Hold the supervisor lock for the whole call. It serializes concurrent
	// first-renders of one session onto ONE editor (the point of keying by
	// session), and it is the supervisor's own lock — never Manager.mu — so even
	// the blocking start grace stalls only other editor spawns, not unrelated RPCs.
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return vscodeEndpoint{}, fmt.Errorf("daemon is shutting down")
	}

	// Reuse a live editor, but only while it still serves THIS worktree: a session
	// restored to a different path (or a key reused after a kill) must never be
	// handed an editor rooted at the old directory.
	if s := v.servers[key]; s != nil {
		switch {
		case s.alive() && s.worktree == worktree:
			if s.ready || v.probeReady(s) {
				return s.endpoint(), nil
			}
			// Up, but not listening yet — keep waiting, don't respawn.
			return vscodeEndpoint{}, errVSCodeStarting
		default:
			// Dead, or serving a stale worktree: drop it and start clean. Stop it
			// OUT OF BAND — stop() blocks for up to the stop grace, and v.mu is held
			// across the spawn below, so stopping inline would stall every OTHER
			// session's editor behind this one teardown. It is already unregistered
			// here, so nothing else can reach it and no one needs to wait for it.
			// Even a DEAD leader is worth stopping: its children can outlive it.
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
		return vscodeEndpoint{}, f.err
	}

	binary, err := resolveVSCodeBinary(v.configuredBinary())
	if err != nil {
		// A missing binary is a stable, cheap-to-detect state, not a failed spawn:
		// recording it would make an install invisible for the cooldown.
		return vscodeEndpoint{}, err
	}

	server, err := v.spawnLocked(key, binary, worktree)
	if err != nil {
		v.failures[key] = vscodeFailure{err: err, at: v.now()}
		return vscodeEndpoint{}, err
	}
	delete(v.failures, key)
	v.servers[key] = server
	if !server.ready {
		return vscodeEndpoint{}, errVSCodeStarting
	}
	return server.endpoint(), nil
}

// probeReady re-checks a not-yet-ready editor's socket and latches s.ready on
// success. Callers must hold v.mu. The dial timeout is deliberately tiny: this
// runs on a proxy request, and "not up yet" must be answered immediately rather
// than waited out.
func (v *vscodeSupervisor) probeReady(s *vscodeServer) bool {
	conn, err := net.DialTimeout("unix", s.socketPath, vscodeProbeInterval)
	if err != nil {
		return false
	}
	_ = conn.Close()
	// This is the OTHER path on which a socket first becomes reachable: an editor
	// that outran the start grace latches ready here, not in startOne, so the mode
	// has to be forced here too or a slow openvscode-server would keep whatever
	// mode its umask gave it.
	//
	// Unlike startOne this only warns. The socket just accepted a connection, so
	// it exists and is ours; a chmod that still fails means something is wrong with
	// the filesystem, not the security posture — and the 0700 directory already
	// restricts the socket to the owning user either way. Refusing to latch would
	// wedge the pane on the "starting" notice forever over a condition the
	// directory has already handled.
	if err := secureVSCodeSocket(s.socketPath); err != nil {
		log.WarningLog.Printf("vscode: %v", err)
	}
	s.ready = true
	return true
}

// secureVSCodeSocket forces socketPath to 0600.
//
// Neither flavor can be trusted to have done it: code-server applies
// --socket-mode only AFTER it finishes listening, and openvscode-server's
// --socket-path has no mode flag at all, so its socket lands under the daemon's
// umask. The 0700 parent directory is what makes both gaps harmless; this makes
// the socket correct in its own right, so the editor's posture does not rest on
// the directory alone.
func secureVSCodeSocket(socketPath string) error {
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("securing the VS Code socket %s failed: %w", socketPath, err)
	}
	return nil
}

// spawnLocked starts one editor on key's socket and waits for it to listen.
// Callers must hold v.mu.
//
// There is no retry loop. The TCP path needed one to re-roll a lost port race;
// a socket path is chosen by the daemon inside a directory only the daemon can
// write, so there is no race to lose and a failure here is a real failure (#1873).
func (v *vscodeSupervisor) spawnLocked(key, binary, worktree string) (*vscodeServer, error) {
	// Before the first editor of this daemon's life, clear out any left by the
	// last one. Safe here precisely because nothing has spawned yet.
	v.sweepAbandonedSockets()
	socketPath, err := vscodeSocketPath(key)
	if err != nil {
		return nil, err
	}
	server, err := v.startOne(binary, flavorForBinary(binary), socketPath, worktree)
	if err != nil {
		return nil, fmt.Errorf("starting %s failed: %w", filepath.Base(binary), err)
	}
	return server, nil
}

// startOne execs one editor and waits for its socket to accept connections.
func (v *vscodeSupervisor) startOne(binary string, flavor vscodeFlavor, socketPath, worktree string) (*vscodeServer, error) {
	// Check the worktree before exec'ing. os/exec reports a missing cmd.Dir as
	// ENOENT naming the BINARY ("fork/exec /usr/bin/code-server: no such file or
	// directory"), which sends the user off debugging a code-server install that
	// is perfectly fine. Name the directory that is actually missing instead.
	if fi, err := os.Stat(worktree); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("the session worktree %s is not a directory (has it been moved or removed?): %w", worktree, err)
	}
	// Clear a stale socket before the child binds. A crashed daemon (or a SIGKILL
	// that outran stop's unlink) leaves the file behind, and bind() on an existing
	// path is EADDRINUSE — code-server unlinks first itself, but openvscode-server
	// makes no such promise, so an editor would simply refuse to start until the
	// file was removed by hand. Removing it here is safe precisely because the
	// directory is ours alone: the path can only ever hold OUR dead socket.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clearing the stale VS Code socket %s failed: %w", socketPath, err)
	}
	cmd := exec.Command(binary, vscodeArgs(flavor, socketPath, worktree)...)
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
		worktree:   worktree,
		socketPath: socketPath,
		transport:  newVSCodeTransport(socketPath),
		cmd:        cmd,
		exited:     make(chan struct{}),
	}
	// Always Wait() the child: without it an exited editor stays a zombie
	// parented to the daemon for the daemon's whole life (#816).
	go func() {
		_ = cmd.Wait()
		close(server.exited)
	}()

	switch err := waitForSocket(socketPath, server.exited, v.startGrace); {
	case err == nil:
		server.ready = true
		// Force the mode now that the socket exists. Fatal here, unlike in
		// probeReady: this is the spawn path, nothing is serving yet, and refusing
		// to hand back an editor whose socket could not be secured is strictly
		// better than serving one that might be world-connectable.
		if err := secureVSCodeSocket(socketPath); err != nil {
			server.stop()
			return nil, err
		}
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

// vscodeChildEnv builds the editor's environment from the daemon's, with the
// tmux ancestry markers REMOVED.
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
func vscodeChildEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if strings.HasPrefix(kv, tmux.EnvMarkerSession+"=") || strings.HasPrefix(kv, tmux.EnvMarkerHome+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// vscodeSocketDir returns the directory every editor socket lives in, creating it
// 0700.
//
// The 0700 mode is the actual access control (#1873), which is why it is forced
// on an EXISTING directory too rather than left to MkdirAll (a no-op when the
// path is already there, so a dir created loose by an older build — or by a
// permissive umask — would stay loose forever). It fences the socket during the
// window between the child's bind() and any chmod, and it is the ONLY thing
// protecting an openvscode-server socket, whose --socket-path has no mode flag.
// Together with the 0600 socket this gives the editor exactly the posture of the
// daemon's own control socket: reachable by the owning user, nobody else.
func vscodeSocketDir() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	sockDir := filepath.Join(dir, vscodeSocketDirName)
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return "", fmt.Errorf("creating the VS Code socket directory failed: %w", err)
	}
	if err := os.Chmod(sockDir, 0o700); err != nil {
		return "", fmt.Errorf("securing the VS Code socket directory failed: %w", err)
	}
	return sockDir, nil
}

// vscodeSocketPath returns a FRESH socket path for key: a hash of the key, to
// identify the session, plus a random nonce, to identify the process.
//
// The key is hashed rather than used directly because a session key carries a
// repo id and a user-chosen title — long, possibly non-ASCII, possibly holding a
// path separator — while a socket path must be one file name inside the socket
// directory and fits in ~104 bytes.
//
// The NONCE is load-bearing, and a key-derived path alone was a bug. Teardown
// unlinks the socket, and teardown runs CONCURRENTLY with the respawn that
// replaces it: ensureServer drops a dead-or-stale editor with `go s.stop()` and
// immediately spawns its replacement under the same key, and stopFor races a
// concurrent spawn the same way (see the CloseTab window in webtab_proxy.go).
// With one path per key, the old server's unlink lands AFTER the new server has
// bound that path and deletes a LIVE editor's socket — leaving it listening on
// an unnamed socket that no dial can ever reach again. A per-process path makes
// "unlink my own socket" unconditionally safe, because no other server can ever
// hold the same one.
//
// The length check is a real failure mode, not paranoia: AGENT_FACTORY_HOME can
// point anywhere (a deep temp dir under CI), and an over-long path fails inside
// net.Listen as a bare "invalid argument" that names nothing. Fail early instead,
// naming the directory the operator can move.
func vscodeSocketPath(key string) (string, error) {
	dir, err := vscodeSocketDir()
	if err != nil {
		return "", err
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generating the VS Code socket name failed: %w", err)
	}
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:4]) + "-" + hex.EncodeToString(nonce[:]) + vscodeSocketExt
	path := filepath.Join(dir, name)
	if len(path) > maxUnixSocketPathLen {
		return "", fmt.Errorf("the VS Code socket path %q is %d bytes, over the %d-byte limit for a unix socket: "+
			"set AGENT_FACTORY_HOME to a shorter path", path, len(path), maxUnixSocketPathLen)
	}
	return path, nil
}

// sweepAbandonedSockets removes every socket left behind by a PREVIOUS daemon.
//
// Socket names carry a nonce, so a dead daemon's sockets are never reused and
// nothing would otherwise ever remove them — a SIGKILLed daemon would leak one
// file per session, for the life of the af home. This is the counterpart of that
// choice, not an extra.
//
// It runs once, on the first spawn rather than at construction, and that timing
// is what makes it safe: a supervisor that has never spawned owns no editors, so
// every socket in the directory is by definition abandoned. (Waiting for a spawn
// also means a daemon that never opens an editor neither creates the directory
// nor touches anything.) The singleton lock guarantees no other daemon owns this
// af home meanwhile.
//
// Best-effort throughout: a failed sweep costs litter, never correctness, and
// must not stop an editor from starting.
func (v *vscodeSupervisor) sweepAbandonedSockets() {
	v.sweepOnce.Do(func() {
		dir, err := vscodeSocketDir()
		if err != nil {
			log.WarningLog.Printf("vscode: %v", err)
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			log.WarningLog.Printf("vscode: reading the socket directory %s failed: %v", dir, err)
			return
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != vscodeSocketExt {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.WarningLog.Printf("vscode: removing the abandoned socket %s failed: %v", path, err)
			}
		}
	})
}

// waitForSocket blocks until socketPath accepts a connection (nil), the child
// exits (a real error), or grace elapses (errVSCodeStarting — still coming up).
//
// Watching exited is what separates the two failure shapes: a child that dies
// instantly (a bad binary, an unreadable worktree) is reported immediately and
// accurately instead of being mistaken for a slow start and waited out.
//
// Unlike the TCP probe this replaces, a successful dial here PROVES the connection
// is our child (#1873). startOne unlinked the path and only the daemon can write
// the 0700 directory, so nothing else can have created the socket — where the old
// waitForPort could dial a foreign listener that won a port race and report it as
// our ready editor.
func waitForSocket(socketPath string, exited <-chan struct{}, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	for {
		select {
		case <-exited:
			return fmt.Errorf("the VS Code server exited during startup (check that it runs correctly: it was asked to serve %s)", socketPath)
		default:
		}
		conn, err := net.DialTimeout("unix", socketPath, vscodeProbeInterval)
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
