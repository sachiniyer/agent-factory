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
//     acceptor outside the tunnel child's own process group
//     (verifyTunnelPeer). No credential is sent to an endpoint that is not
//     provably af's own ssh child.
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

// tunnelGenDirEntry pins one generation dir plus its held .lock per canonical
// AF home. Production resolves one home per process; a test binary can run
// several homes through the same code — keying the dir on the CURRENT home
// (read fresh, not the once-fixed env-marker home) keeps a later home from
// inheriting a dir under an earlier, deleted one.
type tunnelGenDirEntry struct {
	once sync.Once
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
		root, verr := verifyTunnelSocketRoot(home)
		if verr != nil {
			e.err = verr
			return
		}
		e.dir, e.err = createTunnelGenerationDir(root, e)
	})
	return e.dir, e.err
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

// verifyTunnelSocketRoot returns <home>/tunnels after proving ON THE OPENED
// DIRECTORY — never a re-read path — that it is a real directory owned by
// this uid with mode 0700. Anything else is refused with the fix to run:
// checking the opened fd is what keeps a swap between stat and use from
// slipping a foreign directory past the check.
func verifyTunnelSocketRoot(home string) (string, error) {
	root := filepath.Join(home, tunnelSocketRootName)
	created := false
	if err := unix.Mkdir(root, 0o700); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return "", fmt.Errorf("cannot create the tunnel socket directory %q: %w", root, err)
		}
	} else {
		created = true
	}
	// O_NOFOLLOW + O_DIRECTORY refuse a symlink or a non-directory at the open
	// itself; Fstat then describes the very object the socket paths will be
	// created under.
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
			// O_NOFOLLOW reports a symlink ELOOP on some kernels and ENOTDIR
			// (with O_DIRECTORY) on others — refuse both spellings.
			return "", fmt.Errorf("the tunnel socket directory %q is a symlink or not a directory; af requires a "+
				"real directory it can verify as private — remove it and let af create it: rm %s", root, root)
		default:
			return "", fmt.Errorf("cannot open the tunnel socket directory %q: %w", root, err)
		}
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", fmt.Errorf("cannot stat the opened tunnel socket directory %q: %w", root, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", fmt.Errorf("the tunnel socket directory %q is not a directory — remove it and let af create it: rm %s", root, root)
	}
	if st.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("the tunnel socket directory %q is owned by uid %d, not this user (uid %d): "+
			"af's credential-bearing tunnel endpoints live there, so af refuses to use it — chown it to this user or remove it and let af create it",
			root, st.Uid, os.Geteuid())
	}
	if st.Mode&0o777 != 0o700 {
		if !created {
			return "", fmt.Errorf("the tunnel socket directory %q has mode %04o; af requires 0700 so no other local "+
				"account can reach the tunnel endpoints it holds — fix it with: chmod 0700 %s",
				root, st.Mode&0o777, root)
		}
		// af created it just now under a umask that stripped bits; normalize
		// through the opened fd — a path-based chmod could follow a swapped-in
		// link.
		if err := unix.Fchmod(fd, 0o700); err != nil {
			return "", fmt.Errorf("cannot set mode 0700 on the tunnel socket directory %q: %w", root, err)
		}
	}
	return root, nil
}

// createTunnelGenerationDir mkdirs this daemon's own generation dir under the
// verified root and takes its .lock — exclusive, held for the process's life
// on the entry's fd, O_CLOEXEC so no spawned child can hold it open past
// daemon death. The lock is the sweep's hand-off safety: while ANY af daemon
// — this one, or a predecessor mid-upgrade — is alive, its generation dir
// cannot be swept.
func createTunnelGenerationDir(root string, e *tunnelGenDirEntry) (string, error) {
	for attempt := 0; attempt < 4; attempt++ {
		name := fmt.Sprintf("d%d-%06x", os.Getpid(), rand.Uint32()&0xffffff)
		dir := filepath.Join(root, name)
		if err := unix.Mkdir(dir, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return "", fmt.Errorf("cannot create the tunnel generation directory %q: %w", dir, err)
		}
		lockPath := filepath.Join(dir, tunnelGenLockName)
		lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("cannot create the tunnel generation lock %q: %w", lockPath, err)
		}
		// LOCK_EX|LOCK_NB cannot fail against a just-created dir in practice;
		// if it ever did, a foreign holder is exactly what the lock exists to
		// detect — treat the dir as unusable rather than shared.
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = lock.Close()
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("cannot lock the tunnel generation directory %q: %w", dir, err)
		}
		e.lock = lock
		return dir, nil
	}
	return "", fmt.Errorf("cannot mint a unique tunnel generation directory under %q", root)
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

// verifyTunnelPeer is the endpoint's per-connection proof (#7495): the kernel
// names the pid that accepted this unix-socket connection, and af writes
// nothing until that pid is a member of THIS tunnel generation's process
// group — the group the spawn's Setpgid put its ssh child into, whose id is
// the group leader's pid. A foreign listener cannot forge the pid the kernel
// reports; it can only be refused, and a refusal is a tunnel failure, never a
// request sent to the wrong endpoint.
func (p *sandboxProvisioner) verifyTunnelPeer(conn *net.UnixConn) error {
	pid, err := peercred.ConnPID(conn)
	if err != nil {
		return fmt.Errorf("the kernel cannot name the socket's acceptor: %w", err)
	}
	p.reapMu.Lock()
	t := p.tunnel
	p.reapMu.Unlock()
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return errors.New("the tunnel's ssh child is not running")
	}
	leader := t.cmd.Process.Pid
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		return fmt.Errorf("cannot read the acceptor's process group (pid %d): %w", pid, err)
	}
	if pgid != leader {
		return fmt.Errorf("the tunnel endpoint's acceptor (pid %d, process group %d) is not a member of "+
			"this tunnel's process group %d — refusing to use a listener that is not provably af's own ssh child",
			pid, pgid, leader)
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
// daemon is provably dead, and nothing else (#7495):
//
//   - a dir whose .lock can be FLOCK'd has no live owner — flock dies with the
//     process that holds it, which pid reuse cannot fake. Its sockets are
//     dead-path files; RemoveAll takes the whole dir.
//   - a dir whose .lock is HELD belongs to a live af process — including a
//     predecessor daemon still serving sessions during an upgrade hand-off —
//     and is never touched.
//   - a dir with no .lock at all is removed only when empty (a generation
//     that died between mkdir and its lock; it can hold no socket yet). A
//     non-empty lockless dir is unproven and stays.
//   - anything not named like a generation dir is not af's to classify and
//     stays.
func sweepStaleTunnelSocketDirs(root string) (swept int, err error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if !e.IsDir() || !tunnelGenDirRE.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		lockPath := filepath.Join(dir, tunnelGenLockName)
		lock, lerr := os.OpenFile(lockPath, os.O_RDWR, 0)
		if errors.Is(lerr, os.ErrNotExist) {
			// No lock file: only an EMPTY dir is provably a dead setup —
			// nothing can be served through it.
			inner, derr := os.ReadDir(dir)
			if derr == nil && len(inner) == 0 {
				if rerr := os.Remove(dir); rerr == nil {
					swept++
				}
			}
			continue
		}
		if lerr != nil {
			continue
		}
		if ferr := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); ferr != nil {
			// Held by a live process — this generation, or a predecessor's.
			_ = lock.Close()
			continue
		}
		// The lock was free: the holder is dead and pid reuse cannot fake it.
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		if rerr := os.RemoveAll(dir); rerr != nil {
			log.WarningLog.Printf("orphan tunnel sweep: cannot remove dead generation dir %q: %v", dir, rerr)
			continue
		}
		swept++
	}
	return swept, nil
}

// SweepStaleTunnelSocketDirs runs the generation-dir half of the daemon's
// startup tunnel cleanup (#7495), beside SweepOrphanSandboxTunnels' process
// sweep: the process sweep kills leftover ssh children proven by env marker;
// this removes their now-dead socket paths once the flock proves the daemon
// that owned them is gone.
func SweepStaleTunnelSocketDirs() {
	root, err := func() (string, error) {
		home, err := currentTunnelHome()
		if err != nil {
			return "", err
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
	swept, err := sweepStaleTunnelSocketDirs(root)
	if err != nil {
		log.WarningLog.Printf("orphan tunnel sweep: cannot list the tunnel socket directory; skipping the dir sweep: %v", err)
		return
	}
	if swept > 0 {
		log.InfoLog.Printf("orphan tunnel sweep: removed %d tunnel generation dir(s) whose owning af daemon is gone", swept)
	}
}
