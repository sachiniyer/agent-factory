package git

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests pin the fix for the docker credentialed git config-route bug:
// afGitScopedEnvironment (session/git/worktree_git.go) preserves an
// operator-forwarded GIT_CONFIG_COUNT + GIT_CONFIG_KEY_* / GIT_CONFIG_VALUE_*
// (and GIT_CONFIG_PARAMETERS) when the operator forwarded each name WITH a
// host value through AF_GIT_FORWARDED (#7903, the route issue #7891 was about
// — http.sslCAInfo and friends riding the count). But the only production
// consumer of that slice, runGitCommandContextWithEnvironment, then ran it
// through repositoryPathEnvironment, which strips every count-family entry
// unconditionally — a strip added in #4202 to block AMBIENT repo-local
// selection, months before the AF_GIT_FORWARDED claim system existed, with no
// notion of an operator claim. The forwarded count route was therefore
// silently inert for af's own credentialed git (setup fetch, archive push,
// worktree push): dead plumbing as committed.
//
// The fix, reinsertForwardedGitConfigCount, re-applies the count entries the
// operator claimed after the strip — claim-aware, the same per-slot rule the
// rest of the marker system uses — so a complete forwarded count delivers,
// an ambient count (no AF_GIT_FORWARDED claim) stays stripped (#4202's
// protection preserved), and an incomplete forwarded count stays neutralized
// (NeutralizeIncompleteGitConfigCount's empty pin survives). The tests below
// exercise the predicate, the three-layer composition af's own git actually
// runs (afGitScopedEnvironment → repositoryPathEnvironment →
// reinsertForwardedGitConfigCount), and the runner through a real git child
// so the operator's config is observed to deliver end-to-end.

// afGitScopedEnvForTest builds the environment af's own git runs under the
// af-git lockdown, mirroring gitCommandEnvironment without sessionenv.Filter:
// the count entries are placed directly in in, so the test does not depend on
// the hook-passthrough allowlist. It sets the AF_GIT_CONFIG / AF_GIT_FORWARDED
// markers, runs in through afGitScopedEnvironment, and appends the stable
// diagnostics gitCommandEnvironment adds (GIT_TERMINAL_PROMPT=0, LC_ALL=C).
func afGitScopedEnvForTest(t *testing.T, configFile, forwarded string, in []string) []string {
	t.Helper()
	t.Setenv(afGitConfigEnvName, configFile)
	t.Setenv(afGitForwardedEnvName, forwarded)
	return append(afGitScopedEnvironment(in), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

// emptyAfGitConfigFile creates an empty file af's git reads as its global
// config under the lockdown (GIT_CONFIG_GLOBAL points here), so the only
// source of a probed config key is the forwarded GIT_CONFIG_COUNT pair.
func emptyAfGitConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "af-gitconfig")
	require.NoError(t, os.WriteFile(p, nil, 0o600))
	return p
}

func TestAfGitForwardedConfigCountName(t *testing.T) {
	forwarded := map[string]bool{
		"GIT_CONFIG_COUNT":          true,
		"GIT_CONFIG_PARAMETERS":     true,
		"GIT_CONFIG_KEY_0":          true,
		"GIT_CONFIG_VALUE_0":        true,
		"GIT_CONFIG_KEY_2147483647": true,
		"GIT_CONFIG_GLOBAL":         true,
	}
	for name, want := range map[string]bool{
		// count-family names that are forwarded qualify.
		"GIT_CONFIG_COUNT":          true,
		"GIT_CONFIG_PARAMETERS":     true,
		"GIT_CONFIG_KEY_0":          true,
		"GIT_CONFIG_VALUE_0":        true,
		"GIT_CONFIG_KEY_2147483647": true,
		// a forwarded but NON-count route does not qualify (not in the strip list).
		"GIT_CONFIG_GLOBAL": false,
		"GIT_CONFIG_SYSTEM": false,
		// count-family names NOT in the forwarded set never qualify — the
		// claim is what makes re-insertion safe.
		"GIT_CONFIG_KEY_00": false, // literal spelling differs from the forwarded KEY_0
		"GIT_CONFIG_KEY_1":  false,
	} {
		got := afGitForwardedConfigCountName(name, forwarded)
		require.Equal(t, want, got, "afGitForwardedConfigCountName(%q, forwarded)", name)
	}
	// An ambient count name never qualifies without a matching claim, even
	// though it is count-family — the regression guard for the host daemon.
	require.False(t, afGitForwardedConfigCountName("GIT_CONFIG_COUNT", map[string]bool{}))
	require.False(t, afGitForwardedConfigCountName("GIT_CONFIG_KEY_0", nil))
	require.False(t, afGitForwardedConfigCountName("HOME", forwarded))
}

// TestReinsertForwardedGitConfigCountCompleteForwardSurvives pins the core
// fix: a complete operator-forwarded GIT_CONFIG_COUNT + pair that survives
// afGitScopedEnvironment is stripped by repositoryPathEnvironment (the bug)
// and restored by reinsertForwardedGitConfigCount (the fix). This is the
// three-layer composition af's own git actually runs.
func TestReinsertForwardedGitConfigCountCompleteForwardSurvives(t *testing.T) {
	t.Setenv(afGitConfigEnvName, "/af-home/.af-gitconfig")
	t.Setenv(afGitForwardedEnvName, "GIT_CONFIG_COUNT,GIT_CONFIG_KEY_0,GIT_CONFIG_VALUE_0")
	in := []string{
		"HOME=/h", "PATH=/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.sslCAInfo",
		"GIT_CONFIG_VALUE_0=/operator/ca.pem",
	}
	scoped := afGitScopedEnvironment(in)
	for _, want := range []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.sslCAInfo",
		"GIT_CONFIG_VALUE_0=/operator/ca.pem",
	} {
		require.True(t, slices.Contains(scoped, want),
			"afGitScopedEnvironment must preserve the complete forwarded count (#7903): %v", scoped)
	}

	stripped := repositoryPathEnvironment(scoped)
	// The bug surface: repositoryPathEnvironment strips the count entries
	// afGitScopedEnvironment preserved — unconditionally, no claim exemption.
	for _, entry := range []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.sslCAInfo",
		"GIT_CONFIG_VALUE_0=/operator/ca.pem",
	} {
		require.False(t, slices.Contains(stripped, entry),
			"repositoryPathEnvironment strips the forwarded count (#4202 predates the claim system): %v", stripped)
	}

	final := reinsertForwardedGitConfigCount(scoped, stripped)
	for _, want := range []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.sslCAInfo",
		"GIT_CONFIG_VALUE_0=/operator/ca.pem",
	} {
		require.True(t, slices.Contains(final, want),
			"the fix re-inserts the complete forwarded count after the strip: %v", final)
	}
	// Each entry appears exactly once (no duplication): repositoryPathEnvironment
	// strips count-family entries by name, so the append never duplicates.
	for _, entry := range []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.sslCAInfo",
		"GIT_CONFIG_VALUE_0=/operator/ca.pem",
	} {
		count := 0
		for _, e := range final {
			if e == entry {
				count++
			}
		}
		require.Equal(t, 1, count, "a re-inserted count entry must not duplicate: %q in %v", entry, final)
	}
}

// TestReinsertForwardedGitConfigCountAmbientStaysStripped is the regression
// guard that the fix does not over-reach into #4202's ambient protection: with
// no AF_GIT_FORWARDED claim (the host daemon, plain sessions, or any path
// where the operator did not forward the count), a GIT_CONFIG_COUNT in the
// environment is an ambient injection that repositoryPathEnvironment strips and
// reinsertForwardedGitConfigCount must NOT restore.
func TestReinsertForwardedGitConfigCountAmbientStaysStripped(t *testing.T) {
	t.Setenv(afGitForwardedEnvName, "") // no operator claim — the host-daemon shape
	in := []string{
		"HOME=/h", "PATH=/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.worktree",
		"GIT_CONFIG_VALUE_0=/foreign",
	}
	stripped := repositoryPathEnvironment(in)
	final := reinsertForwardedGitConfigCount(in, stripped)
	for _, unwanted := range []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.worktree",
		"GIT_CONFIG_VALUE_0=/foreign",
	} {
		require.False(t, slices.Contains(final, unwanted),
			"an ambient count (no forwarded claim) must stay stripped — #4202's protection: %v", final)
	}
	require.True(t, slices.Contains(final, "HOME=/h"))
	require.True(t, slices.Contains(final, "PATH=/bin"))
}

// TestReinsertForwardedGitConfigCountIncompleteNeutralized pins that an
// incomplete forwarded count stays neutralized through the strip+reinsert: a
// live count over a missing pair is a fatal git error ("missing config
// value"), and afGitScopedEnvironment pins it empty, so reinsert must
// re-insert the empty pin (git reads 0) — never revive it live.
func TestReinsertForwardedGitConfigCountIncompleteNeutralized(t *testing.T) {
	t.Setenv(afGitConfigEnvName, "/af-home/.af-gitconfig")
	t.Setenv(afGitForwardedEnvName, "GIT_CONFIG_COUNT,GIT_CONFIG_KEY_0") // VALUE_0 not forwarded
	in := []string{
		"HOME=/h", "PATH=/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.sslCAInfo",
		// GIT_CONFIG_VALUE_0 absent — the count is incomplete.
	}
	scoped := afGitScopedEnvironment(in)
	require.True(t, slices.Contains(scoped, "GIT_CONFIG_COUNT="),
		"afGitScopedEnvironment must neutralize the incomplete count (empty pin): %v", scoped)
	require.False(t, slices.Contains(scoped, "GIT_CONFIG_COUNT=1"),
		"the incomplete count must not stay live after afGitScopedEnvironment: %v", scoped)

	stripped := repositoryPathEnvironment(scoped)
	final := reinsertForwardedGitConfigCount(scoped, stripped)
	require.True(t, slices.Contains(final, "GIT_CONFIG_COUNT="),
		"the neutralized empty count must be re-inserted, not revived live: %v", final)
	require.False(t, slices.Contains(final, "GIT_CONFIG_COUNT=1"),
		"a neutralized count must not be revived as a live count by reinsert: %v", final)
}

// TestReinsertForwardedGitConfigCountPreStrippedEnvIsNoOp pins the
// repo-gone-authorization path: a caller that pre-strips with
// repositoryPathEnvironment (worktree_repo_gone_authorization.go) passes an
// environment with no count entries, so reinsert finds nothing to re-inject
// even when the forwarded set is non-empty (the agent-server inside the
// container). This is the guarantee that the shared runner is safe for both
// its callers.
func TestReinsertForwardedGitConfigCountPreStrippedEnvIsNoOp(t *testing.T) {
	t.Setenv(afGitConfigEnvName, "/af-home/.af-gitconfig")
	t.Setenv(afGitForwardedEnvName, "GIT_CONFIG_COUNT,GIT_CONFIG_KEY_0,GIT_CONFIG_VALUE_0")
	in := []string{"HOME=/h", "PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v"}
	preStripped := repositoryPathEnvironment(in) // a caller that pre-strips
	for _, unwanted := range []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v"} {
		require.False(t, slices.Contains(preStripped, unwanted),
			"the pre-stripped env has no count entries: %v", preStripped)
	}
	final := reinsertForwardedGitConfigCount(preStripped, repositoryPathEnvironment(preStripped))
	require.Equal(t, preStripped, final,
		"reinsert is a no-op when environment carries no count entries (repo-gone path): %v", final)
}

// TestRunGitCommandContextWithEnvironmentDeliversForwardedGitConfigCount is the
// end-to-end guarantee through a real git child: a complete operator-forwarded
// GIT_CONFIG_COUNT pair, composed through afGitScopedEnvironment and the
// shared runner, is observed by git (git config --get returns the forwarded
// value). Without the fix the count is stripped at the last hop and git sees
// nothing (the bug); with the fix the count is re-inserted and delivers.
func TestRunGitCommandContextWithEnvironmentDeliversForwardedGitConfigCount(t *testing.T) {
	configFile := emptyAfGitConfigFile(t)
	scoped := afGitScopedEnvForTest(t, configFile,
		"GIT_CONFIG_COUNT,GIT_CONFIG_KEY_0,GIT_CONFIG_VALUE_0",
		[]string{
			"HOME=" + os.Getenv("HOME"),
			"PATH=" + os.Getenv("PATH"),
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=repro.probe.key",
			"GIT_CONFIG_VALUE_0=fromcount",
		})

	tmpDir := t.TempDir()
	g := &GitWorktree{}
	out, err := g.runGitCommandContextWithEnvironment(context.Background(), tmpDir, scoped,
		"config", "--get", "repro.probe.key")
	require.NoError(t, err, "git config --get must succeed when the forwarded count delivers")
	require.Equal(t, "fromcount", strings.TrimSpace(out),
		"the operator's forwarded config value must reach af's own git")
}

// TestRunGitCommandContextWithEnvironmentAmbientCountStaysStripped is the
// end-to-end regression guard: with no AF_GIT_FORWARDED claim, an ambient
// GIT_CONFIG_COUNT in the environment is stripped by repositoryPathEnvironment
// and NOT re-injected, so git does not see the key (the ordinary not-found
// exit 1, not a fatal config error). This pins that the fix does not over-reach
// into #4202's ambient protection at the subprocess boundary.
func TestRunGitCommandContextWithEnvironmentAmbientCountStaysStripped(t *testing.T) {
	t.Setenv(afGitForwardedEnvName, "") // no operator claim — the host-daemon shape
	tmpDir := t.TempDir()
	env := []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=repro.ambient.key",
		"GIT_CONFIG_VALUE_0=ambientvalue",
	}
	g := &GitWorktree{}
	out, err := g.runGitCommandContextWithEnvironment(context.Background(), tmpDir, env,
		"config", "--get", "repro.ambient.key")
	require.Error(t, err, "an ambient count must not deliver the key — git exits 1 on not-found")
	require.Empty(t, strings.TrimSpace(out), "the ambient count must not reach af's own git")
	// The count is stripped (not re-injected), so git reports the ordinary
	// key-not-found exit 1 — never the fatal "missing config value" a LIVE
	// count over a present key would raise.
	require.NotContains(t, err.Error(), "missing config", "ambient count stripped, not a fatal pair error: %v", err)
	require.NotContains(t, err.Error(), "unable to parse", "ambient count stripped, not a fatal count error: %v", err)
}
