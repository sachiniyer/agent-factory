package session

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7539: in a copy comparison only two opened regular files may produce
// "does not extend" or "duplicated". The listing notes two regular copies;
// before the comparison opens them, one is swapped for a symlink or deleted.
// An open refused that way is a look that did not happen — unlooked, or a
// named refusal — never evidence that the copies diverge.

// swapACopyAfterTheListing mutates path the way the issue's race does: a
// symlink swapped in for the regular copy the listing saw, or the copy
// deleted outright.
var swapACopyAfterTheListing = []struct {
	name   string
	mutate func(t *testing.T, configDir, path string)
}{
	{
		name: "swapped for a symlink",
		mutate: func(t *testing.T, configDir, path string) {
			target := writeCarryFile(t, configDir, "impostor.jsonl", "{}\n")
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Symlink(target, path))
		},
	},
	{
		name: "deleted",
		mutate: func(t *testing.T, configDir, path string) {
			require.NoError(t, os.Remove(path))
		},
	},
}

// statListedClaudeCopies is the listing liveClaudeCopy judges: every copy of
// id under projects in configDir, as statClaudeCopy reports it.
func statListedClaudeCopies(t *testing.T, root carryRoot, projects []string, id string) []claudeCopy {
	t.Helper()
	var copies []claudeCopy
	for _, project := range projects {
		found, err := statClaudeCopy(root, project, id)
		require.NoError(t, err)
		require.NotNil(t, found)
		copies = append(copies, *found)
	}
	require.GreaterOrEqual(t, len(copies), 2, "the test needs at least two copies to compare")
	return copies
}

// A copy the listing saw is swapped for a symlink or deleted before the
// comparison's open reaches it. Neither open refusal is evidence the copies
// diverge: the lookup did not happen, so liveClaudeCopy refuses unverified
// rather than claiming Claude holds two diverging lines of the conversation.
func TestLiveClaudeCopyACopySwappedAfterTheListingIsUnlooked(t *testing.T) {
	for _, tc := range swapACopyAfterTheListing {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			root := ambientCarryRoot(configDir)
			projects := []string{"projects/-recorded", "projects/-resolved"}
			writeCarryFile(t, configDir, filepath.Join(projects[0], carryTestID+".jsonl"), "{}\n")
			otherPath := writeCarryFile(t, configDir, filepath.Join(projects[1], carryTestID+".jsonl"), "{}\n{}\n")
			copies := statListedClaudeCopies(t, root, projects, carryTestID)

			tc.mutate(t, configDir, otherPath)

			project, reason := liveClaudeCopy(root, carryTestID, copies)
			require.Empty(t, project)
			require.Equal(t, carryUnlookedReason, reason,
				"an open refused after the listing is a look that did not happen, never evidence the copies diverge")
		})
	}
}

// The same property one level up: the staleness listing sees a newer copy
// under another spelling, then the comparison's paced open of it finds the
// file swapped out after that listing. claudeCarryStaleness must refuse
// unverified — never "a newer copy diverges, move one aside".
func TestClaudeCarryStalenessACopySwappedAfterTheListingIsUnlooked(t *testing.T) {
	for _, tc := range swapACopyAfterTheListing {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			projects := []string{"projects/-recorded", "projects/-resolved"}
			past := time.Now().Add(-time.Hour)
			boundPath := writeCarryFile(t, configDir, filepath.Join(projects[0], carryTestID+".jsonl"), "{}\n")
			otherPath := writeCarryFile(t, configDir, filepath.Join(projects[1], carryTestID+".jsonl"), "{}\n{}\n")
			setModTime(t, boundPath, past)
			setModTime(t, otherPath, past.Add(time.Minute))

			// The listing already saw both copies as regular files. The
			// comparison's paced walk calls the stand-in before each syscall;
			// swap the newer copy out exactly when its open is about to run.
			var once sync.Once
			standIn := func(op, path string) {
				if op == "open" && path == otherPath {
					once.Do(func() { tc.mutate(t, configDir, otherPath) })
				}
			}
			prev := carryLookupStandIn.Load()
			carryLookupStandIn.Store(&standIn)
			t.Cleanup(func() { carryLookupStandIn.Store(prev) })

			reason := claudeCarryStaleness(configDir, projects, "/work/repo",
				filepath.Join(projects[0], carryTestID+".jsonl"), carryTestID, func() bool { return true })
			require.Equal(t, carryUnlookedReason, reason,
				"an open refused after the listing is a look that did not happen, never evidence about the copies")
		})
	}
}

// The plan's target-copy check already names what sits at the path: a
// directory where the carried transcript belongs is "the new account holds
// something other than a plain file where the conversation belongs", not the
// generic could-not-look clause. Only the comparison callers take the
// unlooked class (#7539).
func TestTargetCopyReasonKeepsItsNamedNonRegularRefusal(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	rel := filepath.Join("projects", "-repo", carryTestID+".jsonl")
	writeCarryFile(t, src, rel, "{}\n")
	require.NoError(t, os.MkdirAll(filepath.Join(dst, rel), 0o700),
		"a directory where the transcript file belongs")

	c := &conversationCarry{
		agent:      tmux.ProgramClaude,
		srcRoot:    ambientCarryRoot(src),
		dstRoot:    ambientCarryRoot(dst),
		transcript: rel,
	}
	require.Equal(t, "the new account holds something other than a plain file where the conversation belongs",
		c.targetCopyReason())
}
