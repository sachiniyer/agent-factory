package session

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// claudeTranscriptHeadBudget bounds how much of a transcript af reads to learn
// the directory its conversation was launched in.
const claudeTranscriptHeadBudget = 1 << 20

// claudeTranscriptCwd returns the directory the conversation in the transcript
// at path was launched in: the cwd of its first record that names one. Claude
// writes cwd on every message record; the summaries and snapshots it can write
// ahead of them name none. ok is false when no record within the head budget
// names an absolute cwd, or the file cannot be read.
func claudeTranscriptCwd(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	reader := bufio.NewReader(io.LimitReader(file, claudeTranscriptHeadBudget))
	for {
		line, err := reader.ReadBytes('\n')
		var record struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &record) == nil && filepath.IsAbs(record.Cwd) {
			return record.Cwd, true
		}
		if err != nil {
			return "", false
		}
	}
}

// claudeOwnership is whether a transcript's conversation was launched in a
// worktree.
type claudeOwnership int

const (
	claudeOwnedElsewhere claudeOwnership = iota
	claudeOwnedHere
	// claudeOwnershipUnknown: the transcript names no cwd af can read.
	claudeOwnershipUnknown
)

// claudeTranscriptOwnership decides whether transcript's conversation was
// launched in launchDir from the cwd it records, not from the project
// directory it is filed under (#7208). Claude's project names collide (a.b and
// a-b both become a-b), so one project directory can hold another
// directory's conversations. Both sides are compared resolved, the way the
// Codex checks compare a rollout's session_meta cwd.
func claudeTranscriptOwnership(transcript claudeTranscript, launchDir string) claudeOwnership {
	cwd, ok := claudeTranscriptCwd(filepath.Join(transcript.dir, transcript.name))
	if !ok {
		return claudeOwnershipUnknown
	}
	if pathutil.ResolveForCompare(cwd) == pathutil.ResolveForCompare(launchDir) {
		return claudeOwnedHere
	}
	return claudeOwnedElsewhere
}

// newestClaudeTranscriptIn picks, among transcripts whose conversation was
// provably launched in launchDir, the most recently written, breaking an exact
// tie by the greater file name. Candidates are read newest first, so the usual
// answer costs one head read.
func newestClaudeTranscriptIn(transcripts []claudeTranscript, launchDir string) (claudeTranscript, bool) {
	ordered := append([]claudeTranscript(nil), transcripts...)
	sort.SliceStable(ordered, func(a, b int) bool {
		if !ordered[a].modTime.Equal(ordered[b].modTime) {
			return ordered[a].modTime.After(ordered[b].modTime)
		}
		return strings.Compare(ordered[a].name, ordered[b].name) > 0
	})
	for _, transcript := range ordered {
		if claudeTranscriptOwnership(transcript, launchDir) == claudeOwnedHere {
			return transcript, true
		}
	}
	return claudeTranscript{}, false
}
