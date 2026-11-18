package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
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
	projects, complete := claudeProjectSpellings(launch.WorkingDir)
	if !complete {
		// A spelling af could not resolve may hold the copy Claude is
		// appending to: carrying the one the recorded spelling holds could
		// drop the latest turns (Codex review of #7268).
		return carrySpellingsUnresolvedReason
	}
	c.claudeProjects = projects
	c.claudeLaunchDir = launch.WorkingDir
	var refused string
	// unlooked is a copy whose lookup did not answer, or failed: it may be
	// the one Claude is appending to, so no other copy can be bound (#7383).
	unlooked := false
	var src, dst []claudeCopy
	note := func(root carryRoot, project string, into *[]claudeCopy) {
		found, err := statClaudeCopy(root, project, c.id)
		switch {
		case found != nil:
			*into = append(*into, *found)
		case carryLookupUnlooked(err):
			unlooked = true
		case carrySymlinkPath(err) != "" && refused == "":
			refused = symlinkCarryReason(carrySymlinkPath(err))
		}
	}
	// The first lookup that does not answer settles it: the rest are on the
	// same mount, and each would wait out a bound of its own (#7389 review R2).
	for _, project := range c.claudeProjects {
		note(c.srcRoot, project, &src)
		if c.committed && !unlooked {
			note(c.dstRoot, project, &dst)
		}
		if unlooked {
			return carryUnlookedReason
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
// reached the way openCarryArtifact opens it, or nil and why not. The lookup
// runs in a paced flight (#7383).
func statClaudeCopy(root carryRoot, project, id string) (*claudeCopy, error) {
	rel := filepath.Join(project, id+".jsonl")
	return fsprobe.Paced("Claude transcript copy lookup", root.key()+"\x00"+rel, func(step func()) (*claudeCopy, error) {
		file, info, err := openCarryArtifact(pacedCarryWalk(step), root, rel)
		if err != nil {
			return nil, err
		}
		_ = file.Close()
		return &claudeCopy{project: project, size: info.Size(), modTime: info.ModTime()}, nil
	})
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
			if other.project == candidate.project {
				continue
			}
			extends, err := claudeCopyExtends(root, rel(candidate.project), root, rel(other.project))
			if err != nil {
				// Neither "extends" nor "diverges": af could not compare them.
				return "", carryUnlookedReason
			}
			if !extends {
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
// longer one is the shorter one with turns appended. A file that is not there,
// or is not a plain file, extends nothing; one file under two names extends
// itself. The comparison runs in a paced flight whose bound is per read
// (#7383), and err is a lookup in it that did not answer, or failed: then af
// could not tell.
func claudeCopyExtends(longRoot carryRoot, longer string, shortRoot carryRoot, shorter string) (bool, error) {
	return fsprobe.Paced("Claude transcript copy comparison",
		longRoot.key()+"\x00"+longer+"\x00"+shortRoot.key()+"\x00"+shorter,
		func(step func()) (bool, error) {
			return claudeCopyExtendsWith(pacedCarryWalk(step), longRoot, longer, shortRoot, shorter)
		})
}

// claudeCopyExtendsWith is claudeCopyExtends on a walk p already paces.
func claudeCopyExtendsWith(p carryPace, longRoot carryRoot, longer string, shortRoot carryRoot, shorter string) (bool, error) {
	long, longInfo, err := openCarryArtifact(p, longRoot, longer)
	if err != nil {
		return false, unlookedOnly(err)
	}
	defer long.Close()
	short, shortInfo, err := openCarryArtifact(p, shortRoot, shorter)
	if err != nil {
		return false, unlookedOnly(err)
	}
	defer short.Close()
	if os.SameFile(longInfo, shortInfo) {
		return true, nil
	}
	if longInfo.Size() < shortInfo.Size() {
		return false, nil
	}
	same, err := carryPrefixMatches(p, long, short, shortInfo.Size())
	if err != nil {
		return false, unlookedOnly(err)
	}
	return same, nil
}

// unlookedOnly is err when it is a look that did not happen
// (carryLookupUnlooked — a timeout, a failed syscall, or the guarded open's
// non-regular refusal, #7527), and nil for an answer such as absence.
func unlookedOnly(err error) error {
	if carryLookupUnlooked(err) {
		return err
	}
	return nil
}

// claudeTargetReason refuses a carry whose new account already holds the
// conversation under another spelling of the worktree's path than the one the
// carry writes, in a copy its Claude could resume instead of the carried one
// (#7207): one not older than the carried copy, or one the carried copy does
// not extend. An older copy the carried one extends is the state the source
// plan accepts as an older copy Claude has moved past, so it is left in place
// (#7220 review R3); af deletes nothing from an account's home.
//
// The lookups run in one paced flight whose bound is per syscall; one that
// does not answer is af could not check the new account's home (#7383).
func (c *conversationCarry) claudeTargetReason() string {
	in := claudeTargetLookup{srcRoot: c.srcRoot, dstRoot: c.dstRoot, dstHome: c.dstHome, transcript: c.transcript,
		id: c.id, projects: append([]string(nil), c.claudeProjects...)}
	reason, err := fsprobe.Paced("Claude target copy lookup",
		in.srcRoot.key()+"\x00"+in.dstRoot.key()+"\x00"+in.transcript+"\x00"+strings.Join(in.projects, "\x00"),
		func(step func()) (string, error) { return judgeClaudeTarget(pacedCarryWalk(step), in), nil })
	if err != nil {
		return carryTargetUncheckedReason
	}
	return reason
}

// claudeTargetLookup is what claudeTargetReason judges, copied out of the
// carry: an abandoned flight must not read state the plan goes on changing.
type claudeTargetLookup struct {
	srcRoot, dstRoot    carryRoot
	dstHome, transcript string
	id                  string
	projects            []string
}

func judgeClaudeTarget(p carryPace, in claudeTargetLookup) string {
	carried, carriedInfo, err := openCarryArtifact(p, in.srcRoot, in.transcript)
	if err != nil {
		return carryTargetUncheckedReason
	}
	_ = carried.Close()
	for _, project := range in.projects {
		rel := filepath.Join(project, in.id+".jsonl")
		if rel == in.transcript {
			continue
		}
		other, otherInfo, err := openCarryArtifact(p, in.dstRoot, rel)
		switch {
		case err == nil:
			_ = other.Close()
			if otherInfo.ModTime().Before(carriedInfo.ModTime()) {
				extends, err := claudeCopyExtendsWith(p, in.srcRoot, in.transcript, in.dstRoot, rel)
				if err != nil {
					return carryTargetUncheckedReason
				}
				if extends {
					continue
				}
			}
			return fmt.Sprintf(carryTargetOtherCopyFormat, filepath.Join(in.dstHome, rel))
		case errors.Is(err, fs.ErrNotExist):
		case carrySymlinkPath(err) != "":
			return symlinkCarryReason(carrySymlinkPath(err))
		default:
			return carryTargetUncheckedReason
		}
	}
	return ""
}
