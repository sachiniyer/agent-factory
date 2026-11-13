package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// claudeCopy is one copy of the recorded conversation's transcript: the
// project directory it is under, relative to a Claude config dir, and its size
// and modification time when af found it.
type claudeCopy struct {
	project string
	size    int64
	modTime time.Time
}

// locateClaude finds the recorded conversation's transcript and binds the copy
// the carry takes. Claude may file a conversation under any spelling of the
// worktree's path (claudeProjectCandidates), and after its spelling changes,
// under more than one. The carry takes the copy Claude is appending to, the
// newest in the previous account's home (#7207); the staleness check judges
// that same copy. A committed carry re-binds the copy it already landed in the
// new account's home, so a re-plan never switches copies under a carry that
// has begun.
func (c *conversationCarry) locateClaude(program, workDir string) string {
	launch, err := tmux.CommandEnvironmentFromCommand(program, workDir)
	if err != nil || !launch.WorkingDirKnown() {
		return carryNoClaudeDirReason
	}
	c.claudeProjects = claudeProjectCandidates(launch.WorkingDir)
	c.claudeLaunchDir = launch.WorkingDir
	var refused string
	var src, dst []claudeCopy
	note := func(root carryRoot, project string, into *[]claudeCopy) {
		found, err := statClaudeCopy(root, project, c.id)
		if found != nil {
			*into = append(*into, *found)
		} else if path := carrySymlinkPath(err); path != "" && refused == "" {
			refused = symlinkCarryReason(path)
		}
	}
	for _, project := range c.claudeProjects {
		note(c.srcRoot, project, &src)
		if c.committed {
			note(c.dstRoot, project, &dst)
		}
	}
	root, copies := c.srcRoot, src
	if len(dst) > 0 {
		root, copies = c.dstRoot, dst
	}
	if len(copies) == 0 {
		if refused != "" {
			return refused
		}
		return carryTranscriptMissingReason
	}
	project, reason := liveClaudeCopy(root, c.id, copies)
	if reason != "" {
		return reason
	}
	c.transcript = filepath.Join(project, c.id+".jsonl")
	c.aux = filepath.Join(project, c.id)
	return ""
}

// statClaudeCopy reports the copy of conversation id under project in root,
// reached the way openCarryArtifact opens it, or nil and why not.
func statClaudeCopy(root carryRoot, project, id string) (*claudeCopy, error) {
	file, info, err := openCarryArtifact(root, filepath.Join(project, id+".jsonl"))
	if err != nil {
		return nil, err
	}
	_ = file.Close()
	return &claudeCopy{project: project, size: info.Size(), modTime: info.ModTime()}, nil
}

// liveClaudeCopy picks, from copies of conversation id in root, the project
// directory of the copy that holds every byte of every other copy: the one
// Claude went on appending to after a spelling change left an older copy
// behind. Copies that diverge, where neither holds the other, are two lines of
// the conversation written by two Claude processes (the pane, and a terminal
// resuming it under the other spelling); carrying either one drops the other's
// turns, however recently each was written, so that refuses, naming both
// (#7220 review R1). Among copies that each hold all the others, identical
// ones, the newest is taken.
func liveClaudeCopy(root carryRoot, id string, copies []claudeCopy) (string, string) {
	ordered := append([]claudeCopy(nil), copies...)
	sort.SliceStable(ordered, func(a, b int) bool {
		if ordered[a].size != ordered[b].size {
			return ordered[a].size > ordered[b].size
		}
		return ordered[a].modTime.After(ordered[b].modTime)
	})
	rel := func(project string) string { return filepath.Join(project, id+".jsonl") }
	for _, candidate := range ordered {
		holdsAll := true
		for _, other := range ordered {
			if other.project != candidate.project && !claudeCopyExtends(root, rel(candidate.project), root, rel(other.project)) {
				holdsAll = false
				break
			}
		}
		if holdsAll {
			return candidate.project, ""
		}
	}
	path := func(project string) string { return filepath.Join(root.dir(), rel(project)) }
	return "", fmt.Sprintf(carryTranscriptCopiesFormat, path(ordered[0].project), path(ordered[1].project))
}

// claudeCopyExtends reports whether the file at longer, beneath longRoot,
// begins with every byte of the file at shorter, beneath shortRoot: the
// longer one is the shorter one with turns appended. A file af cannot read
// extends nothing; one file under two names extends itself.
func claudeCopyExtends(longRoot carryRoot, longer string, shortRoot carryRoot, shorter string) bool {
	long, longInfo, err := openCarryArtifact(longRoot, longer)
	if err != nil {
		return false
	}
	defer long.Close()
	short, shortInfo, err := openCarryArtifact(shortRoot, shorter)
	if err != nil {
		return false
	}
	defer short.Close()
	if os.SameFile(longInfo, shortInfo) {
		return true
	}
	if longInfo.Size() < shortInfo.Size() {
		return false
	}
	same, err := carryPrefixMatches(long, short, shortInfo.Size())
	return err == nil && same
}

// claudeTargetReason refuses a carry whose new account already holds the
// conversation under another spelling of the worktree's path than the one the
// carry writes, in a copy its Claude could resume instead of the carried one
// (#7207): one not older than the carried copy, or one the carried copy does
// not extend. An older copy the carried one extends is the state the source
// plan accepts as an older copy Claude has moved past, so it is left in place
// (#7220 review R3); af deletes nothing from an account's home.
func (c *conversationCarry) claudeTargetReason() string {
	carried, carriedInfo, err := openCarryArtifact(c.srcRoot, c.transcript)
	if err != nil {
		return carryTargetUncheckedReason
	}
	_ = carried.Close()
	for _, project := range c.claudeProjects {
		rel := filepath.Join(project, c.id+".jsonl")
		if rel == c.transcript {
			continue
		}
		other, otherInfo, err := openCarryArtifact(c.dstRoot, rel)
		switch {
		case err == nil:
			_ = other.Close()
			if otherInfo.ModTime().Before(carriedInfo.ModTime()) && claudeCopyExtends(c.srcRoot, c.transcript, c.dstRoot, rel) {
				continue
			}
			return fmt.Sprintf(carryTargetOtherCopyFormat, filepath.Join(c.dstHome, rel))
		case errors.Is(err, fs.ErrNotExist):
		case carrySymlinkPath(err) != "":
			return symlinkCarryReason(carrySymlinkPath(err))
		default:
			return carryTargetUncheckedReason
		}
	}
	return ""
}
