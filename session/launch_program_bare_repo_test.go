package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// Regression for the bare-repo + linked-worktree proof mismatch: the launcher
// resolves operator config from Instance.Path, which for a bare-repo session is
// the bare directory — the identity root, which owns identity but has no
// checked-out files, so ResolveConfigForRepo reads no checked-in config from
// it. The pane runs in a linked worktree whose RepoFromPath keeps the worktree
// as Root and the bare dir as IdentityRoot: a shim that re-derives from the
// worktree would read a checked-in program override the launcher never saw,
// compute a different proof, and refuse every otherwise-valid launch (#review,
// Codex P2 on 458eb57 — session/launch_program.go:88). resolveResolvedConfigForPath
// must resolve from the identity root the launcher used, so a worktree pane and
// the bare launcher read the SAME operator config and derive the same proof.
func TestResolveAccountLaunchProof_BareRepoWorktreeReadsIdentityRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	// A global config.toml keeps LoadConfig off the first-run default
	// materialization (whose GetClaudeCommand probe shells out) and gives the
	// identity-root resolution a deterministic effective DefaultProgram.
	require.NoError(t, os.WriteFile(
		filepath.Join(testguard.SocketTempDir(t), config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n"), 0o644))

	parent := testguard.CanonicalTempDir(t)
	source := filepath.Join(parent, "source")
	bare := filepath.Join(parent, "bare.git")
	worktree := filepath.Join(parent, "worktree")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
	}
	run(parent, "init", source)
	run(source, "config", "user.email", "test@test.com")
	run(source, "config", "user.name", "Test")
	run(source, "commit", "--allow-empty", "-m", "init")
	run(parent, "clone", "--bare", source, bare)
	run(bare, "worktree", "add", worktree)

	// A checked-in override that lives in the worktree, NOT the bare dir. A
	// shim re-resolving from the worktree reads this; the launcher (resolving
	// from the bare dir) does not, and the two proofs would diverge.
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, config.InRepoConfigDirName), 0o755))
	require.NoError(t, os.WriteFile(config.InRepoTomlConfigPath(worktree),
		[]byte("default_program = \"codex\"\n"), 0o644))

	// Precondition: the worktree's checked-in override IS picked up when
	// resolving from the worktree (WorkspacePath), proving the override is real
	// and the assertion below is not vacuous. The bare dir has no checked-out
	// files, so it resolves to the global default.
	worktreeRepo, err := config.RepoFromPath(worktree)
	require.NoError(t, err)
	require.Equal(t, worktree, worktreeRepo.Root, "a bare repo's linked worktree keeps the worktree as Root")
	require.Equal(t, bare, worktreeRepo.IdentityPath(), "the bare dir is the identity root")
	worktreeResolved, err := config.ResolveConfigForRepo(worktreeRepo)
	require.NoError(t, err)
	require.Equal(t, "codex", worktreeResolved.DefaultProgram, "precondition: the worktree carries the checked-in override")

	bareRepo, err := config.RepoFromPath(bare)
	require.NoError(t, err)
	require.Equal(t, bare, bareRepo.Root, "the bare dir resolves to itself as Root (no checked-out config)")

	// The fix: a pane standing in the worktree resolves from the identity root
	// (the bare dir) — the same root the launcher used — so it does NOT see the
	// worktree's checked-in override and matches the bare-dir derivation. This
	// is recovered from the pane's own git context, not carried by the
	// launcher, so it stays outside the forgeable-env channel the re-derivation
	// defends.
	paneResolved, err := resolveResolvedConfigForPath(worktree)
	require.NoError(t, err)
	require.NotNil(t, paneResolved)
	bareResolved, err := resolveResolvedConfigForPath(bare)
	require.NoError(t, err)
	require.NotNil(t, bareResolved)
	assert.Equal(t, bareResolved.DefaultProgram, paneResolved.DefaultProgram,
		"a bare-repo worktree pane and the bare launcher must read the same operator config and derive the same proof")
	assert.Equal(t, "claude", paneResolved.DefaultProgram,
		"the worktree's checked-in override must not reach a pane that resolves from the identity root")
}

// Regression for the malformed-config attack surface of Codex P1 on
// 4302e0e7 (session/launch_program.go:90): a same-uid child of the agent — the
// same threat that can re-invoke af under the marker — can also corrupt the
// operator's config file. When both the repo and global config layers then fail
// to resolve (e.g. an unparseable config.toml), ResolveAccountLaunchProof must
// propagate the underlying error so the shim REFUSES the launch rather than
// returning (zero, nil). The prior code converted the unreachable config into a
// successful zero proof, which a child matched by submitting an empty
// __AF_ACCOUNT_LAUNCH_PROOF and then let /bin/sh resolve an attacker-controlled
// agent from PATH while applyAccountScope injected the selected account
// credentials. The resolver must fail closed.
func TestResolveAccountLaunchProof_RefusesLaunchOnUnreachableConfig(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// A non-empty, unparseable config.toml forces LoadConfig to error loudly
	// (#734 — defaults are NOT substituted for a present-but-broken file),
	// and home is not a git repo so RepoFromPath fails on it. The global
	// layer — the only fallback after the repo branch — is therefore the
	// only path resolution takes, and it errors instead of substituting
	// defaults from a config the operator never wrote.
	require.NoError(t, os.WriteFile(
		filepath.Join(home, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\npost_worktree_commands = [\n"), 0o644))

	t.Chdir(home)

	// resolveResolvedConfigForPath surfaces the underlying resolution error
	// instead of returning (nil, nil).
	resolved, err := resolveResolvedConfigForPath(home)
	require.Error(t, err, "an unreachable operator config must propagate an error, not a silent zero proof")
	require.Nil(t, resolved)

	// ResolveAccountLaunchProof propagates that error so the shim refuses
	// the launch rather than treating the unreachable config as a zero
	// proof an attacker-supplied empty env var could match.
	proof, err := ResolveAccountLaunchProof("claude", "work", "claude")
	require.Error(t, err, "an unreachable operator config must refuse the scope, not produce a zero proof")
	require.Empty(t, proof, "a refused derivation must not hand back a usable proof")
}
