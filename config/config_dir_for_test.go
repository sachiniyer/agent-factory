package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the credential-root cwd-dependence fix at its source:
// ConfigDirFor refuses a relative AGENT_FACTORY_HOME. The acceptance boundary is
// that `--account work` resolves the account directory (and thus the injected
// CODEX_HOME / CLAUDE_CONFIG_DIR / GEMINI_CLI_HOME credential root) against the
// OPERATOR's home regardless of where af runs, and the load-bearing fact is that
// the home an account is resolved against is never a relative string carried into
// a child process whose cwd is not the setter's. Refusing the relative spelling at
// ConfigDirFor closes both observed shapes (a daemon/pane cwd disagreement, and
// running af from inside an attacker's clone) at the one place both share: the
// string "AGENT_FACTORY_HOME" becomes in the first process that reads it.

// relREFUSED enumerates the relative spellings an operator might write to colocate
// af state with a project. Every one must be refused — the cwd-dependence they
// create at the account-selection boundary is not visible to the operator until a
// same-named account is silently substituted, so the input must reject them
// rather than each downstream boundary patching around the value.
func TestConfigDirFor_RelativeHomeRefused(t *testing.T) {
	for _, rel := range []string{
		".af",
		"af-home",
		"./state",
		"rel",
		"foo/bar",
		".",
		"..",
	} {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			got, err := ConfigDirFor(rel)
			assert.Error(t, err, "a relative AGENT_FACTORY_HOME must be refused at the source")
			assert.Empty(t, got, "a refused home must not produce a directory string")
			if err != nil {
				assert.Contains(t, err.Error(), "AGENT_FACTORY_HOME",
					"the error must name the knob so the operator can fix it")
				assert.Contains(t, err.Error(), "absolute",
					"the error must say what the operator must change to an absolute path or ~/…")
				assert.Contains(t, err.Error(), rel,
					"the error must quote the offending value the operator set")
			}
		})
	}
}

// The refusal is cwd-INDEPENDENT — that is the whole point. A relative value is
// rejected without resolving it against the process cwd, so two af invocations
// from two directories both refuse the same spelling (rather than each silently
// landing on a different physical directory). Pinning this against Chdir is what
// distinguishes the refusal from the cwd-dependent resolution it replaces: the
// old behavior returned the same relative string from every cwd and let each
// consumer resolve it differently; the new behavior refuses from every cwd.
func TestConfigDirFor_RelativeHomeRefusedFromAnyCwd(t *testing.T) {
	rel := ".af"
	wd, err := os.Getwd()
	require.NoError(t, err)

	for _, cwd := range []string{wd, os.TempDir(), "/"} {
		cwd := cwd
		t.Run(cwd, func(t *testing.T) {
			t.Chdir(cwd)
			got, err := ConfigDirFor(rel)
			assert.Error(t, err, "the refusal must not depend on the process cwd")
			assert.Empty(t, got)
		})
	}
}

// Whitespace-only and leading-space values are still relative (and not empty):
// they must be refused, not silently trimmed and treated as the default home.
func TestConfigDirFor_RelativeWhitespaceHomeRefused(t *testing.T) {
	for _, rel := range []string{" ", "  ", "\t.af"} {
		rel := rel
		t.Run(strings.TrimSpace(rel)+"_", func(t *testing.T) {
			_, err := ConfigDirFor(rel)
			assert.Error(t, err, "a non-empty whitespace or whitespace-prefixed value is relative, not unset")
		})
	}
}

// The valid spellings the fix must preserve: an absolute path, the tilde forms
// (which ExpandTilde turns absolute), and the empty/unset form (which resolves to
// a default that is absolute by construction). Refusing the relative spelling must
// not widen the refusal to anything an operator could legitimately have meant.
func TestConfigDirFor_ValidHomesStillResolve(t *testing.T) {
	t.Run("absolute path is returned expanded", func(t *testing.T) {
		abs := t.TempDir()
		got, err := ConfigDirFor(abs)
		require.NoError(t, err)
		assert.Equal(t, abs, got)
		assert.True(t, filepath.IsAbs(got))
	})

	t.Run("absolute nested path is returned expanded", func(t *testing.T) {
		abs := filepath.Join(t.TempDir(), "nested", "af-home")
		got, err := ConfigDirFor(abs)
		require.NoError(t, err)
		assert.Equal(t, abs, got)
		assert.True(t, filepath.IsAbs(got))
	})

	t.Run("tilde expands to an absolute home", func(t *testing.T) {
		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)
		got, err := ConfigDirFor("~")
		require.NoError(t, err)
		assert.Equal(t, homeDir, got)
		assert.True(t, filepath.IsAbs(got))
	})

	t.Run("~/path expands to an absolute home", func(t *testing.T) {
		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)
		got, err := ConfigDirFor("~/af-home")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(homeDir, "af-home"), got)
		assert.True(t, filepath.IsAbs(got))
	})

	t.Run("empty resolves to the default absolute home", func(t *testing.T) {
		got, err := ConfigDirFor("")
		require.NoError(t, err)
		assert.True(t, filepath.IsAbs(got), "the default home must be absolute by construction")
		assert.True(t, strings.HasSuffix(got, ".agent-factory"))
	})
}

// The malformed ~user forms are still rejected with their own message — the new
// IsAbs guard runs AFTER the tilde checks, so a ~user value is reported as an
// invalid tilde (the actionable diagnosis) rather than as a not-absolute path.
func TestConfigDirFor_MalformedTildeStillRejectedBeforeIsAbs(t *testing.T) {
	for _, malformed := range []string{"~.config", "~config"} {
		malformed := malformed
		t.Run(malformed, func(t *testing.T) {
			_, err := ConfigDirFor(malformed)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid tilde format",
				"a ~user form must keep its tilde-specific diagnosis under the new guard")
		})
	}
}

// filepath.IsAbs accepts per-process procfs magic symlinks such as
// /proc/self/cwd, but those resolve relative to the READING process — the
// daemon and the pane shim inherit the same AGENT_FACTORY_HOME and read it
// from different processes (the pane runs in the session worktree), so each
// resolves /proc/self/cwd to a different physical directory. That re-opens the
// cwd-dependence the IsAbs guard closes: a daemon started from the operator's
// directory resolves its real account, while the pane resolves the same
// accepted value to an attacker-controlled <worktree>/.agent-factory and
// injects a planted same-named account. Reject the per-process self/thread-self
// aliases; a concrete /proc/<pid>/cwd is stable and stays valid.
func TestConfigDirFor_ProcessRelativeProcfsHomeRefused(t *testing.T) {
	for _, home := range []string{
		"/proc/self/cwd/.agent-factory",
		"/proc/self/cwd",
		"/proc/self/fd/3/.af",
		"/proc/thread-self/cwd/.af",
		"/proc/thread-self/root",
		// obfuscations that Clean normalizes back to a per-process alias
		"/proc/./self/cwd/.af",
		"/proc/self/../self/cwd/.af",
	} {
		home := home
		t.Run(home, func(t *testing.T) {
			got, err := ConfigDirFor(home)
			assert.Error(t, err, "a per-process procfs AGENT_FACTORY_HOME must be refused at the source")
			assert.Empty(t, got)
			if err != nil {
				assert.Contains(t, err.Error(), "AGENT_FACTORY_HOME",
					"the error must name the knob so the operator can fix it")
				assert.Contains(t, err.Error(), "procfs",
					"the error must say why a syntactically-absolute procfs path is still rejected")
			}
		})
	}

	// A concrete /proc/<pid>/cwd is stable for a given pid and must NOT be
	// rejected — only the per-process self/thread-self aliases are unsafe.
	t.Run("concrete /proc/<pid>/cwd is accepted", func(t *testing.T) {
		got, err := ConfigDirFor("/proc/1/cwd/.agent-factory")
		require.NoError(t, err)
		assert.Equal(t, "/proc/1/cwd/.agent-factory", got)
	})
}

// GetConfigDir is the entry the binary wires into the account lookup: AGENT_FACTORY_HOME
// from THIS process's environment, not a caller-supplied value. Pin that the env path
// refuses a relative value the same way ConfigDirFor does — the lookup hook in main.go
// calls GetConfigDir(), so this is the gate the credential-root resolution actually
// crosses.
func TestGetConfigDir_RelativeEnvRefused(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", ".af")
	_, err := GetConfigDir()
	assert.Error(t, err, "a relative AGENT_FACTORY_HOME in the environment must be refused")
}

// The exploit chain is broken at the source. The account lookup the binary wires
// (main.go: install AccountLookup = func(agent, name) { home, _ := GetConfigDir();
// agentaccount.Selected(home, agent, name) }) resolves the account directory against
// the home GetConfigDir returns. With a relative AGENT_FACTORY_HOME, GetConfigDir now
// REFUSES, so Selected is never called and never builds a relative account.Dir that a
// pane shim would inject as CODEX_HOME. Reproduce the production lookup exactly and
// assert it refuses for a relative home — the same lookup from two cwds must both
// refuse (the old behavior returned the same relative string from both and let each
// child resolve it differently against its own cwd, the silent substitution).
//
// To prove it is the HOME refusal that refuses (not a missing account directory),
// the planted account directory is created on disk under each cwd so a relative
// home WOULD resolve a registered account there — without the fix, the lookup
// silently succeeds against the planted dir; with the fix, it never reaches
// Selected. That is the silent-substitution shape the fix closes.
func TestAccountLookup_RelativeHomeRefusesBeforeSelected(t *testing.T) {
	lookup := func(agent, name string) (sessionenv.Account, error) {
		home, err := GetConfigDir()
		if err != nil {
			return sessionenv.Account{}, err
		}
		return agentaccount.Selected(home, agent, name)
	}

	wd, err := os.Getwd()
	require.NoError(t, err)

	for _, cwd := range []string{wd, os.TempDir()} {
		cwd := cwd
		t.Run(cwd, func(t *testing.T) {
			// Plant the account directory the relative home would resolve to,
			// so the lookup would SUCCEED against the planted account if the
			// home were accepted. The fix must refuse before that happens.
			t.Chdir(cwd)
			// Use a unique relative home per subtest rather than the fixed
			// ".af": this subtest runs from both the repo directory and
			// os.TempDir() and removes the planted tree on cleanup, so a fixed
			// name could clobber a real .af a developer placed in either. The
			// name only needs to be relative for the refusal to exercise the
			// account-lookup path; it never resolves on disk because the home
			// is refused first. filepath.Base(t.TempDir()) alone is just the
			// per-test sequence (e.g. "001"), which is not unique across a
			// developer's real directories either, so carry the parent's base —
			// it bears t.TempDir()'s random suffix — to make the relative home
			// unique and keep the cleanup from removing a real directory.
			tmp := t.TempDir()
			relHome := filepath.Join(filepath.Base(filepath.Dir(tmp)), filepath.Base(tmp))
			planted := filepath.Join(relHome, "accounts", "codex", "work")
			require.NoError(t, os.MkdirAll(planted, 0o700))
			// A marker file a real agent's auth.json would occupy; the agent
			// authenticates with whatever lives here. The lookup must never
			// reach the directory that holds it.
			require.NoError(t, os.WriteFile(filepath.Join(planted, "auth.json"),
				[]byte("ATTACKER-PLANTED"), 0o600))
			t.Cleanup(func() { _ = os.RemoveAll(relHome) })

			t.Setenv("AGENT_FACTORY_HOME", relHome)
			acct, err := lookup("codex", "work")
			assert.Error(t, err, "the production account lookup must refuse a relative home before Selected builds a Dir")
			assert.Equal(t, sessionenv.Account{}, acct, "no account is returned when the home is refused")
		})
	}
}

// And the happy path the fix must not regress: an absolute AGENT_FACTORY_HOME lets
// the lookup resolve a registered account whose Dir is absolute — and the SAME
// absolute Dir is returned from two different cwds (the property the relative
// spelling broke: cwd-independence of `--account work`).
func TestAccountLookup_AbsoluteHomeResolvesCwdIndependent(t *testing.T) {
	home := t.TempDir()
	const agent, name = "codex", "work"
	require.NoError(t, os.MkdirAll(filepath.Join(home, "accounts", agent, name), 0o700))

	lookup := func(agent, name string) (sessionenv.Account, error) {
		h, err := GetConfigDir()
		if err != nil {
			return sessionenv.Account{}, err
		}
		return agentaccount.Selected(h, agent, name)
	}

	wd, err := os.Getwd()
	require.NoError(t, err)

	var first sessionenv.Account
	for _, cwd := range []string{wd, os.TempDir()} {
		cwd := cwd
		t.Run(cwd, func(t *testing.T) {
			t.Chdir(cwd)
			t.Setenv("AGENT_FACTORY_HOME", home)
			acct, err := lookup(agent, name)
			require.NoError(t, err)
			require.Equal(t, agent, acct.Agent)
			require.Equal(t, name, acct.Name)
			assert.True(t, filepath.IsAbs(acct.Dir), "an absolute home must produce an absolute account Dir")
			if first.Dir == "" {
				first = acct
				return
			}
			assert.Equal(t, first.Dir, acct.Dir,
				"the same account from the same absolute home must resolve to the SAME Dir regardless of cwd")
		})
	}
}
