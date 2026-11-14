package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7295, from the independent review of #7291. A gone launch dir lists the
// spelling Claude filed it under, its deepest existing ancestor's resolution
// with the missing remainder rejoined. That rejoin is an answer only when
// every component the walk fell back past is proven absent. A symlink in the
// missing remainder whose target is gone still exists, and the kernel spelled
// the path through that target: such a listing reads incomplete, so the carry
// refuses rather than bind an older copy, and the restore resumes.

// goneLaunchDirLayout builds a launch dir under root and returns it, the
// directory the kernel spells it as, and the func that makes it gone.
type goneLaunchDirLayout struct {
	name  string
	build func(t *testing.T, root string) (workDir, kernel string, gone func())
}

func goneLaunchDirLayouts() []goneLaunchDirLayout {
	return []goneLaunchDirLayout{
		{"the launch dir is a symlink whose target is gone", func(t *testing.T, root string) (string, string, func()) {
			store := filepath.Join(root, "store", "repo")
			require.NoError(t, os.MkdirAll(store, 0o755))
			require.NoError(t, os.MkdirAll(filepath.Join(root, "wt"), 0o755))
			workDir := filepath.Join(root, "wt", "repo")
			require.NoError(t, os.Symlink(store, workDir))
			return workDir, store, func() { require.NoError(t, os.RemoveAll(store)) }
		}},
		{"a middle component is a symlink whose target is gone", func(t *testing.T, root string) (string, string, func()) {
			store := filepath.Join(root, "store", "sub")
			require.NoError(t, os.MkdirAll(filepath.Join(store, "repo"), 0o755))
			require.NoError(t, os.MkdirAll(filepath.Join(root, "wt"), 0o755))
			require.NoError(t, os.Symlink(store, filepath.Join(root, "wt", "sub")))
			return filepath.Join(root, "wt", "sub", "repo"), filepath.Join(store, "repo"), func() { require.NoError(t, os.RemoveAll(store)) }
		}},
		{"a symlinked parent whose leaf is gone (#7290)", func(t *testing.T, root string) (string, string, func()) {
			real := filepath.Join(root, "real")
			require.NoError(t, os.MkdirAll(filepath.Join(real, "repo"), 0o755))
			require.NoError(t, os.Symlink(real, filepath.Join(root, "link")))
			return filepath.Join(root, "link", "repo"), filepath.Join(real, "repo"), func() { require.NoError(t, os.RemoveAll(filepath.Join(real, "repo"))) }
		}},
	}
}

// The restore: an older copy of the id and a newer conversation under the
// recorded spelling, the newest copy under the kernel's. With the launch dir
// present the restore resumes; with it gone it still resumes, retiring
// nothing, whether the listing reads incomplete or lists the kernel's spelling.
func TestClaudeRestoreResumesWhenAGoneLaunchDirHadASymlink(t *testing.T) {
	for _, layout := range goneLaunchDirLayouts() {
		t.Run(layout.name, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)
			workDir, kernel, gone := layout.build(t, testguard.CanonicalTempDir(t))
			recordedProject := filepath.Join("projects", claudeProjectName(workDir))
			kernelProject := filepath.Join("projects", claudeProjectName(kernel))
			now := time.Now()
			line := claudeRecordLine(t, kernel)
			setModTime(t, writeCarryFile(t, configDir, filepath.Join(recordedProject, carryTestID+".jsonl"), line), now.Add(-2*time.Hour))
			setModTime(t, writeCarryFile(t, configDir, filepath.Join(recordedProject, newerConversationID+".jsonl"), line), now.Add(-time.Hour))
			setModTime(t, writeCarryFile(t, configDir, filepath.Join(kernelProject, carryTestID+".jsonl"), line+line), now.Add(-30*time.Minute))
			inst := claudeRestoreInstance(t, workDir, carryTestID)
			require.Equal(t, recordedClaudeResume, inst.judgeRecordedClaude("claude", workDir, carryTestID), "precondition: the newest copy is current")

			gone()
			projects, complete := claudeProjectSpellings(workDir)
			require.False(t, complete && len(projects) == 1, "a complete listing names the kernel's spelling: %v", projects)
			program, ok, decline := prepareExactResumeConversation(inst, "claude")
			require.True(t, ok, "the restore resumes the recorded id: %+v", decline)
			require.Equal(t, "claude --resume "+carryTestID, program)
			require.False(t, decline.retiresRecord(inst.AgentConversation()), "nothing is retired")
		})
	}
}

// The carry: an older copy under the recorded spelling, the newest under the
// kernel's. With the launch dir gone the carry either refuses, as a listing it
// cannot complete, or carries the newest copy: never the older one, which
// would drop the latest turns.
func TestClaudeCarryNeverBindsAnOlderCopyWhenAGoneLaunchDirHadASymlink(t *testing.T) {
	for _, layout := range goneLaunchDirLayouts() {
		t.Run(layout.name, func(t *testing.T) {
			home := carryAmbientProviderHomes(t)
			inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
			workDir, kernel, gone := layout.build(t, testguard.CanonicalTempDir(t))
			cmd := exec.Command("git", "init")
			cmd.Dir = workDir
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
			gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, workDir, inst.Title, "main", "", false, true)
			require.NoError(t, err)
			inst.SetGitWorktreeForTest(gw)
			recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
			recordedProject := filepath.Join("projects", claudeProjectName(workDir))
			kernelProject := filepath.Join("projects", claudeProjectName(kernel))
			source := filepath.Join(home, ".claude")
			now := time.Now()
			line := claudeRecordLine(t, kernel)
			setModTime(t, writeCarryFile(t, source, filepath.Join(recordedProject, carryTestID+".jsonl"), line), now.Add(-2*time.Hour))
			setModTime(t, writeCarryFile(t, source, filepath.Join(kernelProject, carryTestID+".jsonl"), line+line), now.Add(-30*time.Minute))
			require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: the newest copy carries")

			gone()
			refusal := inst.AccountSwapCarryRefusal()
			require.NoError(t, inst.ValidateAccountSwap("work"))
			carry := inst.accountSwapLaunch.carry
			if carry == nil {
				require.NotEmpty(t, refusal, "a carry the live swap refuses, the check-ahead refuses too")
				require.Equal(t, carrySpellingsUnresolvedReason, inst.accountSwapLaunch.carryFallback)
				return
			}
			require.Equal(t, filepath.Join(kernelProject, carryTestID+".jsonl"), carry.transcript,
				"the carry takes the newest copy, never the older one")
		})
	}
}
