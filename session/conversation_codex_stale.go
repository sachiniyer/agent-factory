package session

import (
	"errors"
	"iter"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// staleRecordedCodexDecline answers whether the recorded Codex conversation
// still names the pane's conversation: the zero decline when af can prove it
// does or when there is nothing newer to compare, a refusal when the rollout
// is missing, a newer same-worktree top-level thread exists, or the newer set
// cannot be correlated either way. Fail closed: "we cannot prove current"
// and "provably stale" both refuse, because silently resuming the wrong
// thread is the bug this guard exists to kill. The two are typed apart
// (resumeDecline.unverified), which decides whether an operator's repair is
// retired.
//
// An operator's repair (conv's kind is ConversationCaptureOperator) is
// judged the same way with one difference, and only after its provenance is
// re-checked (#7122): a newer thread the operator chose over never makes it
// stale (operatorRepairWindow.disregards). Any other — a thread whose rollout
// appeared after the repair, or one the pane moved to since — does, whether or
// not the follow check saw the pane move: the rule reads only the record and
// the store, so it holds across a daemon restart or a blind follow alike.
func (i *Instance) staleRecordedCodexDecline(program string, conv AgentConversationData) resumeDecline {
	workDir := i.GetWorktreePath()
	home, err := i.codexHomeForLaunch(program, workDir)
	if err != nil || strings.TrimSpace(home) == "" {
		return declineResumeUnverified("af cannot resolve the Codex home this session writes to")
	}
	// Codex moves a thread between forms as it works: it compresses a rollout
	// left idle and writes a compressed one back out when it is resumed, each
	// time publishing the new file before removing the old. The file af chose
	// can therefore be gone when it is dated although the thread never was,
	// so a vanished rollout gets one more look before it reads as missing
	// (#7134 review F-3).
	decline, gone := i.judgeRecordedCodexRollout(home, program, workDir, conv)
	if gone {
		decline, gone = i.judgeRecordedCodexRollout(home, program, workDir, conv)
	}
	if gone {
		return declineResume(codexRolloutMissingReason)
	}
	return decline
}

// recordedCodexRolloutLocatedHook is a test seam fired with the file that
// stands for the recorded thread once judgeRecordedCodexRollout has found it
// and before it dates it, so a test can move the thread between forms at that
// instant. Nil in production.
var recordedCodexRolloutLocatedHook func(path string)

// judgeRecordedCodexRollout is one look at the store for
// staleRecordedCodexDecline: the decline, or gone when the file found for the
// recorded thread vanished before it could be dated.
func (i *Instance) judgeRecordedCodexRollout(home, program, workDir string, conv AgentConversationData) (resumeDecline, bool) {
	files, err := codexRolloutFiles(home)
	if err != nil {
		return declineResumeUnverified("af could not verify the recorded conversation against the Codex home this session uses"), false
	}
	var recordedPath string
	for file := range files {
		if strings.EqualFold(codexConversationIDFromPath(file), conv.ID) {
			if recordedPath != "" {
				// Two rollouts claim the same id: "current" cannot be proven,
				// now or later.
				return declineResume(codexRolloutDuplicatedReason), false
			}
			recordedPath = file
		}
	}
	if recordedPath == "" {
		// Not missing while Codex still resumes it compressed or from a
		// thread/revert rollout (#7130).
		path, decline, unlooked := recordedCodexRolloutInAnotherForm(home, conv.ID)
		switch {
		case unlooked:
			// The thread is listed in another form, but a lookup of one did
			// not answer, or failed, so af cannot date it: nothing can be
			// attributed against it, which is the no-evidence resume (#7378).
			return resumeDecline{}, false
		case decline.reason != "":
			return decline, false
		}
		recordedPath = path
	}
	if hook := recordedCodexRolloutLocatedHook; hook != nil {
		hook(recordedPath)
	}
	launchDir := LaunchCaptureWorkingDir(program, workDir)
	// noEvidence is the guard's answer when nothing newer can be attributed to
	// the session: resume the recorded thread, after the second look at a
	// rollout that may have moved between forms.
	// The lstat is bounded (fsprobe, #7378), and only a proven absence is
	// "gone": one that did not answer, or failed, cannot say the rollout
	// moved, so it is not looked for again and declined as missing.
	noEvidence := func() (resumeDecline, bool) {
		if _, err := fsprobe.Lstat(recordedPath); pathutil.IsAbsent(err) {
			return resumeDecline{}, true
		}
		return resumeDecline{}, false
	}
	var repair operatorRepairWindow
	if conv.CaptureKind == ConversationCaptureOperator {
		switch decline := operatorRepairProvenance(recordedPath, launchDir); {
		case decline.provenanceUnanswered():
			// A provenance lookup that did not answer is no answer about the
			// repair: the attribution timeout's answer, which resumes the
			// recorded thread and retires nothing. Only an answered provenance
			// declines (#7287 review R5, #7370 review R3).
			return noEvidence()
		case decline.reason != "":
			return decline, false
		}
		repair = operatorRepairWindowOf(conv, time.Now())
	}
	// A newer thread counts against the record only where af owns the
	// directory the agent runs in exclusively (newerConversationIsEvidence,
	// #7210): in the user's own checkout their own codex, or a sibling
	// in-place session, writes threads with the same cwd. Whether the
	// recorded thread is missing, duplicated or someone else's is evidence
	// about the id itself, judged above and here, where the rollout af chose
	// may have gone since the walk: that still takes the second look.
	if !i.newerConversationIsEvidence(launchDir) {
		return noEvidence()
	}
	switch checkCodexRecordedStaleness(recordedPath, files, boundedLaunchDir(launchDir), repair) {
	case codexRolloutStale:
		return declineResume(staleCarryReason), false
	case codexRolloutUncorrelated:
		// A newer rollout whose header af cannot classify yet — Codex may be
		// mid-write — is a look that failed, not evidence about the id.
		return declineResumeUnverified("af could not confirm the recorded conversation is still the newest one in this worktree"), false
	case codexRolloutUnresolved:
		// A lookup that did not answer within its bound — the launch dir's, a
		// newer rollout's cwd, or a header read a newer rollout needed —
		// attributes nothing: the no-evidence resume the attribution rule's
		// own timeout gives above. Only a look that answered declines (#7287
		// review R1, #7370 review R2, R3).
		return resumeDecline{}, false
	case codexRolloutRecordedGone:
		return resumeDecline{}, true
	}
	return resumeDecline{}, false
}

// boundedLaunchDir resolves launchDir for correlating rollouts' cwds, bounded
// (fsprobe, #7279), when a staleness check first needs it. ok is false when
// the resolution did not answer within the bound.
func boundedLaunchDir(launchDir string) func() (string, bool) {
	return func() (string, bool) {
		resolved, err := fsprobe.ResolveForCompare(launchDir)
		return resolved, err == nil
	}
}

// codexStalenessVerdict is the outcome of comparing a recorded rollout against
// the newer files in its store.
type codexStalenessVerdict int

const (
	codexRolloutCurrent codexStalenessVerdict = iota
	codexRolloutStale
	codexRolloutUncorrelated
	codexRolloutRecordedGone
	// codexRolloutUnresolved: a lookup a newer rollout's judgement needed —
	// the launch dir's, the rollout's own cwd, or its header read — did not
	// answer within its bound. Nothing newer was attributed either way.
	codexRolloutUnresolved
)

// checkCodexRecordedStaleness is the mechanical half shared by the restore
// guard and the carry staleness check: stat the recorded rollout, keep the
// enumerated files strictly newer than it, and correlate those to the launch
// dir resolveLaunch names — an empty one leaves every newer top-level file a
// candidate, which the callers' fail-closed reading treats correctly. The
// launch dir is resolved only once a newer file exists, so a check with
// nothing newer never waits on its mount (#7287 review R3). files is the
// store listing the caller already took, so a check never walks the store
// twice.
//
// repair is an operator repair's window (operatorRepairWindowOf): a newer
// file it disregards is a thread the operator chose over and is dropped; any
// other, including one whose header af read but cannot date, stays a candidate
// exactly as it would for any record. The zero window drops nothing.
//
// A newer file whose header the window needed and could not read is neither:
// whether the operator overrode it is unknown. It is counted the way the
// correlation counts the same read — uncorrelated, and unresolved when the
// read did not answer within its bound — and never read again, so a read that
// answers a moment later cannot make a thread af could not date the evidence
// that the repair is stale (#7370 review R2). Stale needs a newer file every
// read of which answered.
func checkCodexRecordedStaleness(recordedPath string, files map[string]struct{}, resolveLaunch func() (string, bool), repair operatorRepairWindow) codexStalenessVerdict {
	return judgeCodexRecordedStaleness(recordedPath, lstatRolloutModTimes(files), resolveLaunch, repair)
}

// rolloutModTime is a listed rollout's mtime, or the lookup error that left it
// unread.
type rolloutModTime struct {
	at     time.Time
	unread error
}

// lstatRolloutModTimes yields each of files with its mtime now. Each lstat is
// bounded (fsprobe, #7378). A rollout gone since the listing is skipped. One
// whose lstat failed, or did not answer within its bound, is yielded with that
// error, since it may be newer. The first that did not answer ends the walk:
// the rest are on the same mount, and each would wait out a bound of its own.
func lstatRolloutModTimes(files map[string]struct{}) iter.Seq2[string, rolloutModTime] {
	return func(yield func(string, rolloutModTime) bool) {
		for path := range files {
			info, err := fsprobe.Lstat(path)
			var modTime rolloutModTime
			switch {
			case err == nil:
				modTime.at = info.ModTime()
			case pathutil.IsAbsent(err):
				continue
			default:
				modTime.unread = err
			}
			if !yield(path, modTime) || errors.Is(err, fsprobe.ErrProbeTimedOut) {
				return
			}
		}
	}
}

// judgeCodexRecordedStaleness is checkCodexRecordedStaleness over rollouts
// whose mtimes are already known — a snapshot of the store (#7156 review
// P2-1). The recorded rollout itself is always stat'd now, bounded (fsprobe,
// #7378): only a proven absence is gone, and one that did not answer, or
// failed, leaves it undatable, so nothing newer is attributed either way.
func judgeCodexRecordedStaleness(recordedPath string, rollouts iter.Seq2[string, rolloutModTime], resolveLaunch func() (string, bool), repair operatorRepairWindow) codexStalenessVerdict {
	recorded, err := fsprobe.Lstat(recordedPath)
	switch {
	case pathutil.IsAbsent(err):
		return codexRolloutRecordedGone
	case err != nil:
		return codexRolloutUnresolved
	}
	var newer []string
	// undated counts the newer files af could not judge: a header the
	// repair's window could not read, or an mtime af could not read at all.
	// undatedUnresolved counts those whose lookup did not happen: a header read
	// that did not answer, or an lstat that did not answer or failed.
	undated, undatedUnresolved := 0, 0
	for path, rollout := range rollouts {
		if rollout.unread != nil {
			// It may be newer: a lookup that did not happen (#7378). That
			// holds for the recorded rollout's own entry too, since a walk
			// that stopped there did not see the rest.
			undated++
			undatedUnresolved++
			continue
		}
		if path == recordedPath {
			continue
		}
		modTime := rollout.at
		if !modTime.After(recorded.ModTime()) {
			continue
		}
		disregarded, unread := repair.disregards(path, modTime)
		switch {
		case unread != nil:
			undated++
			if codexHeaderReadUnanswered(unread) {
				undatedUnresolved++
			}
		case !disregarded:
			newer = append(newer, path)
		}
	}
	uncorrelated, unresolved := undated, undatedUnresolved
	if len(newer) > 0 {
		launchDir, ok := resolveLaunch()
		if !ok {
			return codexRolloutUnresolved
		}
		matches, u, r, _ := classifyCodexRolloutFiles(newer, launchDir)
		if len(matches) > 0 {
			return codexRolloutStale
		}
		uncorrelated, unresolved = uncorrelated+u, unresolved+r
	}
	switch {
	case uncorrelated > unresolved:
		// A header af could not classify: Codex may still be writing it.
		return codexRolloutUncorrelated
	case unresolved > 0:
		return codexRolloutUnresolved
	}
	return codexRolloutCurrent
}
