package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestPrepareCreateLaunchResolvesCommandSpecificCodexHome(t *testing.T) {
	tests := []struct {
		name    string
		command func(repoRoot string) string
		want    func(repoRoot string) string
		wantErr string
	}{
		{
			name: "absolute CODEX_HOME",
			command: func(repoRoot string) string {
				return "CODEX_HOME=" + filepath.Join(repoRoot, "absolute-store") + " codex"
			},
			want: func(repoRoot string) string { return filepath.Join(repoRoot, "absolute-store") },
		},
		{
			name:    "relative CODEX_HOME",
			command: func(string) string { return "CODEX_HOME=relative-store codex" },
			want:    func(repoRoot string) string { return filepath.Join(repoRoot, "relative-store") },
		},
		{
			name:    "env chdir precedes relative CODEX_HOME",
			command: func(string) string { return "env -C nested CODEX_HOME=relative-store codex" },
			want: func(repoRoot string) string {
				return filepath.Join(repoRoot, "nested", "relative-store")
			},
		},
		{
			name:    "unset CODEX_HOME uses command HOME",
			command: func(string) string { return "env -uCODEX_HOME HOME=relative-home codex" },
			want: func(repoRoot string) string {
				return filepath.Join(repoRoot, "relative-home", ".codex")
			},
		},
		{
			name:    "dynamic CODEX_HOME fails closed",
			command: func(string) string { return "CODEX_HOME=$ALT_CODEX_HOME codex" },
			wantErr: "shell expansion",
		},
		{
			name:    "split-string env fails closed",
			command: func(string) string { return "env -S 'CODEX_HOME=/tmp/create-codex codex'" },
			wantErr: "split-string",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			repoRoot := initInPlaceRepo(t, "prepared-create")
			require.NoError(t, os.Mkdir(filepath.Join(repoRoot, "nested"), 0o755))

			cfg := config.DefaultConfig()
			cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: tc.command(repoRoot)}
			require.NoError(t, config.SaveConfig(cfg))

			inst, err := NewInstance(InstanceOptions{
				Title:   "prepared-create",
				Path:    repoRoot,
				Program: tmux.ProgramCodex,
				InPlace: true,
			})
			require.NoError(t, err)

			plan, err := inst.PrepareCreateLaunch()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, repoRoot, plan.workDir,
				"the capture plan must be prepared only after provisioning fixes the launch cwd")
			require.Equal(t, tc.want(repoRoot), plan.conversationCapture.codexHome)
			require.False(t, plan.conversationCapture.startedAt.IsZero(),
				"the before-image must be taken during prepare, before launch")

			writeCodexRolloutFile(t, tc.want(repoRoot),
				"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
			conv, captureErr := CaptureAgentConversation(tmux.ProgramCodex, plan.ConversationCapture(), time.Second)
			require.NoError(t, captureErr)
			require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID,
				"post-launch capture must observe the store frozen by the create plan")
		})
	}
}

func TestPrepareCreateLaunchUsesProvisionedLinkedWorktreeCwd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	repoRoot := initInPlaceRepo(t, "linked-create")

	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: "CODEX_HOME=relative-store codex"}
	require.NoError(t, config.SaveConfig(cfg))
	inst, err := NewInstance(InstanceOptions{
		Title: "linked-create", Path: repoRoot, Program: tmux.ProgramCodex,
	})
	require.NoError(t, err)

	plan, err := inst.PrepareCreateLaunch()
	require.NoError(t, err)
	require.NotEqual(t, repoRoot, plan.workDir, "regular create must use its linked worktree, not the source repo")
	require.Equal(t, inst.GetWorktreePath(), plan.workDir)
	require.Equal(t, filepath.Join(plan.workDir, "relative-store"), plan.conversationCapture.codexHome)
}

func TestPreparedCreateLaunchConsumesFrozenCommand(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	repoRoot := initInPlaceRepo(t, "frozen-create")
	oldHome := filepath.Join(repoRoot, "old-store")
	newHome := filepath.Join(repoRoot, "new-store")

	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: "CODEX_HOME=" + oldHome + " codex"}
	require.NoError(t, config.SaveConfig(cfg))
	inst, err := NewInstance(InstanceOptions{
		Title: "frozen-create", Path: repoRoot, Program: tmux.ProgramCodex, InPlace: true,
	})
	require.NoError(t, err)
	world := newInPlaceTmuxWorld(t)
	inst.SetTmuxSession(tmux.NewTmuxSessionWithDeps("frozen-create", tmux.ProgramCodex, world, world.exec()))

	plan, err := inst.PrepareCreateLaunch()
	require.NoError(t, err)
	require.Equal(t, oldHome, plan.conversationCapture.codexHome)

	cfg.ProgramOverrides[tmux.ProgramCodex] = "CODEX_HOME=" + newHome + " codex"
	require.NoError(t, config.SaveConfig(cfg))
	require.NoError(t, inst.LaunchPreparedCreate(plan))
	t.Cleanup(func() { _ = inst.CloseAttachOnly() })

	world.mu.Lock()
	commands := append([]*exec.Cmd(nil), world.commands...)
	world.mu.Unlock()
	var launched string
	for _, command := range commands {
		joined := strings.Join(command.Args, " ")
		if strings.Contains(joined, "new-session") && strings.Contains(joined, "codex") {
			launched = joined
			break
		}
	}
	require.Contains(t, launched, oldHome, "launch must consume the command frozen before config changed")
	require.NotContains(t, launched, newHome, "launch must not resolve configuration again after capture")
}

type observingCreatePtyFactory struct {
	base        *inPlaceTmuxWorld
	beforeStart func(*exec.Cmd)
}

func (f *observingCreatePtyFactory) Start(command *exec.Cmd) (*os.File, error) {
	if f.beforeStart != nil {
		f.beforeStart(command)
	}
	return f.base.Start(command)
}

func (f *observingCreatePtyFactory) Close() { f.base.Close() }

func TestPreparedCreateLaunchSnapshotsBeforeAgentProcessStart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initInPlaceRepo(t, "ordered-create")
	codexHome := filepath.Join(repoRoot, "ordered-store")
	const rollout = "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl"

	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: "CODEX_HOME=" + codexHome + " codex"}
	require.NoError(t, config.SaveConfig(cfg))
	inst, err := NewInstance(InstanceOptions{
		Title: "ordered-create", Path: repoRoot, Program: tmux.ProgramCodex, InPlace: true,
	})
	require.NoError(t, err)

	world := newInPlaceTmuxWorld(t)
	pty := &observingCreatePtyFactory{base: world}
	pty.beforeStart = func(command *exec.Cmd) {
		joined := strings.Join(command.Args, " ")
		if strings.Contains(joined, "new-session") && strings.Contains(joined, "codex") {
			writeCodexRolloutFile(t, codexHome, rollout)
		}
	}
	inst.SetTmuxSession(tmux.NewTmuxSessionWithDeps("ordered-create", tmux.ProgramCodex, pty, world.exec()))

	plan, err := inst.PrepareCreateLaunch()
	require.NoError(t, err)
	require.Empty(t, newCodexRolloutFiles(plan.ConversationCapture()),
		"the rollout should not exist when the before-image is taken")
	require.NoError(t, inst.LaunchPreparedCreate(plan))
	t.Cleanup(func() { _ = inst.CloseAttachOnly() })

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, plan.ConversationCapture(), time.Second)
	require.NoError(t, err)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID,
		"a rollout created at process start must be newer than the frozen before-image")
}

func TestPrepareCreateLaunchDoesNotGuessCodexStoreForOpaqueOverride(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "daemon-store"))
	repoRoot := initInPlaceRepo(t, "opaque-create")

	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{
		tmux.ProgramCodex: filepath.Join(t.TempDir(), "opaque-agent-wrapper"),
	}
	require.NoError(t, config.SaveConfig(cfg))
	inst, err := NewInstance(InstanceOptions{
		Title: "opaque-create", Path: repoRoot, Program: tmux.ProgramCodex, InPlace: true,
	})
	require.NoError(t, err)

	plan, err := inst.PrepareCreateLaunch()
	require.NoError(t, err)
	require.Empty(t, plan.conversationCapture.codexHome,
		"a config label is not evidence that an opaque wrapper writes Codex rollouts")
}
