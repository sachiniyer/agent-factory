package session

import (
	"fmt"
	"path/filepath"
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
// reached the way probeCarryArtifact checks it, or nil and why not.
func statClaudeCopy(root carryRoot, project, id string) (*claudeCopy, error) {
	file, info, err := openCarryArtifact(root, filepath.Join(project, id+".jsonl"))
	if err != nil {
		return nil, err
	}
	_ = file.Close()
	return &claudeCopy{project: project, size: info.Size(), modTime: info.ModTime()}, nil
}

// liveClaudeCopy picks, from copies of conversation id in root, the project
// directory of the one Claude is appending to: the newest. Copies last written
// at the same moment are told apart by content, since the live one extends the
// others; copies that differ otherwise are a refusal naming both, because af
// cannot tell which one Claude resumes.
func liveClaudeCopy(root carryRoot, id string, copies []claudeCopy) (string, string) {
	newest := copies[0]
	for _, other := range copies[1:] {
		if other.modTime.After(newest.modTime) {
			newest = other
		}
	}
	live := newest
	var tied []claudeCopy
	for _, other := range copies {
		if other.modTime.Equal(newest.modTime) {
			tied = append(tied, other)
			if other.size > live.size {
				live = other
			}
		}
	}
	for _, other := range tied {
		if other.project == live.project {
			continue
		}
		if !claudeCopyExtends(root, id, live.project, other.project) {
			path := func(project string) string { return filepath.Join(root.dir(), project, id+".jsonl") }
			return "", fmt.Sprintf(carryTranscriptCopiesFormat, path(live.project), path(other.project))
		}
	}
	return live.project, ""
}

// claudeCopyExtends reports whether the copy of conversation id under longer
// begins with every byte of the copy under shorter. A copy af cannot read
// extends nothing.
func claudeCopyExtends(root carryRoot, id, longer, shorter string) bool {
	long, longInfo, err := openCarryArtifact(root, filepath.Join(longer, id+".jsonl"))
	if err != nil {
		return false
	}
	defer long.Close()
	short, shortInfo, err := openCarryArtifact(root, filepath.Join(shorter, id+".jsonl"))
	if err != nil {
		return false
	}
	defer short.Close()
	if longInfo.Size() < shortInfo.Size() {
		return false
	}
	same, err := carryPrefixMatches(long, short, shortInfo.Size())
	return err == nil && same
}

// claudeTargetReason refuses a carry whose new account already holds the
// conversation under another spelling of the worktree's path than the one the
// carry writes (#7207). The copy would leave two there, and the replacement
// could resume the other one while af reports the conversation carried.
func (c *conversationCarry) claudeTargetReason() string {
	for _, project := range c.claudeProjects {
		rel := filepath.Join(project, c.id+".jsonl")
		if rel == c.transcript {
			continue
		}
		present, err := probeCarryArtifact(c.dstRoot, rel)
		switch {
		case present:
			return fmt.Sprintf(carryTargetOtherCopyFormat, filepath.Join(c.dstHome, rel))
		case carrySymlinkPath(err) != "":
			return symlinkCarryReason(carrySymlinkPath(err))
		case err != nil:
			return carryTargetUncheckedReason
		}
	}
	return ""
}
