package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	aflog "github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the real `af config get root_agent` CLI command path
// (configGetCmd.RunE) end to end against a registered project carrying a
// typo'd personal [root_agent] leaf, with the same interactive-warning wiring
// the root command's PersistentPreRun installs (configWarningsToStderr +
// SetInteractiveWarningWriter). They confirm the personal unknown-leaf
// warning surfaces on the real CLI stderr (or stays log-only under --json),
// and that the command still exits 0 (warn-only).
//
// The "warning is logged once per process" behavior is pinned by the
// config-package unit tests (TestProjectUnknownLeafInteractiveWarning,
// TestProjectUnknownLeafWarningsMemoIsPerSource); configGetCmd.RunE calls
// log.Initialize(false) at its top, which re-points log.WarningLog at a file
// sink, so a pre-RunE logger swap would be clobbered — these CLI tests
// therefore assert the USER-FACING stderr surface (the interactive writer the
// root command wires), which is the surface the bug's silence was about.

// suppressWarningLog repoints log.WarningLog at a discard buffer for the test
// so the per-test log file does not leak the warning onto the test process's
// stderr. It cannot capture the warning for assertion (log.Initialize inside
// RunE re-points WarningLog at a file), but it keeps test output clean.
func suppressWarningLog(t *testing.T) {
	t.Helper()
	var buf bytes.Buffer
	aflog.CaptureOutput(t, aflog.WarningLog, "", &buf)
}

// wireCLIWarnings replicates rootCmd.PersistentPreRun: when the command is an
// interactive (non-daemon, non-json) invocation, config warnings reach stderr
// via SetInteractiveWarningWriter; under --json they stay log-only. Returns
// the stderr buffer it routes warnings to (nil when log-only) and restores
// the writer on cleanup.
func wireCLIWarnings(t *testing.T, jsonMode bool) *bytes.Buffer {
	t.Helper()
	// configJSONFlag is bound to configGetCmd's --json flag, so
	// configWarningsToStderr(configGetCmd) reads the bound value the same way
	// the real PersistentPreRun reads it on the live command.
	if configWarningsToStderr(configGetCmd) {
		var stderr bytes.Buffer
		config.SetInteractiveWarningWriter(&stderr)
		t.Cleanup(func() { config.SetInteractiveWarningWriter(nil) })
		return &stderr
	}
	config.SetInteractiveWarningWriter(nil)
	t.Cleanup(func() { config.SetInteractiveWarningWriter(nil) })
	return nil
}

// TestCLIConfigGetRootAgentTypoWarnsOnStderr is the primary end-to-end: a
// registered project's personal config carries `enabld = true`, and the real
// `af config get root_agent --repo <path>` warns on stderr (the surface a person
// running af sees), still exits 0, and renders the (inert) value.
func TestCLIConfigGetRootAgentTypoWarnsOnStderr(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", false,
		"[root_agent]\nenabld = true\n")

	setConfigGetReadFlags(t, repoRoot, false, false)
	stderr := wireCLIWarnings(t, false)
	suppressWarningLog(t)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err, "an unknown leaf must warn, not fail the load")
	assert.Equal(t, "{\"enabled\":false}\n", out, "the typo'd enabled is dropped, so the root resolves disabled")

	require.NotNil(t, stderr)
	assert.Contains(t, stderr.String(), `warning: personal project config`,
		"the interactive CLI must surface the warning on stderr")
	assert.Contains(t, stderr.String(), `unknown key "enabld" under "root_agent" is ignored`)
	assert.Contains(t, stderr.String(), `did you mean "enabled"?`)
	assert.Contains(t, stderr.String(), "the config still loads")
	assert.Contains(t, stderr.String(), "allowed root_agent keys: enabled, program")
	assert.NotContains(t, stderr.String(), "in-repo config",
		"the personal warning must not reuse the in-repo wording")
}

// TestCLIConfigGetRootAgentTypoJSONIsLogOnly pins the --json contract: stderr
// carries only the {data,error} envelope, so the unknown-leaf warning stays
// log-only and does NOT leak onto stderr.
func TestCLIConfigGetRootAgentTypoJSONIsLogOnly(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", false,
		"[root_agent]\nenabld = true\n")

	setConfigGetReadFlags(t, repoRoot, false, true)
	stderr := wireCLIWarnings(t, true)
	suppressWarningLog(t)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err, "the load still succeeds under --json")
	// stderr is reserved for the JSON envelope; the warning must not leak there.
	if stderr != nil {
		assert.NotContains(t, stderr.String(), `unknown key "enabld"`,
			"--json must keep the unknown-leaf warning off stderr")
	}
	// stdout carries the JSON envelope with the resolved value.
	var envelope struct {
		Data struct {
			Key   string `json:"key"`
			Value struct {
				Enabled bool `json:"enabled"`
			} `json:"value"`
		} `json:"data"`
		Error any `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &envelope))
	assert.Equal(t, "root_agent", envelope.Data.Key)
	assert.False(t, envelope.Data.Value.Enabled, "the typo'd enabled is dropped; the root resolves disabled")
	assert.Nil(t, envelope.Error, "a warn-only leaf must not surface as a JSON error")
}

// TestCLIConfigGetRootAgentDisableScenarioIsWarned is the headline impact
// scenario from the bug report: a user hand-edits the personal config to
// SILENCE a root the legacy root_agents entry enabled, but typos `enabled` as
// `enabld`. The typo'd disable is dropped, so the root resolves ENABLED (the
// "root won't stay down" symptom); --explain makes the legacy layer legible,
// and the warning is the new feedback the user was missing.
func TestCLIConfigGetRootAgentDisableScenarioIsWarned(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true,
		"[root_agent]\nenabld = false\n")

	setConfigGetReadFlags(t, repoRoot, true, false)
	stderr := wireCLIWarnings(t, false)
	suppressWarningLog(t)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	// The effective value is enabled: the typo'd enabld=false is dropped, so
	// the legacy entry's enabled=true wins.
	assert.Contains(t, out, `root_agent = {"enabled":true}`, "the typo'd disable is dropped; the legacy root stays enabled")
	assert.Contains(t, out, "legacy root_agents", "the trace must show the legacy layer")
	assert.Contains(t, out, "personal project", "the trace must show the personal layer that was meant to disable")
	// The disable attempt is the feedback the user was missing.
	require.NotNil(t, stderr)
	assert.Contains(t, stderr.String(), `unknown key "enabld"`)
	assert.Contains(t, stderr.String(), `did you mean "enabled"?`)
}

// TestCLIConfigGetRootAgentCleanFileIsSilent confirms the fix does not warn on
// a clean personal [root_agent]: a correctly-spelled `enabled = false` loads
// with no warning, the root resolves disabled, and --explain attributes the
// enabled decision to the personal layer.
func TestCLIConfigGetRootAgentCleanFileIsSilent(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true,
		"[root_agent]\nenabled = false\n")

	setConfigGetReadFlags(t, repoRoot, true, false)
	stderr := wireCLIWarnings(t, false)
	suppressWarningLog(t)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	assert.Contains(t, out, `root_agent = {"enabled":false}`, "the correctly-spelled disable wins")
	assert.Contains(t, out, "enabled: personal project", "the personal layer wins the enabled decision")
	require.NotNil(t, stderr)
	assert.Empty(t, stderr.String(), "a clean file must not warn on stderr")
}

// TestCLIConfigGetRootAgentTypoWarnsOnceAcrossRepeatedLoads pins the
// once-per-source memo against the daemon's repeated config reads: a second
// config-get against the same typo'd file does not re-emit the stderr warning,
// while the value still resolves the same way.
func TestCLIConfigGetRootAgentTypoWarnsOnceAcrossRepeatedLoads(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", false,
		"[root_agent]\nenabld = true\n")

	setConfigGetReadFlags(t, repoRoot, false, false)
	stderr := wireCLIWarnings(t, false)
	suppressWarningLog(t)

	// First load: warns.
	_, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	require.NotNil(t, stderr)
	require.Equal(t, 1, strings.Count(stderr.String(), `unknown key "enabld"`))

	// Second load of the same file: the memo holds — no second warning.
	_, err = runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(stderr.String(), `unknown key "enabld"`),
		"the same (file,table,leaf) warns exactly once per process across repeated CLI loads")
}

// TestCLIConfigGetRootAgentProgramTypoWarns pins the other typo class from the
// bug report: a typo'd program leaf (`prograam`) is silently dropped today,
// leaving the root running the default/wrong profile. After the fix the
// warning names the typo and suggests `program`.
func TestCLIConfigGetRootAgentProgramTypoWarns(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n[root_agent]\nenabled = true\n", false,
		"[root_agent]\nprograam = 'claude'\n")

	setConfigGetReadFlags(t, repoRoot, false, false)
	stderr := wireCLIWarnings(t, false)
	suppressWarningLog(t)

	_, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	require.NotNil(t, stderr)
	assert.Contains(t, stderr.String(), `unknown key "prograam" under "root_agent" is ignored`)
	assert.Contains(t, stderr.String(), `did you mean "program"?`)
}

// TestCLIPersonalConfigPathIsNamedInWarning confirms the warning names the
// actual personal config file path so the user can find and fix the typo
// without consulting docs.
func TestCLIPersonalConfigPathIsNamedInWarning(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", false,
		"[root_agent]\nenabld = true\n")
	// Resolve the personal config path the same way the loader does.
	project, err := config.ResolveProjectSelector(repoRoot)
	require.NoError(t, err)
	personalPath, err := config.ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)

	setConfigGetReadFlags(t, repoRoot, false, false)
	stderr := wireCLIWarnings(t, false)
	suppressWarningLog(t)

	_, err = runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	require.NotNil(t, stderr)
	assert.Contains(t, stderr.String(), filepath.Base(personalPath),
		"the warning must name the personal config file so the user can find it")
	// Sanity: the personal file does carry the typo.
	data, readErr := os.ReadFile(personalPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "enabld")
}
