package session

import (
	"iter"
	"os"
	"strings"
	"time"

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
		path, decline := recordedCodexRolloutInAnotherForm(home, conv.ID)
		if decline.reason != "" {
			return decline, false
		}
		recordedPath = path
	}
	if hook := recordedCodexRolloutLocatedHook; hook != nil {
		hook(recordedPath)
	}
	launchDir := LaunchCaptureWorkingDir(program, workDir)
	var repair operatorRepairWindow
	if conv.CaptureKind == ConversationCaptureOperator {
		if decline := operatorRepairProvenance(recordedPath, launchDir); decline.reason != "" {
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
		if _, err := os.Lstat(recordedPath); err != nil {
			return resumeDecline{}, true
		}
		return resumeDecline{}, false
	}
	switch checkCodexRecordedStaleness(recordedPath, files, pathutil.ResolveForCompare(launchDir), repair) {
	case codexRolloutStale:
		return declineResume(staleCarryReason), false
	case codexRolloutUncorrelated:
		// A newer rollout whose header af cannot classify yet — Codex may be
		// mid-write — is a look that failed, not evidence about the id.
		return declineResumeUnverified("af could not confirm the recorded conversation is still the newest one in this worktree"), false
	case codexRolloutRecordedGone:
		return resumeDecline{}, true
	}
	return resumeDecline{}, false
}

// codexStalenessVerdict is the outcome of comparing a recorded rollout against
// the newer files in its store.
type codexStalenessVerdict int

const (
	codexRolloutCurrent codexStalenessVerdict = iota
	codexRolloutStale
	codexRolloutUncorrelated
	codexRolloutRecordedGone
)

// checkCodexRecordedStaleness is the mechanical half shared by the restore
// guard and the carry staleness check: stat the recorded rollout, keep the
// enumerated files strictly newer than it, and correlate those to launchDir —
// an empty launchDir leaves every newer top-level file a candidate, which the
// callers' fail-closed reading treats correctly. files is the store listing
// the caller already took, so a check never walks the store twice.
//
// repair is an operator repair's window (operatorRepairWindowOf): a newer
// file it disregards is a thread the operator chose over and is dropped; any
// other, including one af cannot date, stays a candidate exactly as it would
// for any record. The zero window drops nothing.
func checkCodexRecordedStaleness(recordedPath string, files map[string]struct{}, launchDir string, repair operatorRepairWindow) codexStalenessVerdict {
	return judgeCodexRecordedStaleness(recordedPath, lstatRolloutModTimes(files), launchDir, repair)
}

// lstatRolloutModTimes yields each of files with its mtime now, skipping one
// that cannot be stat'd.
func lstatRolloutModTimes(files map[string]struct{}) iter.Seq2[string, time.Time] {
	return func(yield func(string, time.Time) bool) {
		for path := range files {
			if info, err := os.Lstat(path); err == nil && !yield(path, info.ModTime()) {
				return
			}
		}
	}
}

// judgeCodexRecordedStaleness is checkCodexRecordedStaleness over rollouts
// whose mtimes are already known — a snapshot of the store (#7156 review
// P2-1). The recorded rollout itself is always stat'd now.
func judgeCodexRecordedStaleness(recordedPath string, rollouts iter.Seq2[string, time.Time], launchDir string, repair operatorRepairWindow) codexStalenessVerdict {
	recorded, err := os.Lstat(recordedPath)
	if err != nil {
		return codexRolloutRecordedGone
	}
	var newer []string
	for path, modTime := range rollouts {
		if path == recordedPath {
			continue
		}
		if modTime.After(recorded.ModTime()) && !repair.disregards(path, modTime) {
			newer = append(newer, path)
		}
	}
	if len(newer) == 0 {
		return codexRolloutCurrent
	}
	matches, uncorrelated := codexRolloutFilesForWorkingDir(newer, launchDir)
	if len(matches) > 0 {
		return codexRolloutStale
	}
	if uncorrelated > 0 {
		return codexRolloutUncorrelated
	}
	return codexRolloutCurrent
}
