package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
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
// THIS FILE owns the supervisor's LIFECYCLE: spawn, readiness, respawn, teardown.
// vscode_process.go owns the per-server process unit those steps act on — the
// binary, its argv, and the vscodeServer lifecycle. vscode_socket.go owns the
// transport — how the socket is named, secured, dialed, and swept. The one cross-file rule worth carrying in
// your head: socket names are per-PROCESS, not per session (vscodeSocketPath), so
// releasing a socket is always safe and never races the respawn.
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

// vscodeProbeInterval is how often ensureServer re-probes the socket while
// waiting for a starting editor to accept connections.
const vscodeProbeInterval = 100 * time.Millisecond

// vscodeStopGrace is how long a stopping editor gets to exit on SIGTERM before
// the process GROUP is SIGKILLed, mirroring the watcher supervisor's escalation.
const vscodeStopGrace = 5 * time.Second

// errVSCodeBinaryMissing reports that no editor binary could be found. It is a
// SENTINEL, not a failure: callers render the install hint for it and must not
// log it as an error — a machine without code-server installed is expected.
var errVSCodeBinaryMissing = errors.New("no VS Code server binary found")

// errVSCodeStarting reports that the editor is live but not yet accepting
// connections. Also a sentinel and also not a failure: callers render the
// self-refreshing "starting" notice, which resolves into the editor on its own.
var errVSCodeStarting = errors.New("the VS Code server is still starting")

// errVSCodeAccountScope reports that the session's selected account could not be
// turned into an environment for the editor — it is not registered, its
// directory is gone, or the boundary refused it.
//
// A sentinel because the pane must render it. It is the one spawn refusal whose
// cause is the OPERATOR's to fix (`af accounts add …`, or clear a cloud-mode
// selector), and the generic path answers a JSON error envelope that an iframe
// shows as unreadable text. Like errVSCodeStartExited it is recorded and
// replayed under the respawn cooldown, so its notice must NOT self-refresh.
var errVSCodeAccountScope = errors.New("the VS Code editor cannot be scoped to this session's account")

// errVSCodeStartExited reports an editor that exited without ever serving a
// request — a broken start rather than a crash to heal from. It is what the
// respawn cooldown replays, so a broken install cannot be respawned by every
// auto-refresh of the "starting" notice.
var errVSCodeStartExited = errors.New("the VS Code server exited before it finished starting")

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
// blocks on process start and a socket probe for seconds, and Manager.mu is
// deliberately never held across slow calls (see the pausedMu precedent).
type vscodeSupervisor struct {
	mu      sync.Mutex
	servers map[string]*vscodeServer
	// namespace selects the owner-record/socket directory this supervisor lives in
	// (vscodeNamespaceDirName). Empty is the daemon's; an agent-server sets its
	// own so the daemon's sweeps and its never see each other's editors.
	namespace string
	// reconciling reserves one session key while a prior daemon's persisted
	// owner is inspected and stopped outside mu. Same-key operations retry;
	// unrelated sessions never wait through process-group teardown.
	reconciling   map[string]struct{}
	reconcileCond *sync.Cond
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
	stopGrace        time.Duration
	cooldown         time.Duration
	now              func() time.Time
	groupAlive       func(int) (bool, error)

	// killGroup is the process-group signal seam (see vscodeServer.killGroup),
	// injected HERE rather than onto the server a spawn hands back because this is
	// the only place a test can install it safely: startOne copies it into the
	// server before starting the reaper goroutine that reads it, so the spawn
	// itself supplies the happens-before edge. Setting it on the returned server
	// would be a write racing a reader that is already running.
	killGroup func(pgid int, sig syscall.Signal) error
}

// vscodeFailure is a remembered spawn failure and when it happened.
type vscodeFailure struct {
	err error
	at  time.Time
	// instanceID scopes the cooldown to the session that earned it. The map is
	// keyed by daemonInstanceKey, which addresses a title slot rather than a
	// session, so without this a successor reusing that title inside the cooldown
	// gets its predecessor's spawn error replayed instead of a spawn attempt
	// (#2868/#2876, the same class one map over). Short-lived and self-healing —
	// unlike the restore and limit backoffs — but the guard costs one comparison,
	// and "the window is small" is how a class survives an audit.
	instanceID string
}

func newVSCodeSupervisor() *vscodeSupervisor {
	v := &vscodeSupervisor{
		servers:     make(map[string]*vscodeServer),
		reconciling: make(map[string]struct{}),
		failures:    make(map[string]vscodeFailure),
		configuredBinary: func() string {
			cfg, err := config.LoadConfig()
			if err != nil || cfg == nil {
				return ""
			}
			return cfg.VSCodeServerBinary
		},
		startGrace: vscodeStartGrace,
		stopGrace:  vscodeStopGrace,
		cooldown:   vscodeRespawnCooldown,
		now:        time.Now,
	}
	v.reconcileCond = sync.NewCond(&v.mu)
	return v
}

// ensureServer returns the ENDPOINT of the editor serving worktree for key,
// spawning it if it is not running and RESPAWNING it if it died — the self-heal
// that makes a crashed editor recover on the next render rather than needing the
// tab recreated. It returns errVSCodeBinaryMissing when no editor is installed,
// which callers render as the install hint.
func (v *vscodeSupervisor) ensureServer(key, worktree string) (vscodeEndpoint, error) {
	return v.ensureServerForInstance(key, key, worktree)
}

func (v *vscodeSupervisor) reserveReconcileLocked(key string) bool {
	if v.reconciling == nil {
		v.reconciling = make(map[string]struct{})
	}
	if _, busy := v.reconciling[key]; busy {
		return false
	}
	v.reconciling[key] = struct{}{}
	return true
}

func (v *vscodeSupervisor) releaseReconcile(key string) {
	v.mu.Lock()
	v.clearReconcileLocked(key)
	v.mu.Unlock()
}

func (v *vscodeSupervisor) clearReconcileLocked(key string) {
	if _, ok := v.reconciling[key]; !ok {
		return
	}
	delete(v.reconciling, key)
	v.reconcileCond.Broadcast()
}

// reconcilePersistedBeforeSpawn establishes the cross-daemon invariant before
// a new editor is started. The key reservation serializes same-session callers,
// while the slow process-table and signal waits deliberately run outside mu.
func (v *vscodeSupervisor) reconcilePersistedBeforeSpawn(key, instanceID string) error {
	v.mu.Lock()
	if v.stopped {
		v.mu.Unlock()
		return fmt.Errorf("daemon is shutting down")
	}
	if !v.reserveReconcileLocked(key) {
		v.mu.Unlock()
		return errVSCodeStarting
	}
	server := v.servers[key]
	hasLiveServer := server != nil && server.alive()
	if server != nil && !hasLiveServer {
		delete(v.servers, key)
		if !server.ready {
			// Attributed to the SERVER's stable id, never the caller's. The cached
			// entry sits under a title key, so in a reuse case it can belong to a
			// session the caller has nothing to do with — and this failure is the death
			// of THAT editor. Recording it against the caller would hand a fresh
			// session an error it did not earn on its very first request, which is the
			// bug this guard exists to prevent, re-entered through the write side.
			v.failures[key] = vscodeFailure{err: errVSCodeStartExited, at: v.now(), instanceID: server.instanceID}
		}
	}
	v.mu.Unlock()

	// Keep the reservation until admission: otherwise teardown can remove an
	// existing server in this unlock/relock gap and a replacement can escape it.
	if hasLiveServer {
		return nil
	}
	err := v.stopPersistedForInstance(key, instanceID)
	if err != nil {
		v.releaseReconcile(key)
		return fmt.Errorf("reconciling a previous VS Code editor: %w", err)
	}
	// ensureServer removes the reservation under mu and keeps mu through registration.
	return nil
}

// ensureServerForInstance is the ambient-identity form: no session in hand to
// read a selected account from, so the editor gets the daemon's own
// environment. Every production caller goes through the scoped form below; this
// is what the supervisor's own tests drive, and keeping it spares each of them
// from restating a scope they are not about.
func (v *vscodeSupervisor) ensureServerForInstance(key, instanceID, worktree string) (vscodeEndpoint, error) {
	return v.ensureServerForInstanceInScope(key, instanceID, worktree, ambientVSCodeScope())
}

// ensureServerForInstanceInScope is the production form: the stable instance id
// keeps a same-title recreation from adopting or stopping its predecessor's
// editor, and scope is the credential boundary the editor must run under.
func (v *vscodeSupervisor) ensureServerForInstanceInScope(
	key, instanceID, worktree string, scope vscodeAccountScope,
) (vscodeEndpoint, error) {
	if strings.TrimSpace(worktree) == "" {
		return vscodeEndpoint{}, fmt.Errorf("session has no worktree to open in VS Code")
	}
	if strings.TrimSpace(instanceID) == "" {
		return vscodeEndpoint{}, fmt.Errorf("session has no stable identity for VS Code")
	}
	if err := v.reconcilePersistedBeforeSpawn(key, instanceID); err != nil {
		return vscodeEndpoint{}, err
	}

	// Hold the supervisor lock for the whole call. It serializes concurrent
	// first-renders of one session onto ONE editor (the point of keying by
	// session), and it is the supervisor's own lock — never Manager.mu — so even
	// the blocking start grace stalls only other editor spawns, not unrelated RPCs.
	v.mu.Lock()
	v.clearReconcileLocked(key)
	defer v.mu.Unlock()
	if v.stopped {
		return vscodeEndpoint{}, fmt.Errorf("daemon is shutting down")
	}

	// Reuse a live editor, but only while it still serves THIS worktree UNDER THIS
	// ACCOUNT: a session restored to a different path (or a key reused after a
	// kill) must never be handed an editor rooted at the old directory, and a
	// session whose selected account has changed must never be handed an editor
	// still holding the previous identity's credential root. The account is part
	// of the reuse identity for the same reason the worktree is — it is baked into
	// the child's environ at exec and cannot be changed afterwards.
	if s := v.servers[key]; s != nil {
		alive := s.alive()
		switch {
		case alive && s.instanceID != instanceID:
			// The live editor is stable-ID proof that this key now belongs to a
			// replacement session. A stale caller must not unregister that editor;
			// its manager-side post-check will reject the old instance.
			return vscodeEndpoint{}, fmt.Errorf("VS Code editor key %q belongs to a different stable session instance", key)
		case alive && s.worktree == worktree && s.account == scope.account:
			if s.ready || v.probeReady(s) {
				return s.endpoint(), nil
			}
			// Up, but not listening yet — keep waiting, don't respawn.
			return vscodeEndpoint{}, errVSCodeStarting
		default:
			// Dead, serving a stale worktree, or scoped to a superseded account:
			// drop it and start clean. Stop it OUT OF BAND — stop() blocks for up to
			// the stop grace, and v.mu is held across the spawn below, so stopping
			// inline would stall every OTHER session's editor behind this one
			// teardown. It is already unregistered here, so nothing else can reach
			// it and no one needs to wait for it.
			// A DEAD leader needs nothing from stop() — its reaper already killed
			// the group at the only moment that was safe to — but stop() is correct
			// and immediate on one, so the branch stays uniform.
			neverReady := !s.ready && !alive
			delete(v.servers, key)
			go func() {
				if err := s.stop(); err != nil {
					log.WarningLog.Printf("vscode: stale editor teardown could not be confirmed: %v", err)
				}
			}()
			// An editor that died having NEVER become ready is a broken start, not a
			// crash to heal from — record it so the cooldown applies. Without this
			// only spawnLocked's own errors were recorded, so an editor that outlived
			// the start grace and THEN died would be respawned by every auto-refresh
			// of the "starting" notice: a spawn loop driven by our own UI. A server
			// that WAS ready and then died is the opposite case — a genuine crash —
			// and must still respawn at once, so it is deliberately not recorded.
			if neverReady {
				v.failures[key] = vscodeFailure{
					err:        errVSCodeStartExited,
					at:         v.now(),
					instanceID: s.instanceID,
				}
			}
		}
	}
	// Replay a recent failure instead of respawning, so the notice page's refresh
	// can't drive a spawn loop against a broken editor.
	if f, ok := v.failures[key]; ok && f.instanceID == instanceID && v.now().Sub(f.at) < v.cooldown {
		return vscodeEndpoint{}, f.err
	}

	binary, err := resolveVSCodeBinary(v.configuredBinary())
	if err != nil {
		// A missing binary is a stable, cheap-to-detect state, not a failed spawn:
		// recording it would make an install invisible for the cooldown.
		return vscodeEndpoint{}, err
	}

	server, err := v.spawnLocked(key, instanceID, binary, worktree, scope)
	if err != nil {
		v.failures[key] = vscodeFailure{err: err, at: v.now(), instanceID: instanceID}
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

// spawnLocked starts one editor on key's socket and waits for it to listen.
// Callers must hold v.mu.
//
// There is no retry loop. The TCP path needed one to re-roll a lost port race;
// a socket path is chosen by the daemon inside a directory only the daemon can
// write, so there is no race to lose and a failure here is a real failure (#1873).
func (v *vscodeSupervisor) spawnLocked(
	key, instanceID, binary, worktree string, scope vscodeAccountScope,
) (*vscodeServer, error) {
	// The credential boundary is resolved FIRST, before anything is swept, named
	// or exec'd. A failure REFUSES the editor rather than falling back to the
	// daemon's own environment: an editor is a place a human opens an integrated
	// terminal and runs the agent, so an unscoped one under a session that reports
	// a selected account is the silent wrong-account outcome #3051 exists to
	// prevent, reached through the one door #3051 did not close (#3870).
	//
	// It also stays OUT of the "starting <binary> failed" wrap below, deliberately.
	// This refusal is rendered into the pane, and what the operator has to fix is
	// the account, not the editor — a message that opens by blaming code-server
	// sends them to debug an install that is working.
	environ, err := scope.environment()
	if err != nil {
		return nil, err
	}
	// Before the first editor of this daemon's life, clear out any left by the
	// last one. Safe here precisely because nothing has spawned yet.
	v.sweepAbandonedSockets()
	socketPath, err := vscodeSocketPathIn(v.namespace, key)
	if err != nil {
		return nil, err
	}
	server, err := v.startOne(
		key, instanceID, binary, flavorForBinary(binary), socketPath, worktree, scope.account, environ)
	if err != nil {
		return nil, fmt.Errorf("starting %s failed: %w", filepath.Base(binary), err)
	}
	return server, nil
}

// startOne execs one editor and waits for its socket to accept connections.
// account and environ are the credential boundary spawnLocked resolved: the
// selected account's name, recorded so a later reuse can tell this editor apart
// from one the session has since moved off, and the exact environment to exec
// with.
func (v *vscodeSupervisor) startOne(
	key, instanceID, binary string, flavor vscodeFlavor, socketPath, worktree, account string, environ []string,
) (*vscodeServer, error) {
	// Check the worktree before exec'ing. os/exec reports a missing cmd.Dir as
	// ENOENT naming the BINARY ("fork/exec /usr/bin/code-server: no such file or
	// directory"), which sends the user off debugging a code-server install that
	// is perfectly fine. Name the directory that is actually missing instead.
	fi, err := os.Stat(worktree)
	if err != nil {
		return nil, fmt.Errorf("the session worktree %s cannot be accessed (has it been moved or removed?): %w", worktree, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("the session worktree %s is not a directory", worktree)
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
	processNonce, err := newVSCodeOwnerNonce()
	if err != nil {
		return nil, err
	}
	gatePath := vscodeStartGatePath(socketPath)
	if err := os.Remove(gatePath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clearing the stale VS Code startup gate %s failed: %w", gatePath, err)
	}
	cmd := newGatedVSCodeCommand(binary, vscodeArgs(flavor, socketPath, worktree), gatePath)
	cmd.Dir = worktree
	// Copied rather than appended in place: environ belongs to the caller's scope,
	// and append could write the nonce into its backing array.
	cmd.Env = append(append(make([]string, 0, len(environ)+1), environ...),
		vscodeOwnerNonceEnv+"="+processNonce)
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
		account:    account,
		instanceID: instanceID,
		socketPath: socketPath,
		transport:  newVSCodeTransport(socketPath),
		cmd:        cmd,
		stopGrace:  v.stopGrace,
		exited:     make(chan struct{}),
		// Copied in HERE, before the reaper below exists to read them — the seams
		// have to be installed by construction rather than later assignments.
		killGroup:  v.killGroup,
		groupAlive: v.groupAlive,
	}
	owner, err := captureVSCodeOwner(key, instanceID, cmd.Process.Pid, processNonce)
	if err == nil {
		err = writeVSCodeOwnerForStart(vscodeOwnerPath(socketPath), owner)
		if err == nil {
			server.ownerPath = vscodeOwnerPath(socketPath)
			err = releaseVSCodeStartGate(gatePath)
		}
	}
	if err != nil {
		// Identity is the only safe cross-restart handle. Refuse to run an editor
		// we could not durably identify; otherwise a daemon crash recreates #2668.
		// Use the ordinary reaper even though the harmless startup shell is still
		// gated, so a failed group cleanup remains an explicit unknown outcome.
		go server.reap()
		if stopErr := server.stop(); stopErr != nil {
			return nil, errors.Join(fmt.Errorf("recording editor ownership: %w", err), stopErr)
		}
		return nil, fmt.Errorf("recording editor ownership: %w", err)
	}
	// Reap the child AND clean up its process group; see reap's doc for why the
	// group kill belongs there rather than in stop().
	go server.reap()

	switch err := waitForSocket(socketPath, server.exited, v.startGrace); {
	case err == nil:
		server.ready = true
		// Force the mode now that the socket exists. Fatal here, unlike in
		// probeReady: this is the spawn path, nothing is serving yet, and refusing
		// to hand back an editor whose socket could not be secured is strictly
		// better than serving one that might be world-connectable.
		if err := secureVSCodeSocket(socketPath); err != nil {
			if stopErr := server.stop(); stopErr != nil {
				return nil, errors.Join(err, fmt.Errorf("stopping editor after socket security failure: %w", stopErr))
			}
			return nil, err
		}
		return server, nil
	case errors.Is(err, errVSCodeStarting):
		// Still coming up. Hand the caller a live, not-ready server rather than
		// killing it — a cold code-server legitimately exceeds the grace, and
		// killing it here would restart the clock forever and never start.
		return server, nil
	default:
		// The child exited (bad binary, bad worktree) or the socket could not be
		// probed: never leave it running. Its socket carries a nonce, so a later
		// spawn would not adopt it — but the PROCESS would linger, holding the
		// worktree open and belonging to nothing.
		if stopErr := server.stop(); stopErr != nil {
			return nil, errors.Join(err, fmt.Errorf("stopping editor after startup failure: %w", stopErr))
		}
		return nil, err
	}
}
