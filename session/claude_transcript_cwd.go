package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

const (
	// claudeTranscriptHeadBudget bounds how far into a transcript af reads for
	// the first record that names a directory. Claude writes nothing but small
	// metadata ahead of its first message, so the budget is there for one
	// oversize record (a pasted image is kept up to 5 MiB) and the next one.
	claudeTranscriptHeadBudget = 32 << 20
	// claudeTranscriptRecordLimit is the longest record af parses. A longer
	// one is read past, not parsed: Claude writes a message's cwd after its
	// body, so the record after it names the same directory.
	claudeTranscriptRecordLimit = 1 << 20
	// claudeTranscriptTailBudget is how much of a transcript's end af reads
	// for the record Claude appends when it moves a conversation to another
	// directory, as Claude's own /resume does.
	claudeTranscriptTailBudget = 64 << 10
)

// claudeTranscriptDir returns the directory the transcript at path records its
// conversation in now: the last record in its tail that names one, else the
// cwd of its first record that does. When /cd, an SDK set_cwd or a worktree
// switch moves a conversation, Claude appends {"type":"relocated",
// "relocatedCwd":…} and every record after it names the new directory as its
// cwd, so the tail says where the conversation lives however far back the
// move was; the head names where it began (#7221 review R1, Codex review). ok
// is false when no record af can read names an absolute directory, or the file
// cannot be read. The read runs inside fsprobe's per-path bound (#7223): it is
// made on the restore loop, at load and on the carry check-ahead, where a
// transcript on a wedged mount must read as unknown, not stall them.
func claudeTranscriptDir(path string) (string, bool) {
	got, err := fsprobe.ReadFileWith(path, func(file *os.File) (any, error) {
		if dir, ok := claudeTranscriptTailDir(file); ok {
			return dir, nil
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		dir, _ := claudeTranscriptHeadCwd(file)
		return dir, nil
	})
	dir, _ := got.(string)
	if err != nil || dir == "" {
		return "", false
	}
	return dir, true
}

// claudeTranscriptHeadCwd is the cwd of the first record within the head
// budget that names an absolute one, reading past any record longer than the
// parse limit.
func claudeTranscriptHeadCwd(file io.Reader) (string, bool) {
	reader := bufio.NewReaderSize(io.LimitReader(file, claudeTranscriptHeadBudget), 64<<10)
	for {
		line, err := readClaudeRecord(reader)
		if line != nil {
			var record struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &record) == nil && filepath.IsAbs(record.Cwd) {
				return record.Cwd, true
			}
		}
		if err != nil {
			return "", false
		}
	}
}

// claudeTranscriptTailDir is the directory named by the last whole record
// within the tail budget that names one: a relocation's relocatedCwd, or a
// message's cwd.
func claudeTranscriptTailDir(file *os.File) (string, bool) {
	info, err := file.Stat()
	if err != nil {
		return "", false
	}
	start := max(info.Size()-claudeTranscriptTailBudget, 0)
	tail := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return "", false
	}
	if start > 0 {
		// The first line is cut by the window; only whole records count.
		if cut := bytes.IndexByte(tail, '\n'); cut >= 0 {
			tail = tail[cut+1:]
		}
	}
	dir, found := "", false
	for _, line := range bytes.Split(tail, []byte{'\n'}) {
		var record struct {
			Type         string `json:"type"`
			RelocatedCwd string `json:"relocatedCwd"`
			Cwd          string `json:"cwd"`
		}
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		switch {
		case record.Type == "relocated" && filepath.IsAbs(record.RelocatedCwd):
			dir, found = record.RelocatedCwd, true
		case filepath.IsAbs(record.Cwd):
			dir, found = record.Cwd, true
		}
	}
	return dir, found
}

// readClaudeRecord returns the next record from reader, or nil for one longer
// than the parse limit, which it reads past.
func readClaudeRecord(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	oversize := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !oversize {
			if len(line)+len(chunk) > claudeTranscriptRecordLimit {
				oversize, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if oversize {
			return nil, err
		}
		return line, err
	}
}

// claudeOwnership is whether a transcript's conversation is a worktree's.
type claudeOwnership int

const (
	claudeOwnedElsewhere claudeOwnership = iota
	claudeOwnedHere
	// claudeOwnershipUnknown: no record af can read names a directory.
	claudeOwnershipUnknown
)

// claudeTranscriptOwnership decides whether a transcript filed under one of
// the project directories of a worktree launched in launchDir holds that
// worktree's conversation, by Claude's own /resume rule (#7208, #7221 review
// R1). Claude's project names collide (a.b and a-b both become a-b), so a
// project directory can hold another directory's conversations: a transcript
// is someone else's only when the directory it records resolves to a
// DIFFERENT directory that files under one of the same project names.
// Everything else is this worktree's: a subdirectory the agent cd'd into (a
// message records the session's current cwd), a directory the conversation
// was relocated from, the worktree's path before a move. Both sides are
// compared resolved, the way the Codex checks compare a rollout's cwd.
func claudeTranscriptOwnership(transcript claudeTranscript, launchDir string) claudeOwnership {
	dir, ok := claudeTranscriptDir(filepath.Join(transcript.dir, transcript.name))
	if !ok {
		return claudeOwnershipUnknown
	}
	if pathutil.ResolveForCompare(dir) == pathutil.ResolveForCompare(launchDir) {
		return claudeOwnedHere
	}
	names := map[string]bool{}
	for _, project := range claudeProjectCandidates(launchDir) {
		names[filepath.Base(project)] = true
	}
	for _, project := range claudeProjectCandidates(dir) {
		if names[filepath.Base(project)] {
			return claudeOwnedElsewhere
		}
	}
	return claudeOwnedHere
}

// newestClaudeTranscriptIn picks the most recently written transcript of the
// worktree launched in launchDir, breaking an exact tie by the greater file
// name. Candidates are read newest first, so the usual answer costs one read.
// Another directory's transcript is passed over; one af cannot attribute
// either way ends the search with no answer, since it may be the worktree's
// newest, and an older one past it would name a conversation the user left
// (#7221 review R2).
func newestClaudeTranscriptIn(transcripts []claudeTranscript, launchDir string) (claudeTranscript, bool) {
	ordered := append([]claudeTranscript(nil), transcripts...)
	sort.SliceStable(ordered, func(a, b int) bool {
		if !ordered[a].modTime.Equal(ordered[b].modTime) {
			return ordered[a].modTime.After(ordered[b].modTime)
		}
		return strings.Compare(ordered[a].name, ordered[b].name) > 0
	})
	for _, transcript := range ordered {
		switch claudeTranscriptOwnership(transcript, launchDir) {
		case claudeOwnedHere:
			return transcript, true
		case claudeOwnershipUnknown:
			return claudeTranscript{}, false
		}
	}
	return claudeTranscript{}, false
}
