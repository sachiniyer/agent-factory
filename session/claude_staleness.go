package session

import (
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
)

// judgeClaudeRecorded is the one staleness rule for a recorded Claude
// conversation, shared by the account swap's carry and the restore guard: is
// conversation id still the newest one in the worktree? The worktree's
// conversations are every one filed in configDir under any project directory
// in projects, the spellings claudeProjectCandidates names for it (#7177).
// Newer is by modification time, strictly: the live runtime appends to the
// conversation it is in.
//
// bound, relative to configDir, is the project directory whose copy of the
// conversation the caller would use, as a carry copies one file. "" judges
// the newest copy under any spelling, which is the one a resume by id
// continues.
func judgeClaudeRecorded(configDir string, projects []string, id, bound string) claudeRecordedVerdict {
	transcripts, err := claudeWorktreeTranscripts(configDir, projects)
	if err != nil {
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
	for _, other := range transcripts {
		if !other.modTime.After(recorded.modTime) {
			continue
		}
		// Newer is never the recorded file itself, so a newer file in its own
		// directory is another conversation.
		if other.dir == recorded.dir || !strings.EqualFold(other.id, recorded.id) {
			return claudeRecordedStale
		}
		verdict = claudeRecordedCopies
	}
	return verdict
}

// claudeCarryStaleness is a Claude carry's staleness check: whether the copy
// of conversation id the carry would take, transcript (relative to configDir,
// under one of projects), is still the newest conversation in the worktree. A
// newer copy of the same conversation under another spelling refuses too: the
// carry would take the older copy, without the latest turns.
func claudeCarryStaleness(configDir string, projects []string, transcript, id string) string {
	switch judgeClaudeRecorded(configDir, projects, id, filepath.Dir(transcript)) {
	case claudeRecordedUnlisted:
		return carryTranscriptsUnlistedReason
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
// It refuses on evidence only: a newer conversation in a directory af owns
// exclusively (newerConversationIsEvidence). A row in the user's own checkout,
// a store af cannot name, a spelling it cannot list, and a recorded transcript
// it does not find all say nothing about whether this session moved on, so the
// restore resumes the recorded id as it always has (#7209 review R1, R4).
func (i *Instance) staleRecordedClaudeDecline(program string, conv AgentConversationData) resumeDecline {
	if !i.newerConversationIsEvidence() {
		return resumeDecline{}
	}
	configDir, launchDir, err := i.claudeStoreFor(program, i.GetWorktreePath())
	if err != nil || strings.TrimSpace(configDir) == "" {
		return resumeDecline{}
	}
	if judgeClaudeRecorded(configDir, claudeProjectCandidates(launchDir), conv.ID, "") == claudeRecordedStale {
		return declineResume(staleCarryReason)
	}
	return resumeDecline{}
}
