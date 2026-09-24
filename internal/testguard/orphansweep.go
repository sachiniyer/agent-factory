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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testresidue"
)

// ownerStampFile is the marker naming the creating test binary's process
// instance. It lives inside the dir so it is created and destroyed with it.
// The name is testresidue's so `af doctor` recognises the stamp as harness
// content rather than as something a test run does not leave behind.
const ownerStampFile = testresidue.OwnerStampFile

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
	tmp := filepath.Join(dir, testresidue.OwnerStampTempFile)
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
	// A stamp from a different PID namespace names a process whose pid lives
	// in a number space we cannot read — the owner may be alive there (two
	// test containers sharing a host /tmp mount). Unverifiable is not dead.
	if stamp.nsID != e.nsID {
		return ownerUnknown, true, "owner stamp names a different pid namespace"
	}
	// Same namespace, different boot: the stamped instance cannot still
	// exist — but only when BOTH boot ids are the kernel's strong UUID.
	// proctree.BootID falls back to the PID-namespace identity ("pidns:…")
	// when a subset=pid procfs hides /proc/sys/kernel/random/boot_id, and
	// that fallback is governed by the MOUNT namespace, not the PID
	// namespace. Two processes CAN share a PID namespace (so the nsID gate
	// above passed) yet disagree on boot id because one read the fallback
	// and the other read the real UUID. The fallback scopes identity to the
	// namespace but can be reused after a host reboot, so the proctree.BootID
	// contract requires a persisted destructive action that crosses the
	// fallback boundary to pair the mismatch with proof from the live
	// process (see BootIDIsFallback). Only when both ids are strong is a
	// mismatch proof of death: a reboot ended every instance the stamp
	// could name. When either side is a fallback, fall through to the
	// live-process lookup below, which itself errs toward leaving
	// (ownerUnknown) when it cannot read.
	if stamp.bootID != e.bootID &&
		!proctree.BootIDIsFallback(stamp.bootID) &&
		!proctree.BootIDIsFallback(e.bootID) {
		return ownerDead, true, "owner stamp names a different boot"
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
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return stats // cannot list the temp base — sweep nothing, prove nothing
	}
	for _, entry := range entries {
		// testresidue owns which names this package creates: the exact
		// MkdirTemp shape of IsolateTmux, SandboxTmux, SandboxHome and the
		// sandboxed HOME, and nothing merely starting with one of their prefixes.
		// SocketTempDir's af-* dirs are per-test t.TempDir-cleaned and
		// deliberately not matched. An exact name test over the listing rather
		// than filepath.Glob on purpose: a TMPDIR containing a glob metacharacter
		// would make Glob misread or fail the pattern and silently sweep
		// nothing.
		kind := testresidue.Classify(entry.Name())
		if kind == testresidue.None {
			continue
		}
		// Only a tmux socket dir is a TMUX_TMPDIR, so only it can hold a
		// tmux server to stop. A SandboxHome is an AGENT_FACTORY_HOME and is
		// removed without kill-server. A sandboxed HOME holds neither a server
		// nor a socket, and carries no owner stamp (sandboxUserHome never writes
		// one), so it reads as unstamped below — counted as unattributed and
		// left in place, which keeps the leak visible until `af doctor` clears
		// it on content (#4170 sandbox-HOME regression).
		isTmux := kind == testresidue.TmuxSocketDir
		dir := filepath.Join(baseDir, entry.Name())
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		if e.now().After(deadline) {
			stats.unproven++
			continue
		}
		verdict, stamped, _ := e.classifyDir(dir)
		switch verdict {
		case ownerAlive:
			stats.live++
		case ownerDead:
			if isTmux {
				stats.killFailed += e.killSockets(dir, deadline)
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
	return stats
}

// killSockets runs kill-server on every unix socket under dir and returns
// how many attempts failed or were skipped when the sweep deadline expired.
// TMUX_TMPDIR is only the base — the real socket lives one level down at
// dir/tmux-<uid>/default — so the search walks the tree rather than reading
// the top level. A dead socket fails fast ("no server running"), which is
// indistinguishable from a wedged one — the dir is removed either way since
// its owner is proven dead, but the count stays in the report.
func (e *sweepEnv) killSockets(dir string, deadline time.Time) int {
	var sockets []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&os.ModeSocket != 0 {
			sockets = append(sockets, path)
		}
		return nil
	})
	failed := 0
	for i, sock := range sockets {
		if e.now().After(deadline) {
			failed += len(sockets) - i
			break
		}
		if err := e.killSocket(sock); err != nil {
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
