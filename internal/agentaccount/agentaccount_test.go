package agentaccount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// EXCLUSIVITY, not presence. Two accounts must genuinely resolve to different
// credential directories and a session must receive exactly one — the assertion
// a passthrough implementation fails, since it would hand every session the
// daemon's single ambient value while each looked correct in isolation (#3051).
func TestSelected_TwoAccountsSeeDifferentDirectories(t *testing.T) {
	home := t.TempDir()
	dirA, err := Register(home, "codex", "work")
	require.NoError(t, err)
	dirB, err := Register(home, "codex", "personal")
	require.NoError(t, err)
	require.NotEqual(t, dirA, dirB)

	ambient := []string{
		"CODEX_HOME=/home/op/.codex",
		"OPENAI_API_KEY=sk-ambient",
		"PATH=/usr/bin",
	}

	accountA, err := Selected(home, "codex", "work", "")
	require.NoError(t, err)
	scopedA, err := sessionenv.ApplyAccount(ambient, "codex", accountA)
	require.NoError(t, err)

	accountB, err := Selected(home, "codex", "personal", "")
	require.NoError(t, err)
	scopedB, err := sessionenv.ApplyAccount(ambient, "codex", accountB)
	require.NoError(t, err)

	homeA := envValue(t, scopedA, "CODEX_HOME")
	homeB := envValue(t, scopedB, "CODEX_HOME")
	require.NotEqual(t, homeA, homeB,
		"two concurrent sessions on different accounts must see different credential roots")
	require.Equal(t, dirA, homeA)
	require.Equal(t, dirB, homeB)

	// SUBTRACTION, the load-bearing half: an ambient key outranks the config
	// directory, so if it survived, the selection would be silently ignored while
	// every visible signal said it worked.
	for _, scoped := range [][]string{scopedA, scopedB} {
		require.NotContains(t, strings.Join(scoped, "\n"), "OPENAI_API_KEY=",
			"an ambient credential must not reach a session that selected an account")
	}

	// A session on one account cannot reach the other's directory: they are
	// separate paths, and only one is ever named.
	require.NotContains(t, strings.Join(scopedA, "\n"), dirB)
	require.NotContains(t, strings.Join(scopedB, "\n"), dirA)
}

// No account selected must leave the session exactly as it was before this
// feature existed — never a silent fallback onto some registered account.
func TestSelected_NoSelectionIsNotAnAccount(t *testing.T) {
	home := t.TempDir()
	_, err := Register(home, "codex", "work")
	require.NoError(t, err)

	account, err := Selected(home, "codex", "", "")
	require.NoError(t, err)
	require.Equal(t, sessionenv.Account{}, account,
		"an unselected session must carry no account, not the first registered one")
}

// An unregistered name REFUSES rather than being created on demand. An empty
// directory has no credentials in it, so launching against it would start an
// unauthenticated agent while the UI reported the selected account.
func TestSelected_RefusesAnUnregisteredAccount(t *testing.T) {
	home := t.TempDir()
	_, err := Selected(home, "codex", "never-registered", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not registered")
	require.Contains(t, err.Error(), "af accounts add", "the error must say how to fix it")
}

// Agents whose credential relocation was never verified refuse, rather than
// accepting a selection that would do nothing.
func TestDir_RefusesAnUnverifiedAgent(t *testing.T) {
	for _, agent := range []string{"gemini", "amp", "aider", "unknown"} {
		_, err := Dir(t.TempDir(), agent, "work")
		require.ErrorIs(t, err, ErrUnsupportedAgent, "agent %q", agent)
	}
}

// A name must be one path component. "." and ".." resolve to the account root
// and the AF home, so a create there would scatter agent config over af's state.
func TestValidateName_RejectsTraversalAndSeparators(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "../escape", "/abs", "-leading", strings.Repeat("x", 65)} {
		require.Error(t, ValidateName(name), "name %q must be refused", name)
	}
	for _, name := range []string{"work", "personal-2", "team.eu", "a_b", "A1"} {
		require.NoError(t, ValidateName(name), "name %q is ordinary and must be accepted", name)
	}
}

// The credential directory is owner-only whatever the operator's umask. af does
// not read these bytes, but it chose where they sit.
func TestRegister_CreatesAnOwnerOnlyDirectory(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "claude", "work")
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	require.Equal(t, filepath.Join(home, DirName, "claude", "work"), dir)

	// Idempotent: registering again is not an error, because af only makes the
	// place and the agent's own login fills it.
	again, err := Register(home, "claude", "work")
	require.NoError(t, err)
	require.Equal(t, dir, again)
}

func TestList_EmptyOnAFreshHomeAndSortedAfterRegistration(t *testing.T) {
	home := t.TempDir()
	names, err := List(home, "codex")
	require.NoError(t, err, "no accounts registered is an ordinary state, not a failure")
	require.Empty(t, names)

	for _, name := range []string{"zulu", "alpha"} {
		_, err := Register(home, "codex", name)
		require.NoError(t, err)
	}
	names, err = List(home, "codex")
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "zulu"}, names)
}

// Same precedence as every other key, and an empty result means "none".
func TestResolve_PrefersExplicitThenProjectThenGlobal(t *testing.T) {
	require.Equal(t, "flag", Resolve("flag", "project", "global"))
	require.Equal(t, "project", Resolve("", "project", "global"))
	require.Equal(t, "global", Resolve("", "  ", "global"))
	require.Equal(t, "", Resolve("", "", ""))
}

func envValue(t *testing.T, env []string, name string) string {
	t.Helper()
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	t.Fatalf("%s not present in the scoped environment", name)
	return ""
}
