package session

import (
	"os"
	"path/filepath"
	"strings"
)

// claudeRecordedVerdict is judgeClaudeRecorded's answer about a recorded
// Claude conversation.
type claudeRecordedVerdict int

const (
	// claudeRecordedCurrent: no other conversation in the worktree is newer.
	claudeRecordedCurrent claudeRecordedVerdict = iota
	// claudeRecordedStale: another conversation in the worktree is newer, so
	// the user moved on from the recorded one (a /clear, say).
	claudeRecordedStale
	// claudeRecordedCopies: a copy of the recorded conversation under another
	// spelling is newer than the copy the caller is bound to.
	claudeRecordedCopies
	// claudeRecordedMissing: no copy of the recorded conversation is there.
	claudeRecordedMissing
	// claudeRecordedUnlisted: a spelling's project directory could not be
	// listed, so what it holds is unknown.
	claudeRecordedUnlisted
	// claudeRecordedUncorrelated: a newer conversation names no directory af
	// can read, so whether it is this worktree's is unknown (#7208).
	claudeRecordedUncorrelated
	// claudeRecordedFiledUnlisted: a spelling could not be listed, but one af
	// could list files the recorded conversation, so it exists; whether it is
	// still the newest is unknown (#7221 review R5).
	claudeRecordedFiledUnlisted
)

// judgeClaudeRecorded is the one staleness rule for a recorded Claude
// conversation, shared by the account swap's carry and the restore guard: is
// conversation id still the newest one in the worktree? The worktree's
// conversations are every one filed in configDir under any project directory
// in projects, the spellings claudeProjectCandidates names for it (#7177),
// whose recorded cwd is launchDir (#7208): a project directory can hold
// another directory's conversations under a colliding name, and a newer one
// of those says nothing about this worktree. Newer is by modification time,
// strictly: the live runtime appends to the conversation it is in.
//
// bound, relative to configDir, is the project directory whose copy of the
// conversation the caller would use, as a carry copies one file. "" judges
// the newest copy under any spelling, which is the one a resume by id
// continues.
func judgeClaudeRecorded(configDir string, projects []string, launchDir, id, bound string) claudeRecordedVerdict {
	transcripts, err := claudeWorktreeTranscripts(configDir, projects)
	if err != nil {
		// What an unlistable spelling holds is unknown, but the recorded
		// conversation may be filed under one af could list.
		for _, transcript := range transcripts {
			if strings.EqualFold(transcript.id, id) {
				return claudeRecordedFiledUnlisted
			}
		}
		return claudeRecordedUnlisted
	}
	boundDir := ""
	if bound != "" {
		boundDir = filepath.Join(configDir, bound)
	}
	var recorded *claudeTranscript
	for idx := range transcripts {
		transcript := &transcripts[idx]
		if !strings.EqualFold(transcript.id, id) {
			continue
		}
		switch {
		case boundDir != "":
			if transcript.dir == boundDir {
				recorded = transcript
			}
		case recorded == nil || transcript.modTime.After(recorded.modTime):
			recorded = transcript
		}
	}
	if recorded == nil {
		return claudeRecordedMissing
	}
	verdict := claudeRecordedCurrent
	uncorrelated := false
	for _, other := range transcripts {
		if !other.modTime.After(recorded.modTime) {
			continue
		}
		if other.dir != recorded.dir && strings.EqualFold(other.id, recorded.id) {
			// A newer copy the judged one still holds every byte of brings no
			// turn it lacks; only one that adds or diverges does.
			if !claudeTranscriptExtends(*recorded, other) {
				verdict = claudeRecordedCopies
			}
			continue
		}
		// Newer is never the recorded file itself, so a newer file in its own
		// directory, or under another id, is another conversation: this
		// worktree's only if its transcript records this launch directory.
		switch claudeTranscriptOwnership(other, launchDir) {
		case claudeOwnedHere:
			return claudeRecordedStale
		case claudeOwnershipUnknown:
			uncorrelated = true
		}
	}
	if uncorrelated {
		return claudeRecordedUncorrelated
	}
	return verdict
}

// claudeCarryStaleness is a Claude carry's staleness check: whether the copy
// of conversation id the carry would take, transcript (relative to configDir,
// under one of projects), is still the newest conversation in the worktree.
// The carry binds the copy that holds every other one (locateClaude), so a
// newer copy under another spelling means Claude wrote it while af planned:
// that refuses too, rather than carry a copy without the latest turns.
func claudeCarryStaleness(configDir string, projects []string, launchDir, transcript, id string) string {
	switch judgeClaudeRecorded(configDir, projects, launchDir, id, filepath.Dir(transcript)) {
	case claudeRecordedUnlisted, claudeRecordedFiledUnlisted:
		return carryTranscriptsUnlistedReason
	case claudeRecordedUncorrelated:
		return carryUncorrelatedReason
	case claudeRecordedMissing:
		return carryTranscriptMissingReason
	case claudeRecordedStale:
		return staleCarryReason
	case claudeRecordedCopies:
		return carryTranscriptCopiesReason
	}
	return ""
}

// staleRecordedClaudeDecline is the restore guard for a recorded Claude
// conversation (#7206): a restore resumes it only while it is still the newest
// conversation in the worktree, by the account swap's own rule
// (judgeClaudeRecorded). A /clear leaves the recorded id naming the
// conversation the user abandoned, and resuming that is worse than a stated
// fresh start.
//
// It refuses on evidence only: a newer conversation, launched here by the cwd
// its transcript records, in a directory af owns exclusively and where Claude
// actually runs (newerConversationIsEvidence). A row in the user's own
// checkout or launched outside its worktree, a store af cannot name, a
// spelling it cannot list, a newer conversation that names no directory, and
// a recorded transcript af does not find all say nothing about whether this
// session moved on, so the restore resumes the recorded id as it always has
// (#7209 review R1, R4).
func (i *Instance) staleRecordedClaudeDecline(program string, conv AgentConversationData) resumeDecline {
	configDir, launchDir, err := i.claudeStoreFor(program, i.GetWorktreePath())
	if err != nil || strings.TrimSpace(configDir) == "" || !i.newerConversationIsEvidence(launchDir) {
		return resumeDecline{}
	}
	if judgeClaudeRecorded(configDir, claudeProjectCandidates(launchDir), launchDir, conv.ID, "") == claudeRecordedStale {
		return declineResume(staleCarryReason)
	}
	return resumeDecline{}
}

// claudeTranscriptExtends reports whether the transcript longer begins with
// every byte of shorter. One af cannot read extends nothing.
func claudeTranscriptExtends(longer, shorter claudeTranscript) bool {
	long, err := os.Open(filepath.Join(longer.dir, longer.name))
	if err != nil {
		return false
	}
	defer long.Close()
	short, err := os.Open(filepath.Join(shorter.dir, shorter.name))
	if err != nil {
		return false
	}
	defer short.Close()
	longInfo, err := long.Stat()
	if err != nil {
		return false
	}
	shortInfo, err := short.Stat()
	if err != nil || longInfo.Size() < shortInfo.Size() {
		return false
	}
	if os.SameFile(longInfo, shortInfo) {
		return true
	}
	same, err := carryPrefixMatches(long, short, shortInfo.Size())
	return err == nil && same
}
