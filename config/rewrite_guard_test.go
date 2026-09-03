package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The guard in rewrite_guard.go is a BACKSTOP: once the scanners track multiline
// string state (#3662), no ordinary input reaches it. What the tests below drive
// is therefore the shape of the bug it exists to catch — a writer whose edit does
// not do what the command says it does — by handing each writer a target key and
// an edit that name different settings. That is exactly what a scanner that
// lands on the wrong line produces, and it is what the parse-before-write gate
// cannot see, because the result is still valid TOML.

// TestSetRefusesARewriteThatChangesAnUnrelatedValue drives scalarWrite.apply
// with a write whose declared key and edited leaf disagree. The rewrite parses
// cleanly; it just means something else.
func TestSetRefusesARewriteThatChangesAnUnrelatedValue(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 1\nbranch_prefix = 'me/'\non_archive_command = 'echo bye'\n")
	t.Setenv("SHELL", "/bin/sh")
	original := readFile(t, path)

	w := scalarWrite{key: "on_archive_command", leaf: "branch_prefix", canonical: "clobbered", encoded: "'clobbered'"}
	_, err := w.apply(pinnedTestTarget(path), prettyHomePath(path))

	require.Error(t, err, "an edit that lands on the wrong key must be refused")
	assert.Contains(t, err.Error(), "branch_prefix", "the refusal names the setting that would have moved")
	assert.Contains(t, err.Error(), "no changes written")
	assert.Equal(t, original, readFile(t, path), "and nothing is written")
}

// TestSetProjectRefusesARewriteThatChangesAnUnrelatedValue is the same proof for
// the personal-project writer, which has its own gate.
func TestSetProjectRefusesARewriteThatChangesAnUnrelatedValue(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "branch_prefix = 'me/'\ndefault_program = 'codex'\n")
	path, err := ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	original := readFile(t, path)

	w := scalarWrite{key: "default_program", leaf: "branch_prefix", canonical: "clobbered", encoded: "'clobbered'"}
	_, err = w.applyProject(path, prettyHomePath(path))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "branch_prefix")
	assert.Contains(t, err.Error(), "no changes written")
	assert.Equal(t, original, readFile(t, path))
}

// TestUnsetProjectRefusesARemovalOfTheWrongLine drives applyProjectUnset with a
// key and a leaf that disagree — the delete-side shape of the same bug.
func TestUnsetProjectRefusesARemovalOfTheWrongLine(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "branch_prefix = 'me/'\ndefault_program = 'codex'\n")
	path, err := ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	original := readFile(t, path)

	_, err = applyProjectUnset(path, prettyHomePath(path), "", "branch_prefix", "default_program", false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "branch_prefix")
	assert.Contains(t, err.Error(), "no changes written")
	assert.Equal(t, original, readFile(t, path))
}

// TestUnsetGlobalRefusesARemovalOfTheWrongLine is the global unset's turn: an
// alias whose storage spellings name one setting while the command reports
// another.
func TestUnsetGlobalRefusesARemovalOfTheWrongLine(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 1\nbranch_prefix = 'me/'\nlisten_addr = '0.0.0.0:8443'\n")
	t.Setenv("SHELL", "/bin/sh")
	original := readFile(t, path)

	misaimed := configKeyAlias{canonical: "network.listen_addr", legacy: "branch_prefix", section: "network", leaf: "listen_addr"}
	_, err := applyGlobalUnset(pinnedTestTarget(path), prettyHomePath(path), "network.listen_addr", misaimed)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "branch_prefix")
	assert.Contains(t, err.Error(), "no changes written")
	assert.Equal(t, original, readFile(t, path))
}

// TestProjectRewriteDriftSeesPresenceNotJustValues covers the dimension the
// global guard has no equivalent for. A personal project config distinguishes
// "set to an empty value" from "absent", so a rewrite that dropped a line whose
// value happened to equal the zero value would change how the key resolves while
// every field still compared equal.
func TestProjectRewriteDriftSeesPresenceNotJustValues(t *testing.T) {
	_, _, project := registeredTestProject(t)
	path, err := ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)

	before, err := parseProjectConfig([]byte("branch_prefix = ''\ndefault_program = 'codex'\n"), path)
	require.NoError(t, err)
	after, err := parseProjectConfig([]byte("default_program = 'codex'\n"), path)
	require.NoError(t, err)

	assert.Empty(t, structValueDrift(reflect.ValueOf(*before), reflect.ValueOf(*after)),
		"the fixture must be one the value comparison alone cannot tell apart, or it proves nothing about presence")
	assert.Contains(t, projectRewriteDrift(before, after, "default_program"), "branch_prefix",
		"a dropped line is drift even when the value it held was the zero value")
	assert.Empty(t, projectRewriteDrift(before, after, "default_program", "branch_prefix"),
		"naming both keys as intentional leaves nothing to report")
}

// TestConfigRewriteDriftExemptsOnlyWhatItWasAsked pins the exemption itself: the
// key the writer targeted is passed over, every other value is not, and the
// alias spellings resolve to the same field.
func TestConfigRewriteDriftExemptsOnlyWhatItWasAsked(t *testing.T) {
	before := DefaultConfig()
	before.BranchPrefix = "me/"
	before.ListenAddr = "127.0.0.1:8443"

	moved := snapshotConfig(before)
	moved.BranchPrefix = "new/"

	assert.Empty(t, configRewriteDrift(before, moved, "branch_prefix"))
	assert.Contains(t, configRewriteDrift(before, moved, "on_archive_command"), "branch_prefix",
		"exempting a different key must not blind the guard to this one")

	both := snapshotConfig(moved)
	both.ListenAddr = "0.0.0.0:8443"
	assert.Contains(t, configRewriteDrift(before, both, "branch_prefix"), "listen_addr")
	assert.Empty(t, configRewriteDrift(before, both, "branch_prefix", "network.listen_addr"),
		"the dotted canonical spelling resolves to the same flat field")
	assert.Empty(t, configRewriteDrift(before, both, "branch_prefix", "listen_addr"),
		"and so does the legacy flat spelling")
}

// TestConfigRewriteDriftExemptsOneMapEntryNotTheWholeTable is the dynamic-key
// half: `af config set program_overrides.claude` may move that one entry, and a
// rewrite that also disturbed a sibling override must still be refused.
func TestConfigRewriteDriftExemptsOneMapEntryNotTheWholeTable(t *testing.T) {
	before := DefaultConfig()
	before.ProgramOverrides = map[string]string{"claude": "/bin/claude", "codex": "/bin/codex"}

	one := snapshotConfig(before)
	one.ProgramOverrides["claude"] = "/opt/claude"
	assert.Empty(t, configRewriteDrift(before, one, "program_overrides.claude"))

	both := snapshotConfig(one)
	both.ProgramOverrides["codex"] = "/opt/codex"
	assert.Contains(t, configRewriteDrift(before, both, "program_overrides.claude"), "program_overrides",
		"a sibling entry is not covered by the entry that was asked for")

	removed := snapshotConfig(before)
	delete(removed.ProgramOverrides, "claude")
	assert.Empty(t, configRewriteDrift(before, removed, "program_overrides.claude"),
		"clearing the named entry is what an unset does")
	assert.Contains(t, configRewriteDrift(before, removed, "program_overrides.codex"), "program_overrides")
}

// TestConfigRewriteDriftReportsAKeyItCannotResolve pins the fail-CLOSED
// direction. Silently accepting an unresolvable key would exempt nothing while
// looking like it exempted something — the guard would pass a rewrite it never
// actually checked.
func TestConfigRewriteDriftReportsAKeyItCannotResolve(t *testing.T) {
	before := DefaultConfig()
	after := snapshotConfig(before)

	drift := configRewriteDrift(before, after, "not_a_config_key")
	assert.Contains(t, drift, "not_a_config_key")
	assert.Contains(t, drift, "names no config field")
	assert.Empty(t, configRewriteDrift(before, after), "an identical pair with nothing exempted is clean")
	assert.Equal(t, "the loaded configuration", configRewriteDrift(nil, after, "branch_prefix"))
}

// TestEverySettableKeyIsExemptable is the pin that keeps the fail-closed branch
// above from becoming a routine refusal: every key a writer can be handed must
// resolve, or `af config set` on that key would refuse itself.
func TestEverySettableKeyIsExemptable(t *testing.T) {
	before := DefaultConfig()
	after := snapshotConfig(before)
	for _, key := range SettableKeys() {
		probe := strings.ReplaceAll(key, "<name>", "claude")
		assert.Empty(t, configRewriteDrift(before, after, probe), "settable key %q must be exemptable", probe)
	}
	assert.Empty(t, configRewriteDrift(before, after, SchemaVersionField),
		"the machine-managed schema marker every global write touches must be exemptable too")
}
