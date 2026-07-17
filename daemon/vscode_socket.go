package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sockpath"
	"github.com/sachiniyer/agent-factory/log"
)

// The editor's TRANSPORT: how a daemon-spawned code-server / openvscode-server is
// named, secured, reached, and cleaned up (#1873).
//
// This is the whole of the unix-socket layer, split out of vscode_server.go — which
// owns the supervisor's LIFECYCLE (spawn, readiness, respawn, teardown) and had
// grown past the file-length limit. The seam is deliberate: everything here answers
// "where does the editor listen and who may reach it", and nothing here knows about
// process supervision.
//
// THE SOCKET IS THE AUTH. The editor runs with authentication disabled, so whatever
// can reach its listener has worktree read/write and terminal exec as the af user.
// A 0600 socket in a 0700 directory is what restricts that to the owning user —
// exactly the posture af's own control socket has, and the primitive config.toml
// holds up as the stronger one where it documents require_loopback_token.

// vscodeSocketDirName is the directory, under the af home, holding every editor
// socket. It is kept SHORT deliberately: every byte of it counts against the
// sockaddr_un path limit (see vscodeSocketPath).
const vscodeSocketDirName = "vscode"

// vscodeSocketExt is the socket file's extension. It is only a suffix, NOT the
// sweep's test: an extension is something any file can wear, so the sweep matches
// the full minted name and checks the file really is a socket
// (vscodeSocketNamePattern, isAbandonedVSCodeSocket).
const vscodeSocketExt = ".sock"

// vscodeSocketMode is the socket's file mode, passed to code-server's
// --socket-mode and applied by startOne for flavors that have no mode flag. 0600:
// the owning user only, matching the daemon's own control socket.
const vscodeSocketMode = "0600"

// The socket-path ceiling this file used to define now lives in
// internal/sockpath, which every socket in af shares. It was defined here, and
// enforced only here, which is how the daemon's OWN sockets ended up with no
// check at all: the editor pane failed with a message naming AGENT_FACTORY_HOME
// while the control plane failed with a bare kernel errno (#1940).
//
// sockpath.Max is also the platform's real limit rather than the portable 103
// this constant hardcoded, so on Linux the bound is now 107 — a loosening, which
// can only accept paths that previously worked.

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

// releaseSocket unlinks the editor's socket and drops any pooled connections to
// it. Called by reap, and ONLY by reap.
//
// It belongs here for the same reason the group kill does: this is the one place
// that observes the process actually dying, it runs exactly once per spawn, and
// it always runs. stop() looks like the natural home and is the wrong one — it
// returns early for a REAPED leader (nothing left to signal) and again the moment
// exited closes, which are the two ordinary ways an editor dies. An unlink there
// would be skipped for a dead editor and for one that honored SIGTERM, i.e. for
// almost every real teardown, and the 0700 directory would fill with one dead
// socket per session for the daemon's life. Nothing else prunes it: a SIGKILLed
// child never cleans up after itself, and the path carries a per-process nonce, so
// no later spawn will ever reuse — or unlink — this one.
//
// The nonce is also what makes unlinking here unconditionally safe: this path is
// THIS process's alone, so a teardown racing a respawn (ensureServer drops a dead
// editor with `go s.stop()` and immediately spawns its replacement) cannot delete
// the live editor's socket.
//
// Before close(exited), deliberately, mirroring the group kill's ordering: stop()
// returns the instant exited closes and its callers act at once, so a caller that
// sees teardown complete must not find the socket still on disk.
//
// Best-effort: a failed unlink costs litter, never correctness — a stale socket is
// inert, since nothing listens on it and a dial gets ECONNREFUSED — and the
// daemon's next start sweeps the directory anyway.
func (s *vscodeServer) releaseSocket() {
	if s.socketPath != "" {
		if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
			log.WarningLog.Printf("vscode: removing the editor socket %s failed: %v", s.socketPath, err)
		}
	}
	// The child is dead, so the kernel has closed these already; this releases the
	// descriptors now rather than leaving them for the transport to discover
	// lazily on a request that will never come.
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
}

// endpoint is how the proxy reaches this editor.
func (s *vscodeServer) endpoint() vscodeEndpoint {
	return vscodeEndpoint{SocketPath: s.socketPath, Transport: s.transport}
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
	// ABSOLUTE, always, and resolved HERE — before the path is handed to a child
	// that does not share our working directory.
	//
	// GetConfigDir returns AGENT_FACTORY_HOME as the operator wrote it, tilde
	// expanded but NOT absolutized, so a relative home ("af-home", "./state")
	// yields a relative socket path. The daemon would then dial it against the
	// DAEMON's cwd while the editor bound it against cmd.Dir — the session's
	// worktree — which is a different file, in a directory that does not exist
	// there. The child fails to bind and the pane never comes up. A port number
	// was immune to this, which is why it only appeared once the endpoint became a
	// path (#1873).
	//
	// Absolutizing at the boundary is the fix rather than at the config layer:
	// GetConfigDir feeds callers that resolve in-process, where a relative path
	// works fine, and changing it for everyone belongs in its own change.
	// The rule this file must keep is narrower — a path crossing into a child with
	// a different cwd must be absolute before it is written into argv.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving the af home %q to an absolute path failed: %w", dir, err)
	}
	sockDir := filepath.Join(abs, vscodeSocketDirName)
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return "", fmt.Errorf("creating the VS Code socket directory failed: %w", err)
	}
	if err := os.Chmod(sockDir, 0o700); err != nil {
		return "", fmt.Errorf("securing the VS Code socket directory failed: %w", err)
	}
	return sockDir, nil
}

// vscodeSocketNamePattern matches the socket names vscodeSocketPath mints, and
// ONLY those: two 8-character hex fields (the session hash and the process nonce)
// joined by a hyphen. It is anchored, so it cannot match a longer name that merely
// contains one.
//
// The sweep deletes files, so it must recognize its own work rather than trust the
// directory to hold nothing else. Keep this in lockstep with vscodeSocketPath — a
// name that stops matching stops being swept, and leaks instead.
var vscodeSocketNamePattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{8}\.sock$`)

// isAbandonedVSCodeSocket reports whether e is one of OUR editor sockets, left in
// dir by a previous daemon and safe to unlink.
//
// Two independent checks, because the sweep's only job is deletion and a blanket
// "*.sock" glob is the wrong instrument:
//
//   - the NAME must be one this daemon could have minted. The directory is af's,
//     but "af owns the directory" is an assumption about the world, not a fact the
//     sweep can verify — an operator can point AGENT_FACTORY_HOME anywhere,
//     including somewhere already in use, and a future af feature may put its own
//     sockets here. Matching our pattern means a foreign `agent.sock` survives.
//   - the ENTRY must actually be a socket. A regular file, a directory, or a
//     symlink can carry a .sock name; only a socket can be one of ours. Info() is
//     lstat semantics, so a symlink reports as a symlink and is skipped rather
//     than followed — the sweep can never delete through a link.
//
// A vanished entry (err from Info) is skipped: it is already gone.
func isAbandonedVSCodeSocket(dir string, e os.DirEntry) bool {
	if !vscodeSocketNamePattern.MatchString(e.Name()) {
		return false
	}
	info, err := e.Info()
	if err != nil {
		if !os.IsNotExist(err) {
			log.WarningLog.Printf("vscode: stat %s failed: %v", filepath.Join(dir, e.Name()), err)
		}
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

// vscodeSocketPath returns a FRESH socket path for key: a hash of the key, to
// identify the session, plus a random nonce, to identify the process.
//
// The name's SHAPE is load-bearing beyond readability: the startup sweep deletes
// by it (see vscodeSocketNamePattern), so any change here must change that too.
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
	if err := sockpath.Check("VS Code socket", path); err != nil {
		return "", err
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
			if !isAbandonedVSCodeSocket(dir, e) {
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
			// Wrapped, not bare: this is the errVSCodeStartExited case, and callers
			// (the proxy's notice page, the respawn cooldown) match the SENTINEL to
			// render a styled notice rather than surfacing a raw error.
			return fmt.Errorf("%w (check that it runs correctly: it was asked to serve %s)", errVSCodeStartExited, socketPath)
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
