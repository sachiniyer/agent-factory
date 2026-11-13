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

// unfiledRecordedClaudeReason is why a launch starts Claude on its recorded id
// rather than resuming it (recordedClaudeDecline): a clause, as the other
// decline reasons are.
const unfiledRecordedClaudeReason = "its store holds no transcript for it " +
	"(Claude files a conversation only on its first entry, and deletes old ones), so Claude starts on that id instead of resuming it"

// recordedClaudeVerdict is judgeRecordedClaude's answer about launching a
// recorded Claude id.
type recordedClaudeVerdict int

const (
	// recordedClaudeBlind: the store cannot be named or a spelling cannot be
	// listed, which says nothing about the id. Each caller keeps what it did
	// before it could look.
	recordedClaudeBlind recordedClaudeVerdict = iota
	// recordedClaudeResume: the id is filed, and no newer conversation is
	// evidence that this session moved on from it.
	recordedClaudeResume
	// recordedClaudeStartOn: the store listed cleanly and holds no transcript
	// for the id. Claude never filed it (it files lazily) or its retention
	// deleted it, and --resume of it exits at launch: start Claude on it.
	recordedClaudeStartOn
	// recordedClaudeAbandoned: a newer conversation is evidence this session
	// moved on (a /clear), and resuming would continue a thread the user left.
	recordedClaudeAbandoned
)

// judgeRecordedClaude is the one rule for launching a recorded Claude id
// (#7215): restore, Recover, the adopted sandbox relaunch, a first launch's
// record pin and a committed account swap's re-launch all judge the id by it,
// from one listing of the store the launch writes (claudeStoreFor,
// judgeClaudeRecorded) and through one ownership gate. A newer conversation is
// evidence only in a worktree af created for this session alone, where Claude
// actually runs (newerConversationIsEvidence). In the user's own checkout, an
// in-place row or an adopted worktree, a sibling files there too, so it says
// nothing about this session (#7209 review R1, #7216 review R1).
func (i *Instance) judgeRecordedClaude(program, workDir, id string) recordedClaudeVerdict {
	configDir, launchDir, err := i.claudeStoreFor(program, workDir)
	if err != nil || strings.TrimSpace(configDir) == "" || strings.TrimSpace(launchDir) == "" {
		return recordedClaudeBlind
	}
	switch judgeClaudeRecorded(configDir, claudeProjectCandidates(launchDir), id, "") {
	case claudeRecordedMissing:
		return recordedClaudeStartOn
	case claudeRecordedStale:
		if i.newerConversationIsEvidence(launchDir) {
			return recordedClaudeAbandoned
		}
		return recordedClaudeResume
	case claudeRecordedCurrent, claudeRecordedCopies:
		return recordedClaudeResume
	}
	return recordedClaudeBlind
}

// recordedClaudeDecline is judgeRecordedClaude for a restore: an id to start on
// is a startsOnRecorded decline that keeps the record, an abandoned one is
// refused as #7206's restore guard refuses it, and a blind look resumes the
// recorded id as a restore always has.
func (i *Instance) recordedClaudeDecline(program string, conv AgentConversationData) resumeDecline {
	switch i.judgeRecordedClaude(program, i.GetWorktreePath(), conv.ID) {
	case recordedClaudeStartOn:
		return resumeDecline{reason: unfiledRecordedClaudeReason, startsOnRecorded: true}
	case recordedClaudeAbandoned:
		return declineResume(staleCarryReason)
	}
	return resumeDecline{}
}
