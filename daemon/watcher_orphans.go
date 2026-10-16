package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Reaping the watch commands a previous daemon generation left behind (#5329).
//
// The defect: the autostart unit runs with KillMode=process on purpose — tmux
// servers and scoped session children must outlive a daemon restart — so when
// the daemon dies uncleanly (SIGKILL, OOM), its shutdown handler never runs and
// every watch command it spawned keeps running, untracked. The next generation
// spawns a replacement next to it. A watch script that guards itself with a
// singleton lock (a reasonable pattern: it exists to prevent duplicate
// watchers) then exits 1 on every attempt, the five-exits-in-ten-minutes
// breaker trips within ~15 seconds of startup, and the task is dead until a
// human runs `af tasks restart`. One crash permanently disabled a watch.
//
// The fix is ownership markers, never argv: every watch command is spawned with
// AF_HOME (this daemon's canonical AF home) and AF_WATCH_GEN (a random token
// minted once per daemon generation). /proc/<pid>/environ keeps a process's
// initial environment through orphaning and reparenting, so the marker is a
// durable record the orphan carries itself. The same scheme already authorizes
// tmux orphan reaping (session/tmux/envmarker.go).
//
// Two proof channels feed the same candidate set. The environ markers are
// primary. The fallback is a record af writes at spawn — leader pid, pgid,
// start stamp, boot and PID namespace, generation — for kernels that withhold a
// process's environment: SIP-enabled macOS does so for Apple platform binaries,
// so a watch command made only of /bin/sh and /bin/sleep has no readable
// member. A process whose environ the kernel will not show still cannot evade a
// record af wrote itself (watcher_orphan_records.go).
//
// Why a foreign generation means "orphaned": RunDaemon holds the per-home
// singleton lock before it arms anything, so at most one daemon generation of
// this home is alive at a time. A watch command carrying this home's AF_HOME and
// any generation other than ours therefore belongs to a daemon that is gone.
// That inference is only sound under the lock, which is why only RunDaemon turns
// reaping on (watcherSupervisor.reapOrphans).
//
// Why once, inside the first reconcile: every watcher spawn funnels through
// reconcile, and the first one is not always armTaskAutomation's full re-arm — a
// task write landing in the startup probation window runs a scoped reconcile
// that spawns its watcher first. Sweeping under s.mu in whichever reconcile
// comes first guarantees the orphans are gone before ANY replacement starts.
//
// Fail open on scan errors: an unreaped orphan is exactly today's behavior,
// whereas a daemon wedged at startup on an unreadable process table is worse.
// Fail closed on every ownership doubt: an unreadable uid, environ, identity or
// pgid means the process is skipped, never signaled.
//
// Markers are proof that a group is ours to reap, but an af-owned member means
// the group is af infrastructure regardless of what the markers say. A watch
// script that runs `af` while the daemon is down ad-hoc-spawns a daemon from its
// own environment; main strips AF_WATCH_GEN before that daemon can forward it
// (sessionenv.StripOwnershipMarkers), but its OWN environ is fixed at exec and
// still carries the stale generation. So a group holding a tmux server, an af session-marked
// process, or an af binary leading its own session is spared outright
// (watchOrphanInfraReason) — a stale watch command is cheaper than a reaped
// session fleet.
//
// Why an ancestor of this daemon is still reapable: a watch script that runs
// `af` while the daemon is down is mid-call when this daemon sweeps — its
// pending `af` is blocked on the ping to the daemon it just ad-hoc-spawned — so
// the orphan is this daemon's grandparent. Excluding ancestors let exactly that
// orphan survive a whole extra generation beside its replacement (found by the
// #5329 play-test). What makes signaling its group safe is not ancestry but the
// group boundary: startDaemonChild Setsids the daemon into its own session and
// group, the sweep never signals its own pgid, and killing the orphan's group
// only ends the pending `af` call, which the detached daemon does not need. Its
// own descendants stay excluded: those are this generation's live children,
// whatever stale marker an older binary may have let them inherit.
//
// The residual gaps, stated so they are not rediscovered: a descendant that
// both leaves the watch command's process group (setsid) AND scrubs its
// environment carries no evidence we could act on; and where the kernel
// withholds every member's environ, only the record channel applies, which
// proves the group LEADER — if the leader has already exited, that group is
// out of reach. Both are out of reach by design: guessing would mean signaling
// processes we cannot prove are ours.

// envWatchGeneration names the per-daemon-generation marker stamped into every
// watch command's environment (#5329).
const envWatchGeneration = "AF_WATCH_GEN"

// watchOrphanKillWait bounds how long the sweep waits for a group to vanish
// after the escalation SIGKILL. SIGKILL cannot be caught, so this only covers
// kernel teardown latency.
const watchOrphanKillWait = time.Second

// watchOrphanPoll is the cadence at which the sweep re-checks signaled groups.
const watchOrphanPoll = 25 * time.Millisecond

// Test seams, mirroring the package's existing injection style: a test can
// substitute a fabricated process table (for paths such as a foreign uid that an
// unprivileged test cannot stage for real) and observe signals without sending
// them.
var (
	watchOrphanSnapshot  = proctree.Snapshot
	watchOrphanEnviron   = proctree.Environ
	watchOrphanUID       = proctree.UID
	watchOrphanPGID      = syscall.Getpgid
	watchOrphanKillGroup = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }
	watchOrphanSleep     = time.Sleep
	// watchOrphanSameIdentity re-verifies a snapshotted process instance; a
	// seam so fake-table tests can reach the kill path.
	watchOrphanSameIdentity = proctree.SameIdentity
	// watchOrphanExecutable names the running af binary, whose basename is how
	// an af daemon is recognized among a group's members.
	watchOrphanExecutable = os.Executable
)

// newWatchGeneration mints the random token that distinguishes this daemon
// generation's watch commands from every earlier one's. A rand failure must not
// stop the daemon from starting, so it falls back to a time+pid token, which is
// still unique across generations of one home (they never overlap, see above).
func newWatchGeneration() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d-p%d", time.Now().UnixNano(), os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// watchMarkerHome returns the AF_HOME value stamped into watch commands. The
// canonical form keeps the marker stable across symlinked spellings of one home;
// if canonicalizing fails the raw dir still names the home, and the sweep
// canonicalizes both sides at compare time anyway.
func watchMarkerHome() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	if canonical, err := canonicalDir(dir); err == nil {
		return canonical, nil
	}
	return dir, nil
}

// watchChildEnvDropped are the inherited variables watchChildEnv removes before
// building a watch command's environment.
//
// Most are variables it then sets itself. The tmux pane-ancestry markers are
// dropped outright: a daemon launched from inside an af pane inherits that
// pane's AF_SESSION/AF_SESSION_GEN, but a watch command is not a member of that
// pane. Passing the markers on would misattribute it to an unrelated session,
// and the tmux session orphan reaper (session/tmux/reap.go) keys on exactly
// those markers plus AF_HOME — so tearing that pane session down could kill
// this home's watch commands (#5329). AF_TESTGUARD_RUN is deliberately kept: it
// only attributes processes leaked by a test run, which stays useful here.
var watchChildEnvDropped = []string{
	"AF_TASK_ID", "AF_TASK_NAME", "AF_PROJECT_PATH", envWatchGeneration, "AF_HOME",
	tmux.EnvMarkerSession, tmux.EnvMarkerGeneration,
}

// watchChildEnv builds a watch command's environment: the daemon's own, plus
// the AF_TASK_* script contract, plus the ownership markers (#5329).
//
// Inherited tmux pane-ancestry markers are dropped (see watchChildEnvDropped).
// Inherited copies of every variable set here are dropped first, because
// environ readers — the sweep included — take the FIRST match. A daemon started
// inside another home's tmux pane inherits that home's AF_HOME, which would
// otherwise shadow ours and make our own watch commands look foreign (never
// reaped), or a stale AF_WATCH_GEN could pin them to a generation they do not
// belong to. If the home cannot be resolved, AF_HOME is omitted: that child can
// then never be reaped, which is the safe direction.
func (s *watcherSupervisor) watchChildEnv(taskID, name, dir string) []string {
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+len(watchChildEnvDropped))
	for _, kv := range inherited {
		if !hasEnvKeyIn(kv, watchChildEnvDropped) {
			env = append(env, kv)
		}
	}
	env = append(env,
		"AF_TASK_ID="+taskID,
		"AF_TASK_NAME="+name,
		"AF_PROJECT_PATH="+dir,
		envWatchGeneration+"="+s.generation,
	)
	if home, err := watchMarkerHome(); err == nil {
		env = append(env, "AF_HOME="+home)
	} else {
		log.WarningLog.Printf("watch task %s: cannot resolve AF home for its ownership marker; "+
			"a later daemon will not be able to reap it if this daemon dies uncleanly: %v", taskID, err)
	}
	return env
}

func hasEnvKeyIn(kv string, keys []string) bool {
	for _, key := range keys {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// firstEnv returns the FIRST value of key in environ — the one getenv(3) and
// every other environ reader in the tree resolve to.
func firstEnv(environ []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range environ {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):], true
		}
	}
	return "", false
}

// watchOrphanGroup is one process group holding at least one watch command a
// previous generation marked. members are only the MARKED processes captured
// in the snapshot; they are the evidence that licenses signaling the group.
type watchOrphanGroup struct {
	pgid    int
	members []proctree.Process
	taskIDs []string
}

func (g *watchOrphanGroup) describe() string {
	ids := "unknown task"
	if len(g.taskIDs) > 0 {
		ids = "task " + strings.Join(g.taskIDs, ", ")
	}
	return fmt.Sprintf("process group %d (%s, %d marked process(es))", g.pgid, ids, len(g.members))
}

// anyMemberAlive reports whether at least one captured marked member is still
// the same live process instance. An identity read that errors counts as not
// alive here: this answers "may we signal the group", and an unverifiable
// member grants no such license.
func (g *watchOrphanGroup) anyMemberAlive() bool {
	for _, p := range g.members {
		if same, err := watchOrphanSameIdentity(p); err == nil && same {
			return true
		}
	}
	return false
}

// watchOrphanGroupAlive probes a whole group with signal 0 through the kill
// seam, classified by the same rules as the editor supervisor's group probe.
func watchOrphanGroupAlive(pgid int) (bool, error) {
	return killProbeAlive(watchOrphanKillGroup(pgid, 0))
}

// sweepOrphanedWatchCommands stops every watch command a previous daemon
// generation of this AF home left running (#5329). It runs at most once per
// supervisor, under s.mu, from the first reconcile — see the file comment.
func (s *watcherSupervisor) sweepOrphanedWatchCommands() {
	groups := s.findOrphanedWatchGroups()
	if len(groups) == 0 {
		return
	}
	s.stopOrphanedWatchGroups(groups)
}

// findOrphanedWatchGroups scans the process table for processes that are
// provably a previous generation's watch commands, grouped by process group.
// Any failure to read the table itself returns nothing (fail open); any doubt
// about an individual process skips it (fail closed).
func (s *watcherSupervisor) findOrphanedWatchGroups() []*watchOrphanGroup {
	rawHome, err := watchMarkerHome()
	if err != nil {
		log.WarningLog.Printf("watch orphan sweep skipped: cannot resolve AF home: %v", err)
		return nil
	}
	home, err := canonicalDir(rawHome)
	if err != nil {
		log.WarningLog.Printf("watch orphan sweep skipped: cannot canonicalize AF home %q: %v", rawHome, err)
		return nil
	}
	snap, err := watchOrphanSnapshot()
	if err != nil {
		log.WarningLog.Printf("watch orphan sweep skipped: cannot read the process table: %v", err)
		return nil
	}
	self := os.Getpid()
	uid, ok := watchOrphanUID(self)
	if !ok {
		log.WarningLog.Printf("watch orphan sweep skipped: cannot determine the daemon's own uid")
		return nil
	}
	ownPGID, err := watchOrphanPGID(self)
	if err != nil {
		log.WarningLog.Printf("watch orphan sweep skipped: cannot read the daemon's own process group: %v", err)
		return nil
	}

	byGroup := make(map[int]*watchOrphanGroup)
	for pid, p := range snap {
		// Ancestors are deliberately NOT excluded (see the file comment).
		if pid <= 1 || pid == self || watchOrphanDescendsFrom(snap, pid, self) {
			continue
		}
		if procUID, ok := watchOrphanUID(pid); !ok || procUID != uid {
			continue
		}
		environ, err := watchOrphanEnviron(pid)
		if err != nil {
			// Unknown is not "unmarked" and not "ours" — the same three-valued
			// discipline as proctree.LookupEnv. Not provably ours, so skip.
			continue
		}
		gen, ok := firstEnv(environ, envWatchGeneration)
		if !ok || gen == "" || gen == s.generation {
			// Unmarked processes are never ours — this deliberately spares
			// orphans from before the marker existed, too — and our own
			// generation's watchers are live, supervised work.
			continue
		}
		procHome, ok := firstEnv(environ, "AF_HOME")
		if !ok || procHome == "" {
			continue
		}
		canonicalProcHome, err := canonicalDir(procHome)
		if err != nil || canonicalProcHome != home {
			continue
		}
		if same, err := watchOrphanSameIdentity(p); err != nil || !same {
			continue
		}
		pgid, err := watchOrphanPGID(pid)
		if err != nil || pgid <= 1 || pgid == ownPGID {
			continue
		}
		g := byGroup[pgid]
		if g == nil {
			g = &watchOrphanGroup{pgid: pgid}
			byGroup[pgid] = g
		}
		g.members = append(g.members, p)
		if id, ok := firstEnv(environ, "AF_TASK_ID"); ok && id != "" && !slices.Contains(g.taskIDs, id) {
			g.taskIDs = append(g.taskIDs, id)
		}
	}

	// Second channel: groups whose leader a previous generation recorded. Its
	// candidates merge into the same map, so the infrastructure check and the
	// TERM→KILL escalation below apply to both channels identically.
	s.collectRecordedOrphans(snap, uid, ownPGID, byGroup)

	if len(byGroup) == 0 {
		return nil
	}
	spared := watchOrphanInfraGroups(snap, byGroup)

	groups := make([]*watchOrphanGroup, 0, len(byGroup))
	for pgid, g := range byGroup {
		if reason, ok := spared[pgid]; ok {
			log.WarningLog.Printf("not reaping %s although a previous daemon generation marked it: "+
				"the group holds af infrastructure (%s)", g.describe(), reason)
			continue
		}
		sort.Strings(g.taskIDs)
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].pgid < groups[j].pgid })
	return groups
}

// watchOrphanInfraGroups returns, for each candidate group that holds af
// infrastructure, why it must be spared (see the file comment). It enumerates
// EVERY snapshot member of each candidate group, marked or not. Group
// membership is closed — no process can join a group in another session — so a
// check at collection time suffices: the kill window can only lose members.
func watchOrphanInfraGroups(snap map[int]proctree.Process, candidates map[int]*watchOrphanGroup) map[int]string {
	afComm := ""
	if exe, err := watchOrphanExecutable(); err == nil {
		afComm = filepath.Base(exe)
	}
	spared := make(map[int]string)
	for pid, p := range snap {
		pgid, err := watchOrphanPGID(pid)
		if err != nil || candidates[pgid] == nil {
			continue
		}
		if _, done := spared[pgid]; done {
			continue
		}
		if reason, ok := watchOrphanInfraReason(p, afComm); ok {
			spared[pgid] = reason
		}
	}
	return spared
}

// watchOrphanInfraReason reports whether p is af infrastructure that no orphan
// sweep may signal.
//
// The comm checks are a refusal heuristic, not ownership matching: they can
// only spare a group the markers already proved ours, never condemn one. A
// tmux server's comm is "tmux" or "tmux: server". An af binary counts only as
// a SESSION LEADER: startDaemonChild Setsids the ad-hoc daemon, so it leads its
// own session, while a transient `af` CLI call inside a watch script keeps the
// script's session and must not shield an orphaned watch command forever.
// A member whose environ cannot be read does not disqualify the group: the
// marked-member gate already restricts signaling to verified groups.
func watchOrphanInfraReason(p proctree.Process, afComm string) (string, bool) {
	if strings.HasPrefix(p.Comm, "tmux") {
		return fmt.Sprintf("tmux server pid %d", p.PID), true
	}
	if afComm != "" && p.SID == p.PID && watchOrphanCommMatches(p.Comm, afComm) {
		return fmt.Sprintf("af process pid %d leading its own session", p.PID), true
	}
	if environ, err := watchOrphanEnviron(p.PID); err == nil {
		if session, ok := firstEnv(environ, "AF_SESSION"); ok && session != "" {
			return fmt.Sprintf("pid %d belongs to af session %s", p.PID, session), true
		}
	}
	return "", false
}

// watchOrphanCommMatches compares a kernel comm against an executable
// basename. The kernel truncates comm — to 15 bytes on Linux (TASK_COMM_LEN-1),
// 16 on darwin (MAXCOMLEN) — so a longer basename matches on a truncated prefix
// of at least 15 bytes.
func watchOrphanCommMatches(comm, base string) bool {
	const commMin = 15
	if comm == base {
		return true
	}
	return len(base) > commMin && len(comm) >= commMin && strings.HasPrefix(base, comm)
}

// stopOrphanedWatchGroups SIGTERMs every group in one batch, waits for all of
// them against one shared deadline (s.stopGrace), and SIGKILLs the ones whose
// marked members are still alive at that deadline.
func (s *watcherSupervisor) stopOrphanedWatchGroups(groups []*watchOrphanGroup) {
	var pending []*watchOrphanGroup
	for _, g := range groups {
		// Re-verify right before signaling: the snapshot is evidence about
		// the past, and a group whose marked members are all gone no longer
		// licenses a signal to whatever holds that pgid now.
		if !g.anyMemberAlive() {
			continue
		}
		err := watchOrphanKillGroup(g.pgid, syscall.SIGTERM)
		switch {
		case err == nil:
			pending = append(pending, g)
		case errors.Is(err, syscall.ESRCH):
			log.InfoLog.Printf("orphaned watch command from a previous daemon generation already exited: %s", g.describe())
		default:
			log.WarningLog.Printf("cannot stop orphaned watch command %s: SIGTERM: %v", g.describe(), err)
		}
	}

	deadline := time.Now().Add(s.stopGrace)
	for len(pending) > 0 && time.Now().Before(deadline) {
		pending = s.pruneStoppedWatchGroups(pending)
		if len(pending) > 0 {
			watchOrphanSleep(watchOrphanPoll)
		}
	}
	pending = s.pruneStoppedWatchGroups(pending)

	for _, g := range pending {
		if !g.anyMemberAlive() {
			// The marked members are gone but something still holds the
			// pgid — an unmarked escapee, or a pgid already reused. Neither is
			// provably ours, so the group gets no SIGKILL (fail closed).
			log.WarningLog.Printf("orphaned watch command %s: group outlived its marked members; "+
				"refusing SIGKILL on an unverifiable group", g.describe())
			continue
		}
		if err := watchOrphanKillGroup(g.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			log.WarningLog.Printf("cannot stop orphaned watch command %s: SIGKILL: %v", g.describe(), err)
			continue
		}
		killDeadline := time.Now().Add(watchOrphanKillWait)
		for g.anyMemberAlive() && time.Now().Before(killDeadline) {
			watchOrphanSleep(watchOrphanPoll)
		}
		if g.anyMemberAlive() {
			log.WarningLog.Printf("orphaned watch command %s survived SIGKILL for %s", g.describe(), watchOrphanKillWait)
			continue
		}
		log.InfoLog.Printf("reaped orphaned watch command from a previous daemon generation with SIGKILL "+
			"after it ignored SIGTERM for %s: %s", s.stopGrace, g.describe())
	}
}

// pruneStoppedWatchGroups drops every group that has fully exited, logging the
// reap, and returns the ones still holding processes. A group whose marked
// members died but whose pgid is still occupied stays pending until the
// deadline: that is usually zombies awaiting their new parent's wait(2), and
// the final verdict on it belongs to the escalation step, which refuses to
// SIGKILL it.
func (s *watcherSupervisor) pruneStoppedWatchGroups(pending []*watchOrphanGroup) []*watchOrphanGroup {
	kept := pending[:0]
	for _, g := range pending {
		alive, err := watchOrphanGroupAlive(g.pgid)
		if err != nil {
			log.WarningLog.Printf("cannot confirm orphaned watch command %s stopped: %v", g.describe(), err)
			continue
		}
		if !alive {
			log.InfoLog.Printf("reaped orphaned watch command from a previous daemon generation: %s", g.describe())
			continue
		}
		kept = append(kept, g)
	}
	return kept
}

// watchOrphanDescendsFrom reports whether root appears in pid's PPID chain.
// This generation's own descendants are never orphans, whatever they carry.
func watchOrphanDescendsFrom(snap map[int]proctree.Process, pid, root int) bool {
	seen := make(map[int]bool)
	for range 256 {
		p, ok := snap[pid]
		if !ok || p.PPID <= 0 || seen[p.PPID] {
			return false
		}
		if p.PPID == root {
			return true
		}
		seen[p.PPID] = true
		pid = p.PPID
	}
	return false
}
