package session

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
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
	// claudeRecordedVerdicts counts the verdicts above. A new verdict goes
	// before it, and TestEveryClaudeRecordedVerdictMapsToALaunch requires
	// recordedClaudeLaunchFor to map it.
	claudeRecordedVerdicts
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
		// A newer copy the judged one still holds every byte of brings no
		// turn it lacks; only one that adds or diverges does.
		if !claudeTranscriptExtends(*recorded, other) {
			verdict = claudeRecordedCopies
		}
	}
	return verdict
}

// claudeCarryStaleness is a Claude carry's staleness check: whether the copy
// of conversation id the carry would take, transcript (relative to configDir,
// under one of projects), is still the newest conversation in the worktree.
// The carry binds the copy that holds every other one (locateClaude), so a
// newer copy under another spelling means Claude wrote it while af planned:
// that refuses too, rather than carry a copy without the latest turns.
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
	projects := claudeProjectCandidates(launchDir)
	verdict := judgeClaudeRecorded(configDir, projects, id, "")
	if verdict == claudeRecordedMissing {
		verdict = claudeRecordedAnywhere(configDir, id)
	}
	launch, ok := recordedClaudeLaunchFor(verdict, func() bool { return i.newerConversationIsEvidence(launchDir) })
	if !ok {
		log.ErrorLog.Printf("session %q: recorded claude conversation %s: verdict %d maps to no launch; resuming it", i.Title, id, verdict)
	}
	return launch
}

// recordedClaudeLaunchFor maps each judgeClaudeRecorded verdict to the launch
// it means, explicitly (#7217 review R2). Only a look that could not list is
// blind, and a verdict that found the id's transcript never starts on the id:
// Claude refuses --session-id for an id it holds a transcript for. evidence
// says whether a newer conversation shows this session moved on. ok is false
// for a verdict this does not name, which the verdict enumeration test
// forbids; its answer then is a resume, which never starts on a filed id.
func recordedClaudeLaunchFor(verdict claudeRecordedVerdict, evidence func() bool) (recordedClaudeVerdict, bool) {
	switch verdict {
	case claudeRecordedUnlisted:
		return recordedClaudeBlind, true
	case claudeRecordedMissing:
		return recordedClaudeStartOn, true
	case claudeRecordedCurrent, claudeRecordedCopies:
		return recordedClaudeResume, true
	case claudeRecordedStale:
		if evidence() {
			return recordedClaudeAbandoned, true
		}
		return recordedClaudeResume, true
	}
	return recordedClaudeResume, false
}

// claudeRecordedAnywhere is whether any Claude store af can name holds an id
// the worktree's own project directories do not (#7217 review R4, Codex P1).
// Claude's --resume <id> looks in the current project, its git worktrees, and
// then every other project in its store, while its --session-id in-use check
// reads only <store>/projects/<name(cwd)>/<id>.jsonl. So a transcript under
// another project (a worktree restored to a new path, as RestoreWorktreePath's
// collision suffix or an archive move leaves it) is filed: starting on the id
// would fork a fresh conversation and drop its history, where --resume finds
// it. The stores are the launch's own, the daemon's CLAUDE_CONFIG_DIR and
// $HOME/.claude, since a pane pointed at either files there and Claude's
// checks read only the store the pane uses. A copy in any project of any of
// them resumes; a store af cannot fully look in is a look that did not
// happen; only an id no project anywhere holds is Missing, and started on.
func claudeRecordedAnywhere(primary, id string) claudeRecordedVerdict {
	for _, store := range append([]string{primary}, otherClaudeStores(primary)...) {
		held, looked := claudeStoreHoldsID(store, id)
		if !looked {
			return claudeRecordedUnlisted
		}
		if held {
			return claudeRecordedCurrent
		}
	}
	return claudeRecordedMissing
}

// claudeStoreHoldsID reports whether any project directory of the Claude store
// holds a transcript for id, and whether af could look in every one. A store
// with no projects directory holds nothing.
func claudeStoreHoldsID(store, id string) (held, looked bool) {
	projectsDir := filepath.Join(store, "projects")
	entries, err := os.ReadDir(projectsDir)
	if os.IsNotExist(err) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	names := []string{id + ".jsonl"}
	if lower := strings.ToLower(id); lower != id {
		names = append(names, lower+".jsonl")
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		for _, name := range names {
			info, err := os.Lstat(filepath.Join(projectsDir, entry.Name(), name))
			switch {
			case err == nil && info.Mode().IsRegular():
				return true, true
			case err != nil && !os.IsNotExist(err):
				return false, false
			}
		}
	}
	return false, true
}

// otherClaudeStores are the Claude config dirs af can name besides primary:
// the daemon's own CLAUDE_CONFIG_DIR, then $HOME/.claude.
func otherClaudeStores(primary string) []string {
	var stores []string
	seen := map[string]bool{filepath.Clean(primary): true}
	add := func(dir string) {
		if strings.TrimSpace(dir) == "" {
			return
		}
		dir = filepath.Clean(dir)
		if !seen[dir] {
			seen[dir] = true
			stores = append(stores, dir)
		}
	}
	add(os.Getenv("CLAUDE_CONFIG_DIR"))
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".claude"))
	}
	return stores
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
