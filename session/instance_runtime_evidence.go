// The runtime-identity EVIDENCE record: which command the last positively
// established agent runtime ran (i.runtimeProgram), the pane-root
// (pid, startID) and tmux-server identity pinned beside it at the launch
// boundary (#5066, #7469), the generation counter that lets a captured
// RuntimeProgramEvidence be validated without holding i.mu, and
// settleReattachVerification — the single probe that keeps or retires a
// surviving claim on reattach. Launch boundaries write through
// setRuntimeLaunch / setRuntimeEvidence*Locked; lock-free consumers read
// through the evidence token so a verdict can never straddle a runtime
// replacement. instance_accessors.go keeps the general accessor surface.
package session

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// RuntimeProgram returns durable evidence of the override-resolved base command
// used by the last positively established agent runtime. It is intentionally
// empty for legacy or uncertain records; Program is intent, not runtime proof.
func (i *Instance) RuntimeProgram() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.runtimeProgram
}

// RuntimeProgramEvidence binds one runtime command to the lifecycle generation
// in which it was observed. Its generation is deliberately opaque outside the
// session package; callers can only validate it through Instance.
type RuntimeProgramEvidence struct {
	program    string
	generation uint64
}

// Program returns the resolved command captured by this evidence.
func (e RuntimeProgramEvidence) Program() string { return e.program }

// ObserveRuntimeProgram captures the command and its invalidation generation
// under the same instance lock.
func (i *Instance) ObserveRuntimeProgram() RuntimeProgramEvidence {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return RuntimeProgramEvidence{
		program:    i.runtimeProgram,
		generation: i.runtimeEvidenceGeneration.Load(),
	}
}

// RuntimeProgramEvidenceCurrent reports whether no runtime-command or
// lifecycle transition has superseded evidence since it was observed. It is
// lock-free so daemon callers can validate inside Manager.mu without reversing
// the manager/instance lock order.
func (i *Instance) RuntimeProgramEvidenceCurrent(evidence RuntimeProgramEvidence) bool {
	return evidence.generation == i.runtimeEvidenceGeneration.Load()
}

// CommitRuntimeProgramEvidence runs commit only while evidence still describes
// this instance's current runtime generation. The read lock is the commit
// boundary: every lifecycle or runtime replacement that invalidates evidence
// owns i.mu for writing, so either that invalidation lands first and commit is
// refused, or it waits until commit returns.
//
// commit must not call methods that acquire i.mu. It is intended for a small
// external side effect whose truth depends on this evidence, such as emitting a
// diagnostic about the runtime command.
func (i *Instance) CommitRuntimeProgramEvidence(evidence RuntimeProgramEvidence, commit func()) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if evidence.generation != i.runtimeEvidenceGeneration.Load() {
		return false
	}
	commit()
	return true
}

// setRuntimeProgram records a command only after a launch boundary positively
// established the replacement runtime. The surrounding lifecycle transition
// owns UpdatedAt and the durable checkpoint; touching here would count one
// runtime replacement twice.
func (i *Instance) setRuntimeProgram(program string) {
	i.setRuntimeLaunch(program, nil, time.Time{})
}

// setRuntimeLaunch records a positively established launch: the resolved base
// command plus, when the pane answers, the pane root's (pid, kernel start-time)
// identity — the pair that lets a later reattach prove the process standing
// behind the reused tmux name is still the launch af made (#5066). A pane query
// that cannot answer records the command alone; the record then verifies
// nothing on reattach and ages out exactly like a pre-evidence launch.
//
// spawnedAt is the instant just before the launch spawned the process — ts's
// SpawnedAt, taken after tmux confirmed no session stood behind the name —
// or zero when it has none. It dates the replacement of the runtime a
// standing operator repair was made against (#7147 G-2).
func (i *Instance) setRuntimeLaunch(program string, ts *tmux.TmuxSession, spawnedAt time.Time) {
	if strings.TrimSpace(program) == "" {
		return
	}
	pid := 0
	var startID uint64
	if ts != nil {
		if pane, err := ts.PaneRootProcess(); err == nil {
			pid, startID = pane.PID, pane.StartID
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setRuntimeEvidenceSinceLocked(program, pid, startID, spawnedAt)
	// The same pane answer carried the tmux-server identity (#7469): record
	// it beside the root so a daemon restart restores what the
	// vanished-session sweep attributes detached carriers against. The
	// launch boundary already retired the standing fields inside
	// setRuntimeEvidenceSinceLocked — what lands here can only be the new
	// incarnation's own stamp.
	if ts != nil {
		if server, ok := ts.ServerEvidence(); ok {
			i.runtimeServerSocket = server.Socket
			i.runtimeServerPID = server.PID
			i.runtimeServerStartID = server.StartID
			i.runtimeServerSessionID = server.SessionID
			i.runtimeServerPIDNS = server.PIDNS
		}
	}
}

func (i *Instance) setRuntimeProgramLocked(program string) {
	i.setRuntimeEvidenceLocked(program, 0, 0)
}

func (i *Instance) setRuntimeEvidenceLocked(program string, pid int, startID uint64) {
	i.setRuntimeEvidenceSinceLocked(program, pid, startID, time.Time{})
}

func (i *Instance) setRuntimeEvidenceSinceLocked(program string, pid int, startID uint64, spawnedAt time.Time) {
	// Every caller is a launch boundary: whatever it records — even nothing
	// beyond the command, when its pane query failed — describes the new
	// runtime, so an identity kept from the old one's unanswerable reattach
	// probe can never stand in for it (#7123). A standing operator repair
	// records that its runtime was replaced (#7129). The tmux-server record
	// retires at the same boundary (#7469 R1): a launch whose pane query
	// then fails must persist ABSENT, never the previous incarnation's
	// identity — a stale (socket, old server pid) would read the
	// replacement's own survivors foreign. The re-stamp, when a pane
	// answered, lands in setRuntimeLaunch just below.
	i.runtimeServerSocket = ""
	i.runtimeServerPID = 0
	i.runtimeServerStartID = 0
	i.runtimeServerSessionID = ""
	i.runtimeServerPIDNS = ""
	i.captureEvidence = keptCaptureEvidence{}
	i.markRepairRuntimeReplacedLocked(launchReplacementInstant(spawnedAt, time.Now()))
	if i.runtimeProgram == program && i.runtimePID == pid && i.runtimeStartID == startID {
		return
	}
	// Invalidate lock-free consumers before publishing the replacement value.
	// A consumer may validate while holding a different owner lock and therefore
	// cannot take i.mu to close this ordering edge.
	i.runtimeEvidenceGeneration.Add(1)
	i.runtimeProgram = program
	i.runtimePID = pid
	i.runtimeStartID = startID
}

// settleReattachVerification probes the pane behind the reused tmux name ONCE
// and settles the recorded runtime claim to match the answer: a positive
// (pid, start-time) match keeps the claim and reports verified; anything else
// — a different process, an unanswerable probe, a record carrying no identity
// — is unverified, and the persisted claim is retired if there was one. Both
// answers come from the SAME probe so the task-run verdict and the evidence
// clear can never disagree about which pane answered (#4222 review: two
// separate probes let a pane respawned between them read verified to the
// interruption classifier while the clear saw the mismatch).
//
// verified answers the task-run question on the Lost-restore path — a pane
// that appears while the session was believed dead and cannot prove it is the
// prompted agent is treated as a replacement. claimRetired is the load path's
// deliberately narrower predicate: a tmux name surviving a routine daemon
// restart is the EXPECTED same-runtime case, so only a recorded claim that
// failed verification marks a replacement there — a row carrying none (a
// legacy record) has nothing to distrust and stays a pure reattach.
//
// Both verdicts, and the retire every reader of runtimePID sees, are the same
// whether the probe answered "no pane is rooted at that identity" or could
// not answer at all. Conversation capture alone tells them apart (#7123): an
// answered mismatch is evidence of a replacement and keeps nothing, while an
// unanswerable probe is no evidence either way, so its identity is kept as
// captureEvidence for CheckConversationCaptureEvidence to re-verify later with
// the same session-wide scan. A standing operator repair tells them apart
// the same way: an answered mismatch records that the runtime the repair was
// made against was replaced, exactly as an af launch does (#7132 review F-2),
// because the next launch's resume decision runs before that launch records
// itself.
func (i *Instance) settleReattachVerification(ts *tmux.TmuxSession) (verified, claimRetired bool) {
	i.mu.RLock()
	oldProgram, pid, startID := i.runtimeProgram, i.runtimePID, i.runtimeStartID
	i.mu.RUnlock()
	unanswered := false
	if pid > 0 && ts != nil {
		same, err := ts.PaneRootProcessMatches(pid, startID)
		if err == nil && same {
			// Provably the same launch: keep the claim.
			return true, false
		}
		unanswered = err != nil && !tmux.PaneQueryFoundNoPane(err)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	// Re-check under the write lock: a launch boundary that committed fresh
	// evidence while the pane query ran must not lose it to this verdict.
	if i.runtimeProgram != oldProgram || i.runtimePID != pid || i.runtimeStartID != startID {
		return false, false
	}
	// A kept capture identity is trusted no longer than launch evidence would
	// be, and launch evidence is re-proven at every reattach: the next capture
	// attempt re-scans it (#7131). Counting the settle also voids any scan
	// already in flight, whatever its answer — it began before this reattach
	// (#7140).
	i.captureEvidence.verified = false
	i.captureSettles++
	if i.runtimeProgram == "" && i.runtimePID == 0 {
		// No launch claim to settle.
		return false, false
	}
	i.runtimeEvidenceGeneration.Add(1)
	i.runtimeProgram = ""
	i.runtimePID = 0
	i.runtimeStartID = 0
	switch {
	case unanswered:
		// tmux never answered, so nothing was DISPROVEN: the claim's pair
		// stays as capture evidence, and the anchor it seeded in the
		// TmuxSession stays too — a restore that timed out must not lose its
		// tier-2 root to a query that proved nothing (#7435 R17).
		i.captureEvidence = keptCaptureEvidence{pid: pid, startID: startID}
	case pid > 0 && ts != nil:
		// The seed this claim produced retires with it (#7435 R11/R17):
		// a restore primed the TmuxSession's vanished-sweep anchor from
		// this exact pair, and tmux just ANSWERED that it is not this
		// session's root — keeping it would let tier 2 anchor on a
		// disproven root.
		ts.RetirePaneRootEvidence(pid, startID)
		i.markRepairRuntimeReplacedLocked(time.Now())
	}
	return false, true
}
