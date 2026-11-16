package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

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
		return readClaudeTranscriptDir(file)
	})
	dir, _ := got.(string)
	if err != nil || dir == "" {
		return "", false
	}
	return dir, true
}

// readClaudeTranscriptDir is claudeTranscriptDir over an open transcript: ""
// when no record af can read names a directory.
func readClaudeTranscriptDir(file *os.File) (string, error) {
	if dir, ok := claudeTranscriptTailDir(file); ok {
		return dir, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	dir, _ := claudeTranscriptHeadCwd(file)
	return dir, nil
}

// claudeRecordedDir is what ownership needs of the directory a transcript
// records: where it and each of its lexical ancestors resolve to, and the
// project names their spellings file under.
type claudeRecordedDir struct {
	// resolved is what the recorded directory resolves to, then what each of
	// its lexical ancestors does, nearest first, up to the root.
	resolved []string
	names    map[string]bool
	// unresolved: the kernel's spelling of the directory or of one of its
	// ancestors did not resolve within its bound, so resolved and names may
	// lack the one that decides ownership.
	unresolved bool
}

// claudeRecordedDirFacts resolves dir for claudeTranscriptOwnership. It
// follows symlinks on whatever filesystem dir names, which may be a mount the
// agent cd'd onto that no longer answers, so it runs inside the transcript
// read's bounded flight. A test seam.
//
// It walks dir AND every lexical ancestor of dir, resolving each. Claude files
// a conversation under the project directory of the directory it was launched
// in, while each record names the session's current cwd, which a persisted /cd
// moves (#7221). A project name collides (a.b and a-b both sanitize to a-b),
// so a project directory shared by two launch directories holds the other
// directory's conversations, and a foreign conversation that cd'd into its own
// subdirectory records that subdirectory as its cwd, not the foreign launch
// directory the transcript files under. The recorded directory's own
// spellings alone see the subdirectory, not the colliding launch dir, so the
// ancestors' spellings go into names too (#7367). Each ancestor's resolution
// goes into resolved, because the worktree's own spelling is one of those
// ancestors whenever the agent cd'd below it, and only a resolution tells the
// worktree apart from the colliding directory (#7375 review R1). Every
// spelling is produced through the same bounded claudeProjectSpellings/fsprobe
// path (#7272), inside this one flight: one that did not answer leaves the
// facts unresolved.
var claudeRecordedDirFacts = func(dir string) claudeRecordedDir {
	recorded := claudeRecordedDir{names: map[string]bool{}}
	for walk := filepath.Clean(dir); ; walk = filepath.Dir(walk) {
		// Bounded and checked like the launch side (#7286): a lookup that
		// failed for any reason but absence is a resolution that did not
		// happen.
		projects, resolved, complete := claudeResolvedProjectSpellings(walk)
		if !complete {
			// Ownership is already unknown: stop probing ancestors so a
			// wedged deep path cannot keep blocked fsprobe goroutines alive
			// after the enclosing ReadFileWith flight has returned and the
			// result is discarded (Codex review of #7375).
			return claudeRecordedDir{unresolved: true}
		}
		recorded.resolved = append(recorded.resolved, resolved)
		for _, project := range projects {
			recorded.names[filepath.Base(project)] = true
		}
		// filepath.Dir("/") == "/": stop after the root so a degenerate
		// path cannot spin the walk.
		if parent := filepath.Dir(walk); parent == walk {
			break
		}
	}
	return recorded
}

// claudeTranscriptRecordedDir reads the directory the transcript at path
// records and resolves it, both inside fsprobe's per-path bound (Codex review
// of #7221): a read or a lookup that does not answer leaves ok false, and so
// does a spelling of the directory that did not resolve within its own bound,
// since the names it files under are then incomplete (Codex review of #7268).
func claudeTranscriptRecordedDir(path string) (claudeRecordedDir, bool) {
	facts := claudeRecordedDirFacts // in the caller, as fsprobe's seams
	got, err := fsprobe.ReadFileWith(path, func(file *os.File) (any, error) {
		dir, err := readClaudeTranscriptDir(file)
		if err != nil || dir == "" {
			return nil, err
		}
		return facts(dir), nil
	})
	recorded, ok := got.(claudeRecordedDir)
	return recorded, err == nil && ok && !recorded.unresolved
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
	// claudeOwnershipUnknown: af could not look. No record it can read names a
	// directory; or a resolution either side needed, of the directory the
	// transcript records or of the launch dir, did not answer, or its
	// spellings came back incomplete (#7278 review R1, #7286).
	claudeOwnershipUnknown
)

// claudeTranscriptOwnership decides whether a transcript filed under one of
// the project directories of a worktree launched in launchDir holds that
// worktree's conversation, by Claude's own /resume rule (#7208, #7221 review
// R1). Claude's project names collide (a.b and a-b both become a-b), so a
// project directory can hold another directory's conversations: a transcript
// is someone else's only when neither the directory it records nor any
// lexical ancestor of it resolves to the launch dir or under it, and one of
// them files under one of the same project names (an ancestor counts because a
// colliding directory's conversation may have cd'd into its own subdirectory,
// #7367). Everything else is this worktree's: a subdirectory the agent cd'd
// into (a message records the session's current cwd), including one reached
// through a symlink that leads out of the worktree or through an alias of it
// (#7375 review R1), a directory the conversation was relocated from, the
// worktree's path before a move. A conversation of this worktree that cd'd
// into the colliding directory's own tree reads as that directory's and cannot
// be told apart from that directory's own conversations, the same ambiguity a
// cd into the colliding directory itself already has. Both sides are compared
// resolved, the way the Codex checks compare a rollout's cwd, and every
// resolution is bounded: the recorded side's (the directory and each ancestor)
// inside the transcript read's flight, the launch side's by fsprobe (#7272).
// Either may sit on a mount that no longer answers (NFS, FUSE, a dead sshfs),
// and a resolution that did not answer within the bound is a look that did not
// happen: ownership is unknown, never another directory's. "Another
// directory's" would leave the recorded conversation the newest, which a carry
// reads as leave to carry it.
func claudeTranscriptOwnership(transcript claudeTranscript, launchDir string) claudeOwnership {
	recorded, ok := claudeTranscriptRecordedDir(filepath.Join(transcript.dir, transcript.name))
	if !ok {
		return claudeOwnershipUnknown
	}
	launch, err := fsprobe.ResolveForCompare(launchDir)
	if err != nil {
		return claudeOwnershipUnknown
	}
	// The recorded dir, or any lexical ancestor of it, that resolves to the
	// launch dir or under it makes the conversation this worktree's (#7375
	// review R1). The walk always reaches the worktree's own spelling once the
	// agent cd'd below it, and that spelling files under the colliding project
	// name too, so it is no evidence of another directory: a cd through a
	// symlink below the worktree, or through an alias of it, records a
	// directory that resolves elsewhere while an ancestor resolves here. Only
	// when every walked directory resolves outside this worktree do their
	// names say whose it is. A launch at the root contains every recorded
	// directory.
	for _, dir := range recorded.resolved {
		if pathutil.IsAtOrInside(dir, launch) {
			return claudeOwnedHere
		}
	}
	// The project names the launch dir files under are a launch-side
	// resolution too: an incomplete list could miss the name that makes a
	// colliding transcript another directory's.
	projects, complete := claudeProjectSpellings(launchDir)
	if !complete {
		return claudeOwnershipUnknown
	}
	for _, project := range projects {
		if recorded.names[filepath.Base(project)] {
			return claudeOwnedElsewhere
		}
	}
	return claudeOwnedHere
}
