package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const setConversationID = "019f386f-7206-7fc2-803b-f7045e07a299"

// setConversationInstance is a bare local codex session whose launch directory
// is workDir — enough for the validator's program/store/cwd resolution.
func setConversationInstance(t *testing.T, workDir string) *Instance {
	t.Helper()
	gw, err := sessiongit.NewGitWorktreeFromStorage(workDir, workDir, "t", "main", "", false, true)
	require.NoError(t, err)
	return &Instance{Title: "t", Program: "codex", Path: workDir, gitWorktree: gw}
}

func setConversationConfig(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{"codex": "codex"}
	require.NoError(t, config.SaveConfig(cfg))
}

// A session whose workspace runs off-box (ssh, sandbox, docker, remote) keeps
// its Codex rollout store on the remote host, where the daemon cannot read it.
// The validator refuses before any store access — this is the constraint that
// makes set-conversation unservable for the sandbox scope's off-box targets
// (#6805): every backend authorizeSandboxSession admits reconstructs with
// WorkspaceRemote, so the authorized set and the validatable set have an empty
// intersection. The route was removed from the sandbox scope rather than
// admitting a verb no sandbox-credentialed request can complete.
func TestValidateSettableConversation_RefusesRemoteWorkspace(t *testing.T) {
	setConversationConfig(t)
	workDir := t.TempDir()
	for _, backend := range []string{"ssh", "sandbox", "docker", "remote"} {
		inst := sandboxInstanceOn(t, backend, nil)
		inst.Path = workDir
		assert.Equal(t, WorkspaceRemote, inst.Capabilities().Workspace,
			"backend %q reconstructs with WorkspaceRemote", backend)
		_, err := ValidateSettableConversation(inst, setConversationID)
		require.Error(t, err, "backend %q: a remote workspace is not inspectable from here", backend)
		assert.Contains(t, err.Error(), "runs off this host",
			"backend %q: the refusal names the store-owning host", backend)
	}
}

func TestValidateSettableConversation_HappyPath(t *testing.T) {
	setConversationConfig(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", workDir)

	conv, err := ValidateSettableConversation(setConversationInstance(t, workDir), setConversationID)
	require.NoError(t, err)
	assert.Equal(t, tmux.ProgramCodex, conv.Agent)
	assert.Equal(t, setConversationID, conv.ID)
	assert.Equal(t, ConversationCaptureOperator, conv.CaptureKind,
		"a repair records its own kind so it keeps standing across a restart (#7120)")
	assert.False(t, conv.CapturedAt.IsZero())
}

func TestValidateSettableConversation_RefusesBeforeStore(t *testing.T) {
	setConversationConfig(t)
	workDir := t.TempDir()
	inst := setConversationInstance(t, workDir)

	for _, tc := range []struct {
		name string
		id   string
		want string
	}{
		{"empty id", "", "a conversation id is required"},
		{"whitespace id", "   ", "a conversation id is required"},
		{"not a uuid", "old-chat", "is not a Codex conversation id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateSettableConversation(inst, tc.id)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateSettableConversation_RefusesNonCodex(t *testing.T) {
	setConversationConfig(t)
	workDir := t.TempDir()
	gw, err := sessiongit.NewGitWorktreeFromStorage(workDir, workDir, "t", "main", "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "t", Program: "claude", Path: workDir, gitWorktree: gw}

	_, err = ValidateSettableConversation(inst, setConversationID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "can only inspect a Codex rollout store")
}

func TestValidateSettableConversation_RefusesMissingRollout(t *testing.T) {
	setConversationConfig(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	// The store is real but holds no rollout under the typed id.
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-33-aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa.jsonl", workDir)

	_, err := ValidateSettableConversation(setConversationInstance(t, workDir), setConversationID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no rollout named "+setConversationID)
}

func TestValidateSettableConversation_RefusesUnclassifiable(t *testing.T) {
	setConversationConfig(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	// writeCodexRolloutFile plants a first record that is not session_meta —
	// exactly the header a bound correlation cannot classify.
	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl")

	_, err := ValidateSettableConversation(setConversationInstance(t, workDir), setConversationID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be read or classified")
}

func TestValidateSettableConversation_RefusesSubAgent(t *testing.T) {
	setConversationConfig(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	writeCodexRolloutFileWithSource(t, codexHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", workDir,
		map[string]any{"subagent": "review"})

	_, err := ValidateSettableConversation(setConversationInstance(t, workDir), setConversationID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sub-agent thread")
}

func TestValidateSettableConversation_RefusesForeignWorktree(t *testing.T) {
	setConversationConfig(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", t.TempDir())

	_, err := ValidateSettableConversation(setConversationInstance(t, workDir), setConversationID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in session")
}

func TestValidateSettableConversation_TopLevelSourcesAdmitted(t *testing.T) {
	setConversationConfig(t)
	for _, source := range []any{"cli", "exec", "vscode", nil} {
		codexHome := t.TempDir()
		workDir := t.TempDir()
		writeCodexRolloutFileWithSource(t, codexHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", workDir, source)
		t.Setenv("CODEX_HOME", codexHome)

		_, err := ValidateSettableConversation(setConversationInstance(t, workDir), setConversationID)
		require.NoError(t, err, "source %v is a top-level launch, not a sub-agent", source)
	}
}

func TestValidateSettableConversation_ReadsOnlyFirstRecord(t *testing.T) {
	setConversationConfig(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	// A second record carrying prompt content must not be consulted: the
	// validator classifies only the first session_meta line.
	path := filepath.Join(codexHome, "sessions", "2026", "07", "06", "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	data := `{"type":"session_meta","payload":{"cwd":` + `"` + workDir + `"` + `}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","message":"corrupt json but never read }}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0644))

	_, err := ValidateSettableConversation(setConversationInstance(t, workDir), setConversationID)
	require.NoError(t, err)
}

func TestValidateSettableConversation_AccountStore(t *testing.T) {
	setConversationConfig(t)
	workDir := t.TempDir()
	inst := setConversationInstance(t, workDir)
	accountDir := registerAccount(t, "codex", "work")
	inst.Account = "work"
	// The rollout lives ONLY in the account's registry dir — the ambient home
	// stays empty and would report the id missing.
	t.Setenv("CODEX_HOME", t.TempDir())
	writeCodexRolloutFileWithCwd(t, accountDir, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", workDir)

	conv, err := ValidateSettableConversation(inst, setConversationID)
	require.NoError(t, err)
	assert.Equal(t, setConversationID, conv.ID)
}

// A program_overrides edit after launch must not re-point the repair: the
// validator binds the runtime's recorded command, which carries the store the
// pane actually writes to.
func TestValidateSettableConversation_BindsRuntimeProgram(t *testing.T) {
	setConversationConfig(t)
	runtimeHome := t.TempDir()
	t.Setenv("CODEX_HOME", t.TempDir()) // ambient store stays empty
	workDir := t.TempDir()
	writeCodexRolloutFileWithCwd(t, runtimeHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", workDir)

	inst := setConversationInstance(t, workDir)
	inst.setRuntimeProgram("CODEX_HOME=" + runtimeHome + " codex")

	conv, err := ValidateSettableConversation(inst, setConversationID)
	require.NoError(t, err)
	assert.Equal(t, setConversationID, conv.ID,
		"the store the running command declares is where the rollout lives")
}

// The sibling guarantee — a capture that discovers a rollout must not replace
// an id the operator just recorded — lives in the daemon's commit path under
// the op-lock: TestCaptureAgentConversationSkipsARecordedRepair (CI per the
// daemon-test rule).

// The provider is what the launch command resolves to, not the configured
// enum (#6769 review): a program_overrides mapping and a legacy Program that
// IS the binary path are Codex sessions whose stores af can prove — the same
// identity resolvedAgentLocked uses for capture.
func TestValidateSettableConversation_IdentifiesCodexFromTheLaunchCommand(t *testing.T) {
	t.Run("override maps the configured agent to codex", func(t *testing.T) {
		setConversationConfig(t)
		cfg := config.DefaultConfig()
		cfg.ProgramOverrides = map[string]string{"aider": "codex"}
		require.NoError(t, config.SaveConfig(cfg))
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", workDir)

		inst := setConversationInstance(t, workDir)
		inst.Program = "aider"
		conv, err := ValidateSettableConversation(inst, setConversationID)
		require.NoError(t, err)
		assert.Equal(t, setConversationID, conv.ID)
	})

	t.Run("legacy Program is the codex binary path", func(t *testing.T) {
		setConversationConfig(t)
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", workDir)

		inst := setConversationInstance(t, workDir)
		inst.Program = filepath.Join(t.TempDir(), "bin", "codex")
		conv, err := ValidateSettableConversation(inst, setConversationID)
		require.NoError(t, err)
		assert.Equal(t, setConversationID, conv.ID)
	})

	t.Run("a genuinely non-codex launch still refuses", func(t *testing.T) {
		setConversationConfig(t)
		workDir := t.TempDir()
		inst := setConversationInstance(t, workDir)
		inst.Program = "claude"
		_, err := ValidateSettableConversation(inst, setConversationID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "can only inspect a Codex")
	})
}

// Two rollout files carrying the same id cannot be told apart (#6769 review):
// the repair refuses rather than record an id the restore guard would reject
// as ambiguous at the next resume.
func TestValidateSettableConversation_RefusesAnAmbiguousID(t *testing.T) {
	setConversationConfig(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+setConversationID+".jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-07T11-20-00-"+setConversationID+".jsonl", workDir)

	_, err := ValidateSettableConversation(setConversationInstance(t, workDir), setConversationID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot prove which conversation")
}
