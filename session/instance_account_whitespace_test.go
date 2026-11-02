package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validateAccountName normalizes a whitespace-only account to "" (no account,
// ambient identity) rather than letting it flow through as a raw string the
// gates treat as absent while the record treats as present.
func TestValidateAccountName_WhitespaceOnlyBecomesEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"single space", " "},
		{"multiple spaces", "   "},
		{"tab", "\t"},
		{"newline", "\n"},
		{"mixed whitespace", " \t\n "},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateAccountName(tc.input)
			require.NoError(t, err)
			assert.Empty(t, got,
				"a whitespace-only account must normalize to empty (ambient), not an error")
		})
	}
}

// validateAccountName trims surrounding whitespace from a valid name, preserving
// the "trim around a real name" tolerance the original gates used TrimSpace for.
func TestValidateAccountName_TrimsSurroundingWhitespaceFromValidName(t *testing.T) {
	got, err := validateAccountName("  work  ")
	require.NoError(t, err)
	assert.Equal(t, "work", got)
}

// validateAccountName refuses a non-empty value that cannot be a registered
// account name, so the invariant holds: a stored non-empty Account is always a
// ValidateName-valid name.
func TestValidateAccountName_RefusesInvalidName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"leading dot", ".work"},
		{"shell metacharacter", "work!"},
		{"path separator", "work/personal"},
		{"starts with dash", "-work"},
		{"too long", "a" + string(rune('a')) + string(make([]byte, 64))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateAccountName(tc.input)
			require.Error(t, err,
				"a non-empty value that fails ValidateName must be refused at admission")
			assert.Contains(t, err.Error(), "invalid account",
				"the refusal must name the account as invalid")
		})
	}
}

// A whitespace-only account must be admitted as ambient — the create succeeds with
// no account scoping — and the Instance must record Account="" rather than the raw
// whitespace string. This is the core of the bug: the gates' TrimSpace test
// admitted " " while the record stored it raw, so the session ran unscoped while
// every display surface reported it as account-scoped.
func TestNewInstance_WhitespaceAccountNormalizedToAmbientOnLocalBackend(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := initTempGitRepo(t)

	restore := SetBackendFactoryForTest(func(InstanceOptions, string) (Backend, error) {
		return &LocalBackend{}, nil
	})
	defer restore()

	inst, err := NewInstance(InstanceOptions{
		Title:   "ws-local",
		Path:    repo,
		Program: "codex",
		Account: " ",
	})
	require.NoError(t, err,
		"a whitespace-only account must be admitted (as ambient), not refused")
	require.NotNil(t, inst)
	assert.Empty(t, inst.Account,
		"a whitespace-only account must be normalized to empty on the Instance, "+
			"not stored raw — the record must not claim an account was selected")
	assert.Empty(t, inst.AccountAgent(),
		"no account agent is recorded for an ambient session")
}

// A whitespace-only account on a non-carrying backend (ssh) must be admitted as
// ambient — the off-box gate sees the normalized empty value and returns nil —
// rather than being stored raw while the session runs on ambient credentials.
func TestNewInstance_WhitespaceAccountNormalizedToAmbientOnSshBackend(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := initTempGitRepo(t)
	writeInRepoConfig(t, repo, map[string]any{
		"backend": "ssh",
		"ssh":     map[string]any{"host": "example.invalid"},
	})

	factoryReached := false
	restore := SetBackendFactoryForTest(func(InstanceOptions, string) (Backend, error) {
		factoryReached = true
		return &LocalBackend{}, nil
	})
	defer restore()

	inst, err := NewInstance(InstanceOptions{
		Title:   "ws-ssh",
		Path:    repo,
		Program: "codex",
		Account: " ",
	})
	require.NoError(t, err,
		"a whitespace-only account must be admitted as ambient on ssh, not refused "+
			"by the off-box gate (which sees the normalized empty value)")
	require.NotNil(t, inst)
	assert.Empty(t, inst.Account,
		"the record must not claim an account was selected for an ambient session")
	assert.True(t, factoryReached,
		"the factory must be reached — the gate must not refuse an ambient session")
}

// A valid account with surrounding whitespace must be trimmed and stored as the
// trimmed name, so the recorded Account is always a ValidateName-valid name.
func TestNewInstance_ValidAccountWithSurroundingWhitespaceIsTrimmed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	_, err := agentaccount.Register(home, tmux.ProgramCodex, "work")
	require.NoError(t, err)
	repo := initTempGitRepo(t)

	restore := SetBackendFactoryForTest(func(InstanceOptions, string) (Backend, error) {
		return &LocalBackend{}, nil
	})
	defer restore()

	inst, err := NewInstance(InstanceOptions{
		Title:   "ws-trim",
		Path:    repo,
		Program: "codex",
		Account: "  work  ",
	})
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Equal(t, "work", inst.Account,
		"the recorded Account must be the trimmed name, not the raw string")
	assert.Equal(t, tmux.ProgramCodex, inst.AccountAgent(),
		"the account agent must be recorded for a scoped session")
}

// An invalid (non-whitespace) account name must be refused at NewInstance
// before any state is committed, so the fail-closed rule holds for every
// invalid account input — not just unregistered ones.
func TestNewInstance_InvalidAccountNameRefusedBeforeProvisioning(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := initTempGitRepo(t)

	factoryReached := false
	restore := SetBackendFactoryForTest(func(InstanceOptions, string) (Backend, error) {
		factoryReached = true
		return &LocalBackend{}, nil
	})
	defer restore()

	_, err := NewInstance(InstanceOptions{
		Title:   "ws-invalid",
		Path:    repo,
		Program: "codex",
		Account: "work!",
	})
	require.Error(t, err,
		"an invalid account name must be refused at admission, not stored or provisioned")
	assert.Contains(t, err.Error(), "invalid account",
		"the refusal must name the account as invalid")
	assert.False(t, factoryReached,
		"the factory must not be reached for a refused account")
}
