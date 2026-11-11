package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// installKillingGitShim puts a git shim first on PATH that kills itself with
// signal 9 before writing anything. Every subsequent git invocation produces an
// unanswered probe (config.ErrRepoProbeUnanswered) — the "no answer came back"
// shape #3500/#7077 are about, and the one a loaded machine produces when a
// re-read's git probe goes unanswered. The real git must already have been used
// for repo setup before this is installed.
//
// Mirrors daemon/rootagent_repoprobe_test.go's installUnanswerableGit pattern:
// a signalled death has a negative exit code, so probeWentUnanswered classifies
// it as unanswered, and RepoFromPath wraps the error with ErrRepoProbeUnanswered.
func installKillingGitShim(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"),
		[]byte("#!/bin/sh\nkill -9 $$\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// installCountingGitShim puts a git shim first on PATH that delegates to the
// real git for the first `allow` invocations and kills every invocation
// thereafter. This makes a stateful probe reproducible: the daemon's own resolve
// (which runs first inside ListBackends) passes through to real git, while the
// independent re-read inside BackendKindFor (which runs later inside
// defaultFor) hits only killed probes — exactly the transient window the bug
// is about.
func installCountingGitShim(t *testing.T, allow int) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err, "git must be on PATH for the test to run")
	binDir := t.TempDir()
	counter := filepath.Join(binDir, "counter")
	require.NoError(t, os.WriteFile(counter, []byte("0"), 0o644))
	shim := "#!/bin/sh\n" +
		"n=$(cat " + counter + ")\n" +
		"n=$((n+1))\n" +
		"echo $n > " + counter + "\n" +
		"if [ \"$n\" -le " + strconv.Itoa(allow) + " ]; then exec " + realGit + " \"$@\"; fi\n" +
		"kill -9 $$\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestDefaultFor_UnansweredReprobeIsUnknownNotUnavailable is the core regression
// for the bug introduced by d6ae8e5d (#7078): when the project-config re-read
// inside BackendKindFor fails transiently (every git probe unanswered) while the
// daemon's own resolve succeeded, defaultFor must classify the outcome as
// BackendUnknown ("could not be evaluated", retry-oriented) — not
// BackendUnavailable ("fix that key") with a fabricated "sets backend = %q,
// which is not a known backend" claim about a config that is perfectly valid.
//
// The direct defaultFor call isolates the classification: cfg and cfgErr are
// pre-computed with real git (so cfgErr == nil and cfg.Backend == "local"), and
// only BackendKindFor's independent re-read sees the unanswerable git.
func TestDefaultFor_UnansweredReprobeIsUnknownNotUnavailable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)
	writeRepoBackendConfig(t, repo, map[string]any{"backend": "local"})

	// The daemon's own resolve — the one ListBackends performs before
	// defaultFor. It succeeds, so cfgErr == nil and cfg.Backend == "local".
	cfg, cfgErr := config.ResolveConfig(repo)
	require.NoError(t, cfgErr)
	require.NotNil(t, cfg)
	require.Equal(t, "local", cfg.Backend, "precondition: the config is valid and sets backend=local")

	// The backends catalog ListBackends builds before calling defaultFor.
	backends := backendCatalog(config.SupportedBackends, cfg, cfgErr, repo, "")

	// Now make every git invocation die by signal 9. BackendKindFor's
	// independent re-read — the one inside defaultFor — sees only killed
	// probes, so resolveProjectConfig exhausts its retry budget and returns a
	// *ProjectConfigUnknownError. The daemon's own resolve already completed,
	// so this shim affects only the re-read.
	installKillingGitShim(t)

	name, status, reason := defaultFor(backends, cfg, cfgErr, repo)

	assert.Equal(t, "", name, "there is no default to name when the re-read could not complete")
	assert.Equal(t, BackendUnknown, status,
		"a config that could not be READ is unknown (retry), not unavailable (fix the key)")
	// The reason must NOT fabricate a claim about the backend VALUE — the config
	// is valid, it set backend = "local", and the only thing that failed was
	// reading it back under load.
	assert.NotContains(t, reason, "not a known backend",
		"the reason must not fabricate an unknown-backend claim about a valid config")
	assert.NotContains(t, reason, "fix that key",
		"the reason must not tell the user to fix a config that is fine")
	// The reason MUST be retry-oriented, naming the read failure — the same
	// distinction #7077/#7078 introduced everywhere else in the launch path.
	assert.Contains(t, reason, "retry",
		"the reason says what to do: retry, not fix the config")
}

// TestListBackends_UnansweredReprobeIsUnknownNotUnavailable is the end-to-end
// form of the regression: the full ListBackends RPC, with a stateful git shim
// that lets the daemon's own resolve pass through to real git and kills only
// BackendKindFor's independent re-read. The default must come back as
// BackendUnknown with a retry-oriented reason, not BackendUnavailable with a
// fabricated "sets backend = %q, which is not a known backend" claim.
//
// The shim delegates the first 2 git invocations to real git (the daemon's own
// RepoFromPath: `git rev-parse --show-toplevel` + `git rev-parse --git-dir
// --git-common-dir`) and kills every invocation thereafter. A repo with
// backend="local" and no docker/ssh/hook config makes no further git calls in
// the daemon's resolve or backendCatalog, so the killed calls are all inside
// BackendKindFor's re-read.
func TestListBackends_UnansweredReprobeIsUnknownNotUnavailable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)
	writeRepoBackendConfig(t, repo, map[string]any{"backend": "local"})

	// Let the daemon's own resolve (2 git calls for a simple repo) pass through
	// to real git, and kill every git invocation thereafter. The killed calls
	// are all inside BackendKindFor's independent re-read.
	installCountingGitShim(t, 2)

	resp := listBackends(t, repo)

	// The default must be empty with BackendUnknown — "could not be evaluated",
	// not BackendUnavailable — "fix that key" — and the reason must name the
	// read failure, not fabricate a claim about the backend value.
	assert.Equal(t, "", resp.Default,
		"there is no default to name when the re-read could not complete")
	assert.Equal(t, BackendUnknown, resp.DefaultStatus,
		"a config that could not be READ is unknown (retry), not unavailable (fix the key)")
	assert.NotContains(t, resp.DefaultReason, "not a known backend",
		"the reason must not fabricate an unknown-backend claim about a valid config")
	assert.NotContains(t, resp.DefaultReason, "fix that key",
		"the reason must not tell the user to fix a config that is fine")
	assert.Contains(t, resp.DefaultReason, "retry",
		"the reason says what to do: retry, not fix the config")
	// The per-backend rows answer from the daemon's OWN resolve (cfgErr == nil),
	// which succeeded — so they are evaluated normally: local is available, the
	// unconfigured backends are unavailable. The bug is specific to the DEFAULT,
	// whose BackendKindFor re-read went unanswered; the per-backend rows are
	// NOT unknown, because the config they were checked against did read.
	assert.Equal(t, BackendAvailable, optionByName(t, resp, config.BackendLocal).Status,
		"local reads no repo config, so it stays available")
	for _, name := range []string{config.BackendDocker, config.BackendSSH, config.BackendHook, config.BackendSandbox} {
		assert.Equal(t, BackendUnavailable, optionByName(t, resp, name).Status,
			"backend %s is unconfigured in this repo and is unavailable, not unknown — the daemon's own resolve read the config", name)
	}
}

// TestDefaultFor_InvalidBackendValueStillUnavailable is the no-regression guard:
// a config that LOADED and named a genuinely unrecognized backend value must
// still be BackendUnavailable with the "fix that key" reason. The fix only
// re-routes the unknown-read class (transient re-read failures) to
// BackendUnknown; the unrecognized-value class stays exactly where it was.
func TestDefaultFor_InvalidBackendValueStillUnavailable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)
	writeRepoBackendConfig(t, repo, map[string]any{"backend": "bogus"})

	resp := listBackends(t, repo)

	assert.Equal(t, "", resp.Default)
	assert.Equal(t, BackendUnavailable, resp.DefaultStatus,
		"a config that names a genuinely unknown backend is unavailable, not unknown")
	assert.Contains(t, resp.DefaultReason, `"bogus"`,
		"the reason names the offending value")
	assert.Contains(t, resp.DefaultReason, "not a known backend",
		"the reason says the value is unrecognized")
	assert.Contains(t, resp.DefaultReason, "fix that key",
		"the reason says what to do: fix the config")
}
