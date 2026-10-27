package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// The per-server process unit the supervisor lifecycle in vscode_server.go acts
// on: which binary to run (the two flavors disagree on every flag that matters),
// the argv to launch it with, and the vscodeServer itself — reap/alive/stop,
// including why the process-group kill is only safe adjacent to cmd.Wait().
// The transport that unit listens on lives in vscode_socket.go.

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
// is deprecated upstream in favor of exactly this one.) Verified against
// openvscode-server 1.109.5: positional yields no folderUri in the workbench,
// --default-folder opens the worktree. (#1880)
func vscodeArgs(flavor vscodeFlavor, socketPath, worktree string) []string {
	switch flavor {
	case flavorOpenVSCode:
		return []string{
			"--socket-path", socketPath,
			// openvscode-server's --auth none equivalent: no token on any request.
			// Same posture, and the same reasoning, as code-server's below: the
			// socket's 0600-in-0700 perms are what authenticate, and the daemon
			// proxy is the only route to it.
			"--without-connection-token",
			"--default-folder", worktree,
		}
	default:
		return []string{
			// code-server ignores --bind-addr entirely when --socket is set, so
			// this replaces the TCP listener rather than adding to it.
			"--socket", socketPath,
			"--socket-mode", vscodeSocketMode,
			// No auth on the editor itself, and unlike before it needs none: the
			// daemon proxy is the only route to the socket, and the socket's
			// 0600-in-0700 perms restrict it to the owning user. (Before #1873 this
			// sat on a loopback port that any local process could reach.)
			"--auth", "none",
			// code-server AUTO-GENERATES ~/.config/code-server/config.yaml (with a
			// random password) when no config exists. Point it at /dev/null so a
			// daemon-spawned editor neither depends on nor mutates the user's
			// config file, and every setting comes from this argv.
			"--config", os.DevNull,
			"--disable-telemetry",
			"--disable-update-check",
			"--disable-workspace-trust",
			// AF runs one editor per session but deliberately leaves code-server's
			// user-data directory shared so settings and extensions carry across
			// them. Its remembered last workspace is shared too, and without this
			// flag a second server can redirect to another session's folder even
			// though the positional argument below names the right worktree (#2683).
			"--ignore-last-opened",
			worktree,
		}
	}
}

// vscodeServer is one supervised editor process serving one session's worktree.
type vscodeServer struct {
	worktree string
	// account is the selected account NAME this editor's environment was built
	// for, empty for the ambient identity. It is part of the reuse identity: the
	// credential root is fixed in the child's environ at exec, so an editor whose
	// session has since selected a different account can only be replaced, never
	// re-scoped (#3870).
	account    string
	instanceID string
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
	ownerPath string
	// stopGrace defaults to vscodeStopGrace when unset. Tests shorten it when
	// modelling an editor that accepts signals but never confirms its exit.
	stopGrace time.Duration
	// reapErr is published by closing exited. Non-nil means group cleanup was not
	// confirmed, so the durable owner remains and teardown fails.
	reapErr error

	// ready latches once the editor has accepted a connection. Until then the
	// process is up but must not be proxied to, so callers see errVSCodeStarting
	// instead of a 502 from a socket that is not listening yet.
	ready bool

	// exited is closed by the reaping goroutine once the child has been reaped and
	// its process-group cleanup has been attempted, so alive can distinguish a live
	// leader without a syscall. A closed channel plus nil reapErr confirms the
	// group cleanup; a non-nil reapErr keeps the outcome explicitly unknown.
	exited chan struct{}

	// killGroup overrides the process-group signal syscall. Teardown's correctness
	// is about WHICH signals are sent and WHEN, which a test can only observe by
	// intercepting them; nil means the real syscall.
	//
	// Test seam, and it must be in place at CONSTRUCTION, never written after:
	// reap() runs on its own goroutine from the moment of the spawn and reads this
	// on its way out, so a later write is an unsynchronized write against an
	// already-running reader. A spawned server therefore inherits it from
	// vscodeSupervisor.killGroup, which startOne copies in before it starts the
	// reaper; only a test that builds a vscodeServer directly — no reaper, no
	// second goroutine — may set it inline.
	killGroup func(pgid int, sig syscall.Signal) error
	// groupAlive is the matching observation seam. It lets the reaper distinguish
	// successful signal delivery from confirmed process-group exit.
	groupAlive func(pgid int) (bool, error)
}

// signalGroup sends sig to the child's process GROUP, through the killGroup seam
// when one is set.
func (s *vscodeServer) signalGroup(pgid int, sig syscall.Signal) error {
	if s.killGroup != nil {
		return s.killGroup(pgid, sig)
	}
	return syscall.Kill(-pgid, sig)
}

// reap Waits the child, SIGKILLs its process GROUP, releases its socket, and
// closes exited — in that order, all four of which are load-bearing. Runs as one
// goroutine per spawn.
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
// close(exited) goes LAST, after both the kill and the socket release. stop()
// returns the moment exited closes and its callers act immediately —
// ArchiveSession MOVES the worktree. Closing first would let it move a directory
// the surviving children still hold open, and would let a caller observe teardown
// as complete while the editor's socket was still on disk.
//
// The socket release is here for the same reason the group kill is: this is the one
// place that always runs, exactly once, at the moment the process is really gone.
// See releaseSocket for why stop() is the wrong home for it (#1873).
func (s *vscodeServer) reap() {
	// Read the pgid before the Wait that frees it: after this returns, the leader's
	// pid is the kernel's to give away, and only a surviving group member keeps it
	// pinned long enough for the kill below to mean what it says.
	pgid := s.cmd.Process.Pid
	_ = s.cmd.Wait()
	killErr := s.signalGroup(pgid, syscall.SIGKILL)
	switch {
	case killErr != nil && !errors.Is(killErr, syscall.ESRCH):
		s.reapErr = fmt.Errorf("reaping VS Code editor process group %d for %s: %w", pgid, s.worktree, killErr)
		s.releaseSocketKeepingOwner()
	case errors.Is(killErr, syscall.ESRCH):
		// ESRCH is itself a confirmed-absent group at the safely pinned reap point.
		s.releaseSocket()
	default:
		grace := s.stopGrace
		if grace <= 0 {
			grace = vscodeStopGrace
		}
		gone, err := s.waitForProcessGroupExit(pgid, grace)
		if err != nil {
			s.reapErr = fmt.Errorf("confirming reaped VS Code editor process group %d for %s: %w", pgid, s.worktree, err)
			s.releaseSocketKeepingOwner()
		} else if !gone {
			s.reapErr = fmt.Errorf("VS Code editor process group %d for %s still exists after reaper SIGKILL", pgid, s.worktree)
			s.releaseSocketKeepingOwner()
		} else {
			s.releaseSocket()
		}
	}
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
func (s *vscodeServer) stop() error {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	grace := s.stopGrace
	if grace <= 0 {
		grace = vscodeStopGrace
	}
	pgid := s.cmd.Process.Pid
	// A reaped leader means the reaper has already made the last safely pinned group
	// kill attempt. The numeric pgid is no longer authority for another signal;
	// return that attempt's explicit result.
	if !s.alive() {
		return s.reapErr
	}
	// Alive ⇒ unreaped ⇒ the leader still holds the group id ⇒ -pgid is still ours.
	// (The alive()→signal gap is a microsecond TOCTOU inherent to POSIX group
	// signalling: there is no pgid handle to pin, and pidfd is per-process.)
	termErr := s.signalGroup(pgid, syscall.SIGTERM)
	select {
	case <-s.exited:
		// exited closes AFTER the reaper's group SIGKILL, so the group is clean and
		// there is nothing further to escalate to — unless that kill failed, in
		// which case reapErr preserves the unknown outcome.
		return s.reapErr
	case <-time.After(grace):
	}
	var killErr error
	if s.alive() {
		killErr = s.signalGroup(pgid, syscall.SIGKILL)
	}
	// Wait for the reap so teardown can't return while the group is still up (#816).
	// The reaper also unlinks the socket before closing exited (releaseSocket), so
	// a returning stop() leaves neither a live process nor a socket behind.
	select {
	case <-s.exited:
		return s.reapErr
	case <-time.After(grace):
	}
	if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		return fmt.Errorf("killing VS Code editor process group %d for %s: %w", pgid, s.worktree, killErr)
	}
	if termErr != nil && !errors.Is(termErr, syscall.ESRCH) {
		return fmt.Errorf("stopping VS Code editor process group %d for %s: %w", pgid, s.worktree, termErr)
	}
	return fmt.Errorf("VS Code editor process group %d for %s did not confirm exit after SIGKILL", pgid, s.worktree)
}
