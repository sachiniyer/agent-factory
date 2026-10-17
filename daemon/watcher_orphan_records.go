package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// The second ownership-proof channel for orphaned watch commands (#5329).
//
// The environ markers are the primary proof, but they are only as good as the
// kernel's willingness to show them. On SIP-enabled macOS the kernel withholds
// the environment of Apple platform binaries, so a shell-only watch command —
// `while :; do sleep 60; done` is /bin/sh plus /bin/sleep — has no member whose
// environ proctree.Environ can read. Every member answers "unknown", the group
// is never collected, and the orphan survives its whole next generation: the
// exact defect the sweep exists to kill.
//
// So the daemon also records each watch command it spawns, in a file af writes
// itself under this AF home: the leader's pid and pgid, its start stamp, the
// boot and PID namespace it ran in, and the spawning generation. A process the
// kernel will not describe still cannot evade a record af wrote about it. This
// is the fallback the issue author sanctioned ("recorded pgid plus process start
// time stored in this home's state"), built the way the editor owner record is
// (vscode_owner.go): every recorded identity field must match the live process
// before the record authorizes anything, and anything unverifiable means no
// match.
//
// Pid reuse cannot turn a stale record into a kill: StartID pins the process
// instance, BootID and the PID namespace pin the kernel and container it ran
// in, and the process's CURRENT pgid must still equal the recorded one. A
// record also never overrules a live answer: it is consulted only when the
// leader's environ is unreadable, and not at all when the boot identity is the
// reboot-unstable PID-namespace fallback (proctree.BootIDIsFallback). The
// record proves only the group LEADER; a group whose leader has exited while
// children carry on is reachable through the environ channel alone.
//
// Records are written only by the supervisor RunDaemon arms (reapOrphans): the
// same home lock that makes a foreign generation mean "orphaned" is what makes
// one record file per home safe to rewrite, and test supervisors never touch a
// home's record file.

// watchChildRecordName is the record file, at the AF home root beside the
// daemon's other single-file records (daemon.pid, state.json, tasks.json).
const watchChildRecordName = "watch-children.json"

// watchChildRecord is one spawned watch command's process identity.
type watchChildRecord struct {
	TaskID       string `json:"task_id"`
	PID          int    `json:"pid"`
	PGID         int    `json:"pgid"`
	StartID      uint64 `json:"start_id"`
	BootID       string `json:"boot_id"`
	PIDNamespace string `json:"pid_namespace_id"`
	Generation   string `json:"generation"`
}

// valid reports whether every field a match depends on is present. A record
// missing any of them can never authorize a signal.
func (r watchChildRecord) valid() bool {
	return r.PID > 1 && r.PGID > 1 && r.StartID != 0 && r.BootID != "" &&
		r.PIDNamespace != "" && r.Generation != ""
}

// Seams for the record channel, in the style of the sweep's own.
var (
	watchRecordLookup       = proctree.Lookup
	watchRecordBootID       = proctree.BootID
	watchRecordPIDNamespace = proctree.PIDNamespaceID
)

func watchChildRecordPath() (string, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, watchChildRecordName), nil
}

// maxWatchChildRecordBytes bounds the record file read. One record is about
// 250 bytes, so this holds thousands of live watch commands; anything larger
// was not written by af.
const maxWatchChildRecordBytes = 1 << 20

// readWatchChildRecords loads the record file. A missing file is an empty set;
// an unreadable or corrupt one is an error the caller reports and survives.
//
// Only a bounded regular file is read (#5329). The startup sweep reads it under
// s.mu, so a FIFO or device planted at this path would otherwise block the
// first reconcile — and every watcher reload and shutdown behind it — or stream
// without end. The open is O_NOFOLLOW and O_NONBLOCK so neither a symlink to
// such a file nor a FIFO with no writer can stall the open itself, and the type
// is checked on the opened descriptor, not on a path that could be swapped.
func readWatchChildRecords(path string) ([]watchChildRecord, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("watch child record file %s is not a regular file (%s)", path, info.Mode().Type())
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxWatchChildRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxWatchChildRecordBytes {
		return nil, fmt.Errorf("watch child record file %s exceeds %d bytes", path, maxWatchChildRecordBytes)
	}
	var records []watchChildRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("corrupt watch child record file %s: %w", path, err)
	}
	return records, nil
}

// recordWatchChild persists the identity of a watch command just started as
// pid (its own group leader, via Setpgid) for taskID.
//
// It is split in two so that it never stalls child supervision (#5329). The
// identity is captured synchronously, from /proc rather than the AF home, and
// while the caller still holds the child unreaped, so the pid cannot have been
// recycled under it. The file read, rewrite and fsync then run on their own
// goroutine: they touch the AF home's filesystem, and a stalled filesystem there
// must not keep runOnce from installing the reader goroutines and the stop
// watchdog that bound the child's life.
//
// A bookkeeping failure never stops a healthy watch command: on Linux the
// environ channel still proves it, and on macOS the cost is only that this one
// child cannot be reaped after an unclean daemon death — which the warning says.
func (s *watcherSupervisor) recordWatchChild(taskID string, pid int) {
	record, err := s.captureWatchChildRecord(taskID, pid)
	if err == nil && record.valid() {
		go func() {
			if err := s.persistWatchChildRecord(record); err != nil {
				warnWatchChildRecord(taskID, pid, err)
			}
		}()
		return
	}
	if err != nil {
		warnWatchChildRecord(taskID, pid, err)
	}
}

func warnWatchChildRecord(taskID string, pid int, err error) {
	log.WarningLog.Printf("watch task %s: cannot record watch command pid %d (%v); "+
		"if this daemon dies uncleanly, a later daemon may be unable to reap it", taskID, pid, err)
}

// captureWatchChildRecord reads pid's identity. An already-exited child yields
// a zero record and no error: there is nothing left to reap, so nothing to record.
func (s *watcherSupervisor) captureWatchChildRecord(taskID string, pid int) (watchChildRecord, error) {
	bootID, err := watchRecordBootID()
	if err != nil {
		return watchChildRecord{}, fmt.Errorf("kernel boot identity: %w", err)
	}
	pidNamespace, err := watchRecordPIDNamespace()
	if err != nil {
		return watchChildRecord{}, fmt.Errorf("PID namespace: %w", err)
	}
	process, err := watchRecordLookup(pid)
	if err != nil {
		if errors.Is(err, proctree.ErrProcessExited) {
			return watchChildRecord{}, nil
		}
		return watchChildRecord{}, fmt.Errorf("process identity: %w", err)
	}
	record := watchChildRecord{
		TaskID: taskID, PID: pid, PGID: pid, StartID: process.StartID,
		BootID: bootID, PIDNamespace: pidNamespace, Generation: s.generation,
	}
	if !record.valid() {
		return watchChildRecord{}, fmt.Errorf("incomplete process identity for pid %d", pid)
	}
	return record, nil
}

// persistWatchChildRecord merges record into the home's record file, pruning
// every entry that no longer names a live process instance — record included,
// if its child exited before this ran.
func (s *watcherSupervisor) persistWatchChildRecord(record watchChildRecord) error {
	path, err := watchChildRecordPath()
	if err != nil {
		return err
	}
	pid, bootID, pidNamespace := record.PID, record.BootID, record.PIDNamespace

	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	existing, err := readWatchChildRecords(path)
	if err != nil {
		// Start over rather than lose this child too; the corrupt content only
		// ever described processes whose environ channel still applies.
		log.WarningLog.Printf("rewriting watch child records: %v", err)
		existing = nil
	}
	kept := make([]watchChildRecord, 0, len(existing)+1)
	for _, r := range existing {
		if r.valid() && r.PID != pid && r.BootID == bootID && r.PIDNamespace == pidNamespace && watchRecordStillLive(r) {
			kept = append(kept, r)
		}
	}
	if watchRecordStillLive(record) {
		kept = append(kept, record)
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	// Refuses a symlinked path, like the editor owner record: this file is
	// process proof the daemon re-reads before signalling, never user-authored.
	return config.AtomicWriteFileRefusingLink(path, append(raw, '\n'), 0o600)
}

// watchRecordStillLive reports whether r's process instance is still running.
// A record that cannot be checked is dropped: it authorizes nothing anyway.
func watchRecordStillLive(r watchChildRecord) bool {
	p, err := watchRecordLookup(r.PID)
	return err == nil && p.StartID == r.StartID
}

// collectRecordedOrphans adds every group whose LEADER a previous generation
// recorded and that still verifies against snap into byGroup, alongside the
// environ channel's candidates. It applies the sweep's own exclusions (self,
// own group, own descendants, foreign uid) plus the record's identity fields.
// Any doubt skips the record.
func (s *watcherSupervisor) collectRecordedOrphans(snap map[int]proctree.Process, uid, ownPGID int, byGroup map[int]*watchOrphanGroup) {
	path, err := watchChildRecordPath()
	if err != nil {
		log.WarningLog.Printf("watch orphan sweep: cannot resolve watch child records: %v", err)
		return
	}
	records, err := readWatchChildRecords(path)
	if err != nil {
		log.WarningLog.Printf("watch orphan sweep: ignoring watch child records: %v", err)
		return
	}
	if len(records) == 0 {
		return
	}
	bootID, err := watchRecordBootID()
	if err != nil {
		log.WarningLog.Printf("watch orphan sweep: ignoring watch child records: kernel boot identity: %v", err)
		return
	}
	if proctree.BootIDIsFallback(bootID) {
		// A subset=pid procfs hides the boot UUID, and the PID-namespace
		// fallback can repeat after a host reboot, so a stale record could line
		// up with an unrelated process. proctree's contract requires live proof
		// alongside it for destructive work; the record channel exists precisely
		// for processes that offer none, so it stands down here (#5329).
		log.InfoLog.Printf("watch orphan sweep: ignoring watch child records: boot identity %q is not reboot-stable", bootID)
		return
	}
	pidNamespace, err := watchRecordPIDNamespace()
	if err != nil {
		log.WarningLog.Printf("watch orphan sweep: ignoring watch child records: PID namespace: %v", err)
		return
	}
	self := os.Getpid()
	for _, r := range records {
		// This generation's own records are its live watchers; the sweep runs
		// before this generation spawns anything, but the guard does not rely
		// on that ordering.
		if !r.valid() || r.Generation == s.generation || r.BootID != bootID || r.PIDNamespace != pidNamespace {
			continue
		}
		p, ok := snap[r.PID]
		if !ok || p.StartID != r.StartID || r.PID == self || watchOrphanDescendsFrom(snap, r.PID, self) {
			continue
		}
		if procUID, ok := watchOrphanUID(r.PID); !ok || procUID != uid {
			continue
		}
		// The record speaks only for a leader whose environ the kernel
		// withholds. A readable environ is the stronger, live answer, and the
		// environ channel has already acted on it: marked means collected
		// there, unmarked means not ours — and no record may overrule that.
		if _, err := watchOrphanEnviron(r.PID); !errors.Is(err, proctree.ErrEnvUnreadable) {
			continue
		}
		pgid, err := watchOrphanPGID(r.PID)
		if err != nil || pgid != r.PGID || pgid <= 1 || pgid == ownPGID {
			continue
		}
		if same, err := watchOrphanSameIdentity(p); err != nil || !same {
			continue
		}
		g := byGroup[pgid]
		if g == nil {
			g = &watchOrphanGroup{pgid: pgid}
			byGroup[pgid] = g
		}
		if !slices.ContainsFunc(g.members, func(m proctree.Process) bool { return m.PID == r.PID }) {
			g.members = append(g.members, p)
		}
		if r.TaskID != "" && !slices.Contains(g.taskIDs, r.TaskID) {
			g.taskIDs = append(g.taskIDs, r.TaskID)
		}
	}
}
