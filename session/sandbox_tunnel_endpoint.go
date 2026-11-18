package session

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/peercred"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/sockpath"
	"github.com/sachiniyer/agent-factory/log"
)

// The local end of a sandbox tunnel is a unix socket, never a TCP port (#7495).
//
// A `ssh -L <port>:<remote>` forward has no "pick a port and tell me" form, so
// af used to reserve one by binding :0 and closing it — a gap in which ANY
// local process could take the port. af's first request through the tunnel
// carries the session's bearer credential, and nothing distinguished the ssh
// child's listener from a stranger's: the readiness probe connected to
// whoever won the port, and the credential went with it.
//
// A unix socket cannot be taken that way. ssh -L <socket>:<remote> binds a
// path, not a port, and the path lives in a directory no other uid can enter:
//
//	<af-home>/tunnels/             owned by this uid, mode 0700, never a
//	                               symlink — verified on the OPENED directory,
//	                               not a re-read path
//	  d<pid>-<rand>/               one directory per daemon generation
//	    .lock                      flock'd for the daemon's whole life; a lock
//	                               that can be taken proves the owner exited,
//	                               so a sweep can remove the dir without ever
//	                               touching a live daemon's — or a live
//	                               predecessor's during an upgrade hand-off
//	    s<N>-<slug>.sock           one name per tunnel, unique within the dir
//
// Three properties follow:
//
//   - Another local account can never place a listener where af will dial:
//     they cannot enter tunnels/ at all.
//   - A same-uid squatter cannot hold the endpoint either: ssh runs with
//     -o StreamLocalBindUnlink=yes, which unlinks whatever is at the path
//     before binding — and the path can only ever be inside THIS daemon's
//     own generation dir, because af generates it there.
//   - Every connection is proven before a byte is written: the kernel names
//     the acceptor's pid (SO_PEERCRED/LOCAL_PEERPID), and af refuses any
//     acceptor that is not — or does not descend from — the tunnel's own
//     spawn (verifyTunnelPeer). No credential is sent to an endpoint that is
//     not provably af's own ssh child.
//
// The directory is verified on the opened object, and that fd is held for
// the process's life: everything af creates under tunnels/ — generation
// dirs, their .lock files — is created relative to it (mkdirat/openat, then
// fstat), never by re-resolving the path (#7503). A same-uid process that
// swaps tunnels/ afterwards cannot redirect af's files. A generation dir
// deleted mid-life is re-established under the held fd rather than served
// from a dead cache entry; and when tunnels/ ITSELF is unlinked the held fd
// goes dead (nlink 0), so the next spawn re-verifies the path end-to-end
// before anything lands in it — a foreign tunnels/ swapped in for the old
// one is refused by the same checks as the first verify (#7511).
//
// There is deliberately no TCP fallback: a transport that cannot do a unix
// forward fails the provision loudly, naming the transport, rather than
// silently re-opening the race this exists to close.

const (
	// tunnelSocketRootName is the AF-home subdirectory every tunnel's local
	// endpoint lives under.
	tunnelSocketRootName = "tunnels"
	// tunnelGenLockName is the file each generation dir holds an exclusive
	// flock on for its owner's life. The lock is the sweep's proof of death:
	// it dies with the process, which pid reuse cannot fake.
	tunnelGenLockName = ".lock"
	// tunnelGenDirPattern is the only name shape af itself creates inside
	// tunnels/ — the sweep refuses to touch anything else.
	tunnelGenDirPattern = `^d[0-9]+-[0-9a-f]{6}$`
)

var tunnelGenDirRE = regexp.MustCompile(tunnelGenDirPattern)

// tunnelSocketSeq orders socket names within this generation so two tunnels
// (and two dials of the same reattach) never share a path.
var tunnelSocketSeq atomic.Uint64

// tunnelSocketRoot is the verified <home>/tunnels directory held OPEN for the
// process's life. Everything af creates under it — generation dirs, their
// .lock files — is created RELATIVE TO THE FD (mkdirat/openat, then fstat),
// never by re-resolving the path: a swap of the directory after verification
// cannot redirect af's writes into whatever replaced it (#7503). The path is
// kept only to mint the socket paths ssh's -L binds and errors name.
type tunnelSocketRoot struct {
	path string
	// dir is the verified directory fd (O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC) —
	// the boundary every create under tunnels/ is relative to.
	dir *os.File
}

// tunnelGenDirEntry pins one generation dir plus its held .lock per canonical
// AF home. Production resolves one home per process; a test binary can run
// several homes through the same code — keying the dir on the CURRENT home
// (read fresh, not the once-fixed env-marker home) keeps a later home from
// inheriting a dir under an earlier, deleted one.
type tunnelGenDirEntry struct {
	once sync.Once
	mu   sync.Mutex // serializes establish/ensure between concurrent provisions
	root *tunnelSocketRoot
	name string // the generation dir's basename — re-established under root if it vanishes
	dir  string
	err  error
	// lock keeps the generation's .lock fd open for the process's life so the
	// flock dies with it (and never survives into a child: the fd is O_CLOEXEC
	// and is never handed out via ExtraFiles).
	lock *os.File
}

var tunnelGenDirs sync.Map // canonical AF home -> *tunnelGenDirEntry

// tunnelGenerationDir resolves — creating on first use — this daemon's own
// generation dir under the CURRENT home's verified tunnels root. A var so a
// test can substitute a temp dir.
var tunnelGenerationDir = defaultTunnelGenerationDir

func defaultTunnelGenerationDir() (string, error) {
	home, err := currentTunnelHome()
	if err != nil {
		return "", err
	}
	ent, _ := tunnelGenDirs.LoadOrStore(home, &tunnelGenDirEntry{})
	e := ent.(*tunnelGenDirEntry)
	e.once.Do(func() {
		e.root, e.err = verifyTunnelSocketRoot(home)
		if e.err != nil {
			return
		}
		e.mu.Lock()
		e.err = e.establishLocked()
		e.mu.Unlock()
	})
	if e.err != nil {
		return "", e.err
	}
	// Every caller gets a LIVE generation dir, not the cached name: a dir
	// deleted while the daemon runs is re-established under the held root fd
	// rather than served from a stale cache entry (#7503), and a root that
	// was unlinked entirely is re-verified so its re-establish can land
	// (#7511). Cheap when nothing is wrong — one fstat + one fstatat per call.
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureRootLocked(home); err != nil {
		return "", err
	}
	if err := e.ensureLocked(); err != nil {
		return "", err
	}
	return e.dir, nil
}

// ensureRootLocked re-verifies the held root when it has been unlinked — a
// `rm -rf tunnels/` deletes the very directory the held fd names, and
// nothing can be created under an unlinked inode, so the generation's
// re-establish would fail "no such file or directory" until restart (#7511).
// The re-verify re-opens the path and re-proves tunnels/ (creating it 0700
// when nothing is there): a swapped-in foreign tunnels/ is refused by the
// same checks as first verify. The generation's name is kept, so socket
// paths minted against it stay valid across the recovery. Callers hold e.mu.
func (e *tunnelGenDirEntry) ensureRootLocked(home string) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(e.root.dir.Fd()), &st); err == nil && st.Nlink != 0 {
		return nil
	}
	// nlink == 0 (or an fstat failure): the held fd names a dead inode.
	// Re-verify rather than fail — the dead fd is released only after a
	// replacement verifies, so a failed verify never worsens the state.
	root, err := verifyTunnelSocketRoot(home)
	if err != nil {
		return err
	}
	_ = e.root.dir.Close()
	e.root = root
	return nil
}

// currentTunnelHome resolves this process's canonical AF home FRESH on every
// call — unlike sandboxTunnelHome, which is process-fixed because the env
// marker it stamps must be. The socket root follows the home the caller is
// configured for right now; in production the two agree (one daemon, one
// home), and the daemon's startup sweep and provision path always name the
// same tunnels/.
func currentTunnelHome() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return pathutil.ResolveForCompare(abs), nil
}

// verifyTunnelSocketRoot returns <home>/tunnels held open, after proving —
// on the OPENED object, never a re-read path — that tunnels/ is a real
// directory owned by this uid with mode 0700. The held fd is the one boundary
// later creates are relative to: checking the opened fd is what keeps a swap
// between stat and use from slipping a foreign directory past the check
// (#7495), and holding it is what keeps a swap AFTER the check from
// redirecting af's own files (#7503). Only tunnels/ must be private — it is
// the part af owns; the home itself is verified to open and read as a
// directory, never created here (the AF home latch is #3845/#3850's domain)
// and never mode-checked (the peer and socket proofs already refuse a foreign
// acceptor, so home writability is at most a doctor warning). The caller owns
// the fd: generation-dir users hold it for the process's life; the startup
// sweep closes it when done.
func verifyTunnelSocketRoot(home string) (*tunnelSocketRoot, error) {
	homeFd, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open the AF home directory %q — af must verify it before trusting "+
			"the tunnel directory inside: %w. If this path is wrong, point AGENT_FACTORY_HOME at af's "+
			"configuration directory", home, err)
	}
	defer func() { _ = unix.Close(homeFd) }()
	var hst unix.Stat_t
	if err := unix.Fstat(homeFd, &hst); err != nil {
		return nil, fmt.Errorf("cannot stat the AF home directory %q: %w", home, err)
	}
	if hst.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("the AF home %q is not a directory: point AGENT_FACTORY_HOME at a "+
			"directory af can keep private", home)
	}

	rootPath := filepath.Join(home, tunnelSocketRootName)
	created := false
	if err := unix.Mkdirat(homeFd, tunnelSocketRootName, 0o700); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("cannot create the tunnel socket directory %q: %w", rootPath, err)
		}
	} else {
		created = true
	}
	// O_NOFOLLOW + O_DIRECTORY refuse a symlink or a non-directory at the open
	// itself; Fstat then describes the very object the socket paths will be
	// created under — and stays open, held, for every later create.
	fd, err := unix.Openat(homeFd, tunnelSocketRootName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.EACCES):
			// A foreign-owned 0700 dir fails the open itself — the owner
			// check below never gets to speak. Name the requirement and
			// the fix rather than reporting a bare EACCES (#7495).
			return nil, fmt.Errorf("cannot enter the tunnel socket directory %q — it is owned by another "+
				"account, and af requires it be owned by this user (uid %d) because af's credential-bearing "+
				"tunnel endpoints live there: chown it to this user, or remove it and let af create it",
				rootPath, os.Geteuid())
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
			// O_NOFOLLOW reports a symlink ELOOP on some kernels and ENOTDIR
			// (with O_DIRECTORY) on others — refuse both spellings.
			return nil, fmt.Errorf("the tunnel socket directory %q is a symlink or not a directory; af requires a "+
				"real directory it can verify as private — remove it and let af create it: rm %s", rootPath, rootPath)
		default:
			return nil, fmt.Errorf("cannot open the tunnel socket directory %q: %w", rootPath, err)
		}
	}
	dir := os.NewFile(uintptr(fd), rootPath)
	var st unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &st); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("cannot stat the opened tunnel socket directory %q: %w", rootPath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = dir.Close()
		return nil, fmt.Errorf("the tunnel socket directory %q is not a directory — remove it and let af create it: rm %s", rootPath, rootPath)
	}
	if st.Uid != uint32(os.Geteuid()) {
		_ = dir.Close()
		return nil, fmt.Errorf("the tunnel socket directory %q is owned by uid %d, not this user (uid %d): "+
			"af's credential-bearing tunnel endpoints live there, so af refuses to use it — chown it to this user or remove it and let af create it",
			rootPath, st.Uid, os.Geteuid())
	}
	if st.Mode&0o777 != 0o700 {
		if !created {
			_ = dir.Close()
			return nil, fmt.Errorf("the tunnel socket directory %q has mode %04o; af requires 0700 so no other local "+
				"account can reach the tunnel endpoints it holds — fix it with: chmod 0700 %s",
				rootPath, st.Mode&0o777, rootPath)
		}
		// af created it just now under a umask that stripped bits; normalize
		// through the opened fd — a path-based chmod could follow a swapped-in
		// link.
		if err := unix.Fchmod(int(dir.Fd()), 0o700); err != nil {
			_ = dir.Close()
			return nil, fmt.Errorf("cannot set mode 0700 on the tunnel socket directory %q: %w", rootPath, err)
		}
	}
	return &tunnelSocketRoot{path: rootPath, dir: dir}, nil
}

// establishLocked creates this daemon's generation dir under the verified
// root fd and takes its .lock — exclusive, held for the process's life on the
// entry's fd, O_CLOEXEC so no spawned child can hold it open past daemon
// death. The lock is the sweep's hand-off safety: while ANY af daemon — this
// one, or a predecessor mid-upgrade — is alive, its generation dir cannot be
// swept. Callers hold e.mu.
func (e *tunnelGenDirEntry) establishLocked() error {
	for attempt := 0; attempt < 4; attempt++ {
		e.name = fmt.Sprintf("d%d-%06x", os.Getpid(), rand.Uint32()&0xffffff)
		if err := unix.Mkdirat(int(e.root.dir.Fd()), e.name, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return fmt.Errorf("cannot create the tunnel generation directory under %q: %w", e.root.path, err)
		}
		e.dir = filepath.Join(e.root.path, e.name)
		if err := e.takeLockLocked(); err != nil {
			_ = unix.Unlinkat(int(e.root.dir.Fd()), e.name, unix.AT_REMOVEDIR)
			return err
		}
		return nil
	}
	return fmt.Errorf("cannot mint a unique tunnel generation directory under %q", e.root.path)
}

// ensureLocked guarantees the entry's generation dir exists and is af's own —
// re-establishing it (same name, fresh .lock) when it has gone missing
// (#7503). A directory that exists but no longer reads as this daemon's own
// is refused rather than adopted. Callers hold e.mu.
func (e *tunnelGenDirEntry) ensureLocked() error {
	var st unix.Stat_t
	err := unix.Fstatat(int(e.root.dir.Fd()), e.name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Uid == uint32(os.Geteuid()) && st.Mode&0o777 == 0o700:
		return nil // alive and provably ours — nothing to do
	case err == nil:
		return fmt.Errorf("the tunnel generation directory %q was replaced by something that is not "+
			"af's own 0700 dir — refusing to use it; remove %s and let af re-establish it",
			e.dir, e.dir)
	case errors.Is(err, unix.ENOENT):
		break // deleted mid-life: re-establish it below
	default:
		return fmt.Errorf("cannot stat the tunnel generation directory %q: %w", e.dir, err)
	}
	if err := unix.Mkdirat(int(e.root.dir.Fd()), e.name, 0o700); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("cannot re-create the tunnel generation directory %q: %w", e.dir, err)
		}
		// A racing recreate (or a foreign plant) won the name: verify it is a
		// dir af can own — the takeLock fstat refuses anything else.
	}
	return e.takeLockLocked()
}

// takeLockLocked opens the generation dir through the root fd, re-proves it
// (a directory, this uid, 0700), creates its .lock relative to the dir fd,
// and holds the flock for the process's life. The generation's previous lock
// fd — guarding an inode a deleted dir abandoned — is released. Callers hold
// e.mu.
func (e *tunnelGenDirEntry) takeLockLocked() error {
	genFd, err := unix.Openat(int(e.root.dir.Fd()), e.name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("cannot open the tunnel generation directory %q: %w", e.dir, err)
	}
	defer func() { _ = unix.Close(genFd) }()
	var st unix.Stat_t
	if err := unix.Fstat(genFd, &st); err != nil {
		return fmt.Errorf("cannot stat the opened tunnel generation directory %q: %w", e.dir, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(os.Geteuid()) || st.Mode&0o777 != 0o700 {
		return fmt.Errorf("the tunnel generation directory %q is not a private directory owned by this "+
			"user — refusing to use it; remove it and let af re-establish it: rm -rf %s", e.dir, e.dir)
	}
	lockFd, err := unix.Openat(genFd, tunnelGenLockName,
		unix.O_RDWR|unix.O_CREAT|unix.O_TRUNC|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("cannot create the tunnel generation lock in %q: %w", e.dir, err)
	}
	lock := os.NewFile(uintptr(lockFd), filepath.Join(e.dir, tunnelGenLockName))
	// LOCK_EX|LOCK_NB cannot fail against a just-established dir in practice;
	// if it ever did, a foreign holder is exactly what the lock exists to
	// detect — treat the dir as unusable rather than shared.
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return fmt.Errorf("cannot lock the tunnel generation directory %q: %w", e.dir, err)
	}
	// Prove the lock af now holds IS the .lock this dir names: fstat the held
	// fd and fstatat the name under the dir fd, and require the same
	// (dev, inode). A replacement dir whose lock file af did not create is
	// refused rather than adopted, and a .lock swapped between openat and
	// here can never be what guards the generation (#7511).
	if err := heldLockIsTheNamedFile(genFd, lock); err != nil {
		_ = lock.Close()
		return fmt.Errorf("the tunnel generation lock in %q is unprovable: %w", e.dir, err)
	}
	if e.lock != nil {
		_ = e.lock.Close()
	}
	e.lock = lock
	return nil
}

// heldLockIsTheNamedFile proves the open lock fd IS the file dirfd names
// .lock — identical (dev, inode) on both sides (#7511). The sweep and the
// generation's own liveness read the PATH's .lock, so a held lock that is
// some other inode would guard nothing the dir can name: the dir could be
// swept while af believed it locked, or a foreign lock adopted as af's own.
// Both sides are queried on opened objects (fstat / fstatat under the dir
// fd), never a re-resolved path.
func heldLockIsTheNamedFile(dirfd int, lock *os.File) error {
	var lst, dst unix.Stat_t
	if err := unix.Fstat(int(lock.Fd()), &lst); err != nil {
		return fmt.Errorf("cannot stat the held lock: %w", err)
	}
	if err := unix.Fstatat(dirfd, tunnelGenLockName, &dst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("cannot stat %q inside the generation directory: %w", tunnelGenLockName, err)
	}
	if lst.Dev != dst.Dev || lst.Ino != dst.Ino {
		return fmt.Errorf("the held lock is not the %q the directory names — the entry was replaced "+
			"under the proof; refusing to guard a directory af cannot name", tunnelGenLockName)
	}
	return nil
}

// newTunnelSocketName mints a socket name unique within this generation dir
// and short enough to leave dir + name inside sun_path. The sequence is what
// makes the name unique; the session's slug rides along only for readability
// (`ls` answers "which session is this") and only as far as the remaining
// budget allows — trimmed, then dropped — so a deep but valid AF home still
// gets a socket (#7495). When even the minimal s<N>.sock does not fit the
// name comes back anyway and the caller's sockpath.Check refuses it, naming
// AGENT_FACTORY_HOME as the knob.
func newTunnelSocketName(title string, budget int) string {
	seq := tunnelSocketSeq.Add(1)
	slug := ""
	if s := Slugify(title); s != "" {
		// "s<N>-<slug>.sock": the slug's room is whatever the minimal name
		// leaves behind it and its separator — at most the usual cap.
		room := budget - len(fmt.Sprintf("s%d-.sock", seq))
		if room > 24 {
			room = 24
		}
		if room > 0 {
			if len(s) > room {
				s = strings.TrimRight(s[:room], "-")
			}
			slug = s
		}
	}
	if slug == "" {
		return fmt.Sprintf("s%d.sock", seq)
	}
	return fmt.Sprintf("s%d-%s.sock", seq, slug)
}

// proctreeLookup is the one seam the peer proof reads the process table
// through — a var so a test can answer for pids no kernel will attest.
var proctreeLookup = proctree.Lookup

// tunnelLeaderRef is the lock-free snapshot of one tunnel generation's spawn
// the peer proof reads on EVERY fresh dial (#7495): the spawned child's pid —
// the group/session leader supervision kills by — plus the proctree start
// stamp captured at publish, so the proof can distinguish this generation's
// leader from any stranger that later reuses the pid. Written only through
// setTunnel; read with no lock a caller can hold — account swaps hold reapMu
// across the very REST calls that dial through this proof, and a dial that
// needed the same mutex would stall to its deadline with the caller parked
// on it.
type tunnelLeaderRef struct {
	pid     int
	startID uint64 // proctree identity stamp; 0 = unreadable at publish, and UNPROVABLE — every dial refuses (#7503)
}

// setTunnel assigns this provisioner's current tunnel generation AND the
// lock-free leader snapshot the peer proof reads on every dial — the two must
// move together, so every p.tunnel write goes through here. Callers hold
// reapMu (or own the provisioner outright, as tests do).
func (p *sandboxProvisioner) setTunnel(t *tunnelChild) {
	p.tunnel = t
	p.publishTunnelLeader(t)
}

// publishTunnelLeader refreshes the atomic snapshot verifyTunnelPeer reads:
// the spawned child's pid and its start stamp, so a dead leader — or a
// reused pid — can never adopt the acceptor's ancestry. A stamp the process
// table cannot be read for is published as 0, which the proof refuses
// outright (#7503): a leader af cannot distinguish from a pid reuse is no
// anchor at all, and a dial must fail closed until a STAMPED leader is
// published — never pass on liveness alone.
func (p *sandboxProvisioner) publishTunnelLeader(t *tunnelChild) {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		p.tunnelLeader.Store(nil)
		return
	}
	ref := &tunnelLeaderRef{pid: t.cmd.Process.Pid}
	if pr, err := proctreeLookup(ref.pid); err == nil {
		ref.startID = pr.StartID
	}
	p.tunnelLeader.Store(ref)
}

// verifyTunnelPeer is the endpoint's per-connection proof (#7495): the kernel
// names a pid for the unix-socket connection's peer, and af writes nothing
// until that pid is shown to be — or to descend from — THIS tunnel
// generation's own spawn. Descent is the rule rather than group membership
// because a documented sandbox.ssh wrapper (sshpass, `setsid`, `script -c`)
// puts its ssh in a fresh process group or session: the ssh is still provably
// af's own spawn, and a group-equality check would refuse it.
//
// The honest shape of the proof (#7503, #7511): only the leader carries an
// af-minted identity — the (pid, startID) pair stamped at publish. The
// acceptor pid the kernel reports is the listen()-time pid: SO_PEERCRED can
// still name it after the real acceptor exits, so it is a hint about who the
// peer WAS, never proof the named process is still alive — that is why the
// anchor is the stamped (pid, startID) pair rather than the pid alone. A
// leader that exited is refused by the liveness check; a reused pid is
// refused by the stamp's equality. Every intermediate link is a bare pid
// read from the process table, and the only evidence a link carries is the
// PPID re-read after the walk — a chain that moved mid-check is caught
// rather than adopted. StartID itself is used for EQUALITY ONLY, never
// arithmetic: proctree documents it as an identity stamp, and on darwin it
// is wall-clock p_starttime — ordering on it could refuse af's own transport
// after a backwards clock step (#7511). The walk ends at init — an acceptor
// reparented off the spawn's chain cannot prove the spawn started it, and
// is refused.
//
// That is same-uid defence in depth, deliberately: the verified 0700
// directory keeps other accounts from placing a listener at all, and the
// peer proof keeps a same-uid process from answering in the forward's place.
// A foreign listener can forge neither the pid the kernel reports nor a
// stable ancestry; a refusal is a tunnel failure, never a request sent to
// the wrong endpoint.
//
// The leader comes from an atomic snapshot — never from a lock a caller can
// hold (see tunnelLeaderRef).
func (p *sandboxProvisioner) verifyTunnelPeer(conn *net.UnixConn) error {
	pid, err := peercred.ConnPID(conn)
	if err != nil {
		return fmt.Errorf("the kernel cannot name the socket's acceptor: %w", err)
	}
	leader := p.tunnelLeader.Load()
	if leader == nil {
		return errors.New("the tunnel's ssh child is not running")
	}
	lp, err := proctreeLookup(leader.pid)
	if err != nil {
		return fmt.Errorf("the tunnel's ssh child (pid %d) is not running: %w", leader.pid, err)
	}
	if leader.startID == 0 {
		// The leader was published without a readable start stamp: nothing
		// distinguishes it from a stranger reusing the pid, so liveness alone
		// must never carry the proof (#7503). Refuse every dial until a
		// stamped leader is published.
		return fmt.Errorf("the tunnel's ssh child (pid %d) has no readable start stamp — af refuses to "+
			"anchor its proof on a process it cannot distinguish from a reused pid", leader.pid)
	}
	if lp.StartID != leader.startID {
		return fmt.Errorf("the tunnel's ssh child (pid %d) was replaced by another process — "+
			"a reused pid cannot anchor af's proof of its own transport", leader.pid)
	}
	var chain []proctree.Process
	for cur, depth := pid, 0; depth < 64 && cur > 1; depth++ {
		if cur == leader.pid {
			if len(chain) == 0 {
				// Depth 0: the kernel's pid IS the published leader, and the
				// stamp check above already proved that pid is the SAME live
				// process af spawned — the strongest admission the proof
				// makes. Honest limitation (#7511): the kernel's pid is the
				// listen()-time pid, so a leader that exited while its socket
				// fd survived in a descendant still reports it — and is
				// REFUSED by the liveness check above, never trusted on the
				// stale pid. There is no ancestry walk to corroborate at
				// depth 0; the stamped (pid, StartID) equality is the proof.
				return nil
			}
			// Every link's PPID is re-read: a chain that moved while we
			// walked it is no proof of anything (#7503).
			for _, rec := range chain {
				now, rerr := proctreeLookup(rec.PID)
				if rerr != nil {
					return fmt.Errorf("the acceptor's ancestry (pid %d) vanished mid-check: %w", rec.PID, rerr)
				}
				if now.PPID != rec.PPID {
					return fmt.Errorf("the acceptor's ancestry (pid %d) reparented mid-check — "+
						"refusing a chain af cannot prove is stable", rec.PID)
				}
			}
			return nil
		}
		pr, err := proctreeLookup(cur)
		if err != nil {
			return fmt.Errorf("cannot trace the acceptor's ancestry (pid %d): %w", cur, err)
		}
		chain = append(chain, pr)
		if pr.PPID <= 1 || pr.PPID == cur {
			break
		}
		cur = pr.PPID
	}
	return fmt.Errorf("the tunnel endpoint's acceptor (pid %d) does not descend from this tunnel's "+
		"ssh child (pid %d) — refusing to use a listener that is not provably af's own ssh child",
		pid, leader.pid)
}

// verifyTunnelSocketFile lstat-proves the socket ssh just bound — the thing
// itself, not the option af asked for. StreamLocalBindMask=0177 states the
// intent (0600), but that keyword has no first-obtained-wins guard: an
// ssh_config value is read after the command line and can still strip the
// mode. So the probe proves the filesystem's answer: a socket, owned by this
// uid, with no group or other bits. Inside the two verified 0700 directories
// a stripped mode would still be unreachable to another account — the check
// is defense in depth that fails loudly rather than quietly relaxing.
func verifyTunnelSocketFile(sockPath string) error {
	var st unix.Stat_t
	if err := unix.Lstat(sockPath, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("the tunnel endpoint %q is not a socket", sockPath)
	}
	if st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("the tunnel endpoint %q is owned by uid %d, not this user (uid %d)",
			sockPath, st.Uid, os.Geteuid())
	}
	if perm := st.Mode & 0o777; perm&0o077 != 0 {
		return fmt.Errorf("the tunnel endpoint %q has mode %04o reachable by group or other — af requires "+
			"the bound socket be private (0600). af passes -o StreamLocalBindMask=0177, but an ssh_config "+
			"StreamLocalBindMask overrides the command line: scope that option out of the transport's "+
			"config file, or remove it", sockPath, perm)
	}
	return nil
}

// tunnelSocketPath mints the path this provision's -L will bind: inside this
// daemon's own generation dir — the ONLY place StreamLocalBindUnlink can ever
// unlink — with a name unique within it.
func (p *sandboxProvisioner) tunnelSocketPath() (string, error) {
	dir, err := tunnelGenerationDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, newTunnelSocketName(p.spec.Title, sockpath.Max-len(dir)-1))
	if err := sockpath.Check("sandbox tunnel endpoint", path); err != nil {
		return "", err
	}
	// The -L spec quotes the path inside [brackets] — OpenSSH's escape, read
	// since before the 7.6 floor — which expresses every byte but a literal
	// ']'. A ':' home needs the escape; a ']' home cannot be expressed at
	// all, and is refused with the same remedy.
	if strings.Contains(path, "]") {
		return "", fmt.Errorf("the tunnel socket path %q contains ']', which af's bracket-quoted "+
			"ssh -L spec cannot express: set AGENT_FACTORY_HOME to a directory without ']'", path)
	}
	return path, nil
}

// removeTunnelSocketLocked unlinks this provision's socket file on teardown —
// hygiene only; the whole generation dir is swept once the daemon exits.
// Callers hold reapMu; the path is this generation's own, so the unlink can
// never reach a socket another daemon serves.
func (p *sandboxProvisioner) removeTunnelSocketLocked() {
	if p.socketPath != "" {
		_ = os.Remove(p.socketPath)
	}
}

// sweepStaleTunnelSocketDirs removes generation dirs under root whose owning
// daemon is provably dead, and nothing else (#7495). Every read and every
// removal is relative to the VERIFIED root fd (#7503) — a swapped tunnels/
// between verify and sweep can never turn the removals loose on a directory
// af did not prove.
//
//   - a dir whose .lock can be FLOCK'd has no live owner — flock dies with the
//     process that holds it, which pid reuse cannot fake. Its sockets are
//     dead-path files; the dir's entries are unlinked and it is removed.
//   - a dir whose .lock is HELD belongs to a live af process — including a
//     predecessor daemon still serving sessions during an upgrade hand-off —
//     and is never touched.
//   - a dir with no .lock at all is removed only when empty (a generation
//     that died between mkdir and its lock; it can hold no socket yet). A
//     non-empty lockless dir is unproven and stays.
//   - a dir containing a SUBDIRECTORY is more than af ever creates — unproven,
//     it stays.
//   - anything not named like a generation dir is not af's to classify and
//     stays.
func sweepStaleTunnelSocketDirs(root *tunnelSocketRoot) (swept int, err error) {
	rootFd := int(root.dir.Fd())
	names, err := root.dir.Readdirnames(-1)
	if err != nil {
		return 0, err
	}
	for _, name := range names {
		if !tunnelGenDirRE.MatchString(name) {
			continue
		}
		genFd, gerr := unix.Openat(rootFd, name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if gerr != nil {
			continue // not a dir af can classify — a symlink or a file stays
		}
		gen := os.NewFile(uintptr(genFd), name)
		inner, derr := gen.Readdirnames(-1)
		if derr != nil {
			_ = gen.Close()
			continue
		}
		lockFd, lerr := unix.Openat(genFd, tunnelGenLockName,
			unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		switch {
		case errors.Is(lerr, unix.ENOENT):
			// No lock file: only an EMPTY dir is provably a dead setup —
			// nothing can be served through it.
			if len(inner) == 0 {
				if unix.Unlinkat(rootFd, name, unix.AT_REMOVEDIR) == nil {
					swept++
				}
			}
			_ = gen.Close()
		case lerr != nil:
			_ = gen.Close()
		default:
			if ferr := unix.Flock(lockFd, unix.LOCK_EX|unix.LOCK_NB); ferr != nil {
				// Held by a live process — this generation, or a
				// predecessor's.
				_ = unix.Close(lockFd)
				_ = gen.Close()
				continue
			}
			// The lock was free: the holder is dead and pid reuse cannot
			// fake it. Remove the dir's contents fd-relative — but only if
			// everything inside is a plain non-directory entry, the only
			// shape af itself ever creates (#7503: nested dirs are unproven).
			removable := true
			for _, in := range inner {
				var ist unix.Stat_t
				if unix.Fstatat(genFd, in, &ist, unix.AT_SYMLINK_NOFOLLOW) != nil ||
					ist.Mode&unix.S_IFMT == unix.S_IFDIR {
					removable = false
					break
				}
			}
			if removable {
				for _, in := range inner {
					_ = unix.Unlinkat(genFd, in, 0)
				}
				if unix.Unlinkat(rootFd, name, unix.AT_REMOVEDIR) == nil {
					swept++
				} else {
					log.WarningLog.Printf("orphan tunnel sweep: cannot remove dead generation dir %q", filepath.Join(root.path, name))
				}
			}
			_ = unix.Flock(lockFd, unix.LOCK_UN)
			_ = unix.Close(lockFd)
			_ = gen.Close()
		}
	}
	return swept, nil
}

// SweepStaleTunnelSocketDirs runs the generation-dir half of the daemon's
// startup tunnel cleanup (#7495), beside SweepOrphanSandboxTunnels' process
// sweep: the process sweep kills leftover ssh children proven by env marker;
// this removes their now-dead socket paths once the flock proves the daemon
// that owned them is gone.
func SweepStaleTunnelSocketDirs() {
	root, err := func() (*tunnelSocketRoot, error) {
		home, err := currentTunnelHome()
		if err != nil {
			return nil, err
		}
		return verifyTunnelSocketRoot(home)
	}()
	if err != nil {
		// The root failed verification (or could not be created): sweeping
		// anything under an unverified directory is exactly what the check
		// exists to prevent — say so and stop.
		log.WarningLog.Printf("orphan tunnel sweep: cannot verify the tunnel socket directory; skipping the dir sweep: %v", err)
		return
	}
	defer func() { _ = root.dir.Close() }()
	swept, err := sweepStaleTunnelSocketDirs(root)
	if err != nil {
		log.WarningLog.Printf("orphan tunnel sweep: cannot list the tunnel socket directory; skipping the dir sweep: %v", err)
		return
	}
	if swept > 0 {
		log.InfoLog.Printf("orphan tunnel sweep: removed %d tunnel generation dir(s) whose owning af daemon is gone", swept)
	}
}
