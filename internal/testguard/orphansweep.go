package testguard

// Orphan sweeping for the temp dirs this package creates.
//
// IsolateTmux, SandboxTmux and SandboxHome each make a dir under os.TempDir
// and remove it in a cleanup — t.Cleanup for the per-test one, a returned
// restore func for the package-level ones. Those cleanups run only when the
// test binary exits through m.Run(). A binary that dies on a -timeout panic
// or SIGKILL never runs them, and a tmux server created on the private
// TMUX_TMPDIR daemonizes — it leaves the dead binary's process group by
// definition, so process-group containment (#4417) cannot reach it either.
// The dir and the server outlive their owner indefinitely (#4468).
//
// The fix is an owner stamp plus a lazy sweep. Every dir this package
// creates gets an `owner` file naming the creating test BINARY as a process
// instance — (pid, StartID), scoped by boot ID and PID namespace ID, the
// same identity proctree uses before signalling across daemon restarts. On
// entry, each of the three entry points sweeps sibling dirs once per
// process: a dir whose stamped owner is provably dead (process gone, or the
// pid recycled to a different instance, or the stamp names a different boot
// or PID namespace) is reaped — its tmux server is killed by socket and the
// dir removed. Every other outcome leaves the dir alone:
//
//   - owner instance still alive        -> a running test owns it; idleness
//     of the dir is never consulted, so a live-but-quiet test cannot be
//     mistaken for a corpse.
//   - stamp missing or malformed        -> a pre-stamp dir, or one caught
//     mid-create; there is no owner to check and no proof to manufacture.
//   - identity read fails               -> "could not look" is not "dead".
//   - current boot/namespace unreadable -> the pid identity cannot be
//     scoped to this boot, so nothing can be proven.
//
// Leaving an unproven dir wastes a tmpdir; killing a live test's server
// breaks a passing run. The sweep therefore errs exclusively toward
// leaving, and reports what it left so the leak stays visible rather than
// silent.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// ownerStampFile is the marker naming the creating test binary's process
// instance. It lives inside the dir so it is created and destroyed with it.
const ownerStampFile = "owner"

// orphanDirPatterns are the temp-dir prefixes this package creates.
// af-tmux-* covers both IsolateTmux (af-tmux-*) and SandboxTmux
// (af-tmux-pkg-*); af-test-home-* covers SandboxHome. SocketTempDir's af-*
// dirs are per-test t.TempDir-cleaned and deliberately not matched.
var orphanDirPatterns = []string{"af-tmux-*", "af-test-home-*"}

// orphanSweepBudget bounds the whole once-per-process sweep so a /tmp full
// of wedged servers cannot stall a test binary at startup.
var orphanSweepBudget = 30 * time.Second

// tmuxKillTimeout bounds each kill-server so a wedged orphan server cannot
// stall the sweep on its own socket.
const tmuxKillTimeout = 3 * time.Second

// ownerStamp is the recorded identity of the test binary that created a
// dir. pid and startID name the process instance (proctree.Process
// identity); bootID and nsID scope that identity to the boot and PID
// namespace that issued it, since StartID is only unique within a boot.
type ownerStamp struct {
	pid     int
	startID uint64
	bootID  string
	nsID    string
}

// writeOwnerStamp records the calling process's identity inside dir. It is
// best-effort: a dir whose stamp cannot be written is simply never reaped,
// which is the safe direction. The file is written via rename so a
// concurrent sweep never reads a partial stamp — a dir seen without a
// complete stamp reads as unstamped and is left alone.
func writeOwnerStamp(dir string) error {
	self, err := proctree.Lookup(os.Getpid())
	if err != nil {
		return fmt.Errorf("reading own process identity: %w", err)
	}
	boot, err := proctree.BootID()
	if err != nil {
		return fmt.Errorf("reading boot id: %w", err)
	}
	ns, err := proctree.PIDNamespaceID()
	if err != nil {
		return fmt.Errorf("reading pid namespace id: %w", err)
	}
	line := fmt.Sprintf("%d\t%d\t%s\t%s\n", self.PID, self.StartID, boot, ns)
	tmp := filepath.Join(dir, ownerStampFile+".tmp")
	if err := os.WriteFile(tmp, []byte(line), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, ownerStampFile))
}

// readOwnerStamp parses dir's stamp. ok is false for a missing, unreadable,
// or malformed file — all of which mean "unstamped" to the sweep.
func readOwnerStamp(dir string) (stamp ownerStamp, ok bool) {
	data, err := os.ReadFile(filepath.Join(dir, ownerStampFile))
	if err != nil {
		return ownerStamp{}, false
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) != 4 {
		return ownerStamp{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return ownerStamp{}, false
	}
	startID, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return ownerStamp{}, false
	}
	// Empty scope fields cannot scope the pid identity to a boot, so the
	// stamp cannot be acted on — treat it as absent.
	if fields[2] == "" || fields[3] == "" {
		return ownerStamp{}, false
	}
	return ownerStamp{pid: pid, startID: startID, bootID: fields[2], nsID: fields[3]}, true
}

// ownerVerdict is what the sweep can prove about a dir's owner. The zero
// value is "unknown" — the same convention EnvStatus uses — so any missed
// assignment or unhandled case defaults to the leave-it-alone outcome.
type ownerVerdict int

const (
	ownerUnknown ownerVerdict = iota
	ownerAlive
	ownerDead
)

// sweepEnv carries the sweep's process-table and tmux seams so tests can
// drive every verdict with fixtures instead of real tmux servers.
type sweepEnv struct {
	lookup     func(pid int) (proctree.Process, error)
	bootID     string // "" when the current boot identity is unreadable
	nsID       string // "" when the current namespace identity is unreadable
	killSocket func(sock string) error
	now        func() time.Time
}

// defaultSweepEnv reads the process scope once. A scope read that fails
// leaves the field empty, which turns every stamped dir into ownerUnknown —
// on a platform where we cannot scope a pid we cannot prove anything.
func defaultSweepEnv() *sweepEnv {
	boot, _ := proctree.BootID()
	ns, _ := proctree.PIDNamespaceID()
	return &sweepEnv{
		lookup:     proctree.Lookup,
		bootID:     boot,
		nsID:       ns,
		killSocket: killTmuxServer,
		now:        time.Now,
	}
}

// classifyDir decides what the sweep may do with one dir. It can return
// ownerDead only through proof; anything short of proof is ownerUnknown or
// ownerAlive, both of which leave the dir in place. stamped reports whether
// the dir carried a readable stamp at all, so the caller can count
// "unattributable" separately from "attributed but unproven".
func (e *sweepEnv) classifyDir(dir string) (verdict ownerVerdict, stamped bool, reason string) {
	stamp, ok := readOwnerStamp(dir)
	if !ok {
		return ownerUnknown, false, "no readable owner stamp"
	}
	if e.bootID == "" || e.nsID == "" {
		return ownerUnknown, true, "current boot/namespace identity unreadable"
	}
	// A stamp from a different boot or PID namespace names a process
	// instance that cannot exist here — (pid, StartID) is only unique
	// within its scope, so this check must precede the pid lookup.
	if stamp.bootID != e.bootID || stamp.nsID != e.nsID {
		return ownerDead, true, "owner stamp names a different boot or pid namespace"
	}
	cur, err := e.lookup(stamp.pid)
	switch {
	case errors.Is(err, proctree.ErrProcessExited), errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ESRCH):
		return ownerDead, true, "owner process exited"
	case err != nil:
		return ownerUnknown, true, fmt.Sprintf("cannot revalidate owner pid %d: %v", stamp.pid, err)
	case cur.StartID != stamp.startID:
		return ownerDead, true, "owner pid was recycled to a different process instance"
	default:
		return ownerAlive, true, ""
	}
}

// sweepStats is the once-per-process sweep report, logged by whichever
// entry point ran it so the leak is visible rather than silent.
type sweepStats struct {
	reaped       int // dirs whose owner was proven dead and removed
	live         int // stamped dirs whose owner is still running
	unattributed int // dirs with no readable stamp — pre-stamp debris or mid-create
	unproven     int // stamped dirs the sweep could not prove dead
	killFailed   int // dead-owner dirs whose server did not answer kill-server
}

func (s sweepStats) noteworthy() bool {
	return s.reaped > 0 || s.unattributed > 0 || s.unproven > 0 || s.killFailed > 0
}

func (s sweepStats) String() string {
	return fmt.Sprintf("reaped %d owner-dead test temp dirs; %d still owned by a live test; "+
		"%d left without provable ownership (unstamped: %d, identity unproven: %d, server kill failed: %d) — "+
		"unproven dirs are left in place deliberately (#4468)",
		s.reaped, s.live, s.unattributed+s.unproven, s.unattributed, s.unproven, s.killFailed)
}

// sweep scans baseDir for this package's temp dirs, reaping the ones whose
// stamped owner is provably dead. tmux dirs additionally get kill-server on
// every socket inside before removal — the daemonized server is the real
// survivor; the dir is only its footprint.
func (e *sweepEnv) sweep(baseDir string) sweepStats {
	var stats sweepStats
	deadline := e.now().Add(orphanSweepBudget)
	for _, pattern := range orphanDirPatterns {
		matches, err := filepath.Glob(filepath.Join(baseDir, pattern))
		if err != nil {
			continue // a malformed pattern cannot happen; keep sweeping
		}
		isTmux := strings.HasPrefix(pattern, "af-tmux-")
		for i, dir := range matches {
			if e.now().After(deadline) {
				stats.unproven += len(matches) - i
				return stats
			}
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				continue
			}
			verdict, stamped, _ := e.classifyDir(dir)
			switch verdict {
			case ownerAlive:
				stats.live++
			case ownerDead:
				if isTmux {
					stats.killFailed += e.killSockets(dir)
				}
				if err := os.RemoveAll(dir); err != nil {
					stats.unproven++
					continue
				}
				stats.reaped++
			case ownerUnknown:
				if stamped {
					stats.unproven++
				} else {
					stats.unattributed++
				}
			}
		}
	}
	return stats
}

// killSockets runs kill-server on every unix socket in dir and returns how
// many attempts failed. A dead socket fails fast ("no server running"),
// which is indistinguishable from a wedged one — the dir is removed either
// way since its owner is proven dead, but the count stays in the report.
func (e *sweepEnv) killSockets(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	failed := 0
	for _, entry := range entries {
		if entry.Type()&os.ModeSocket == 0 {
			continue
		}
		if err := e.killSocket(filepath.Join(dir, entry.Name())); err != nil {
			failed++
		}
	}
	return failed
}

// killTmuxServer asks the server on sock to exit, bounded so a wedged
// orphan cannot stall the sweep. Same shape as ambientTmuxOutput: a
// context timeout plus WaitDelay so a socket that never answers cannot hang.
func killTmuxServer(sock string) error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxKillTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "-S", sock, "kill-server")
	cmd.WaitDelay = tmuxTripwireWaitDelay
	return cmd.Run()
}

var (
	orphanSweepOnce  sync.Once
	orphanSweepStats sweepStats
)

// sweepOrphanTempDirsOnce runs the sweep at most once per test binary —
// whichever of IsolateTmux, SandboxTmux or SandboxHome runs first — and
// returns the stats plus whether this call was the one that ran it, so only
// that caller logs the report.
func sweepOrphanTempDirsOnce() (sweepStats, bool) {
	ran := false
	orphanSweepOnce.Do(func() {
		ran = true
		if os.Getenv("AF_DISABLE_ORPHAN_SWEEP") == "1" {
			return
		}
		orphanSweepStats = defaultSweepEnv().sweep(os.TempDir())
	})
	return orphanSweepStats, ran
}
