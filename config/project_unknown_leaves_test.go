package config

import (
	"bytes"
	"strings"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectUnknownLeafWarnsOnTypos is the regression test for the silent-drop
// bug on the personal per-project [root_agent] surface: a hand-edited typo'd
// leaf (enabld, prograam) was silently discarded by Go's non-strict
// unmarshalling, so the layer parsed with err = nil and contributed nothing
// (EnabledSet=false, zero-value profile) with no diagnostic — unlike the
// sibling in-repo [docker]/[ssh] surface (#4599) and the global strict probe.
// After the fix every typo row loads with NO error (warn now, reject later),
// warns naming the typo'd leaf, its table, and the likely intended key, and
// the layer is still inert — the typed decode ignores the typo'd value exactly
// as before, only the silence is removed.
func TestProjectUnknownLeafWarnsOnTypos(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantKey     string
		wantSuggest string
	}{
		{
			name:        "enabld typo (enabled misspelled)",
			body:        "[root_agent]\nenabld = true\n",
			wantKey:     "enabld",
			wantSuggest: "enabled",
		},
		{
			name:        "prograam typo (program misspelled)",
			body:        "[root_agent]\nprograam = 'claude'\n",
			wantKey:     "prograam",
			wantSuggest: "program",
		},
		{
			name:        "disable spelled wrong (enabld = false)",
			body:        "[root_agent]\nenabld = false\n",
			wantKey:     "enabld",
			wantSuggest: "enabled",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, project := registeredTestProject(t)
			path := writePersonalConfig(t, project.ID, tc.body)
			resetUnknownTableLeafWarnings()
			warnings := captureLog(t, &aflog.WarningLog)

			cfg, err := LoadProjectConfig(project.ID)
			require.NoError(t, err, "an unknown leaf must warn, not fail the load")
			require.NotNil(t, cfg)
			layer := cfg.RootAgentLayer()
			require.NotNil(t, layer, "the [root_agent] table is still present")
			assert.False(t, layer.EnabledSet, "the typo'd enabled leaf does not set EnabledSet")
			assert.Equal(t, "", layer.Value.Program, "the typo'd program leaf does not set Program")
			assert.True(t, cfg.IsSet("root_agent"), "the table itself still counts as present")

			logged := warnings.String()
			assert.Contains(t, logged, tc.wantKey, "warning must name the typo'd leaf")
			assert.Contains(t, logged, "root_agent", "warning must name the table")
			assert.Contains(t, logged, "unknown key", "warning must call out the unknown key")
			assert.Contains(t, logged, "ignored", "warning must say the leaf is ignored")
			assert.Contains(t, logged, "config still loads", "warning must say the config still loads")
			assert.Contains(t, logged, `did you mean "`+tc.wantSuggest+`"?`,
				"warning must suggest the likely intended key")
			assert.Contains(t, logged, "personal project config",
				"warning must label the personal layer, not the in-repo one")
			assert.Contains(t, logged, path, "warning must name the personal config file")
			assert.NotContains(t, logged, "in-repo config",
				"the personal warning must not reuse the in-repo wording")
		})
	}
}

// TestProjectUnknownLeafInteractiveWarning pins the CLI stderr surface: with
// an interactive writer installed, the unknown-leaf warning is printed there
// as well as logged, once per process across repeated loads, while the load
// succeeds. A nil writer (daemon, TUI) keeps it log-only. The full message
// format — file path, typo'd leaf, table, did-you-mean, sorted allowed set —
// is pinned exactly so the CLI/doctor surfaces stay stable.
func TestProjectUnknownLeafInteractiveWarning(t *testing.T) {
	_, _, project := registeredTestProject(t)
	path := writePersonalConfig(t, project.ID, "[root_agent]\nenabld = true\n")
	warnings := captureLog(t, &aflog.WarningLog)
	var stderr bytes.Buffer
	SetInteractiveWarningWriter(&stderr)
	t.Cleanup(func() { SetInteractiveWarningWriter(nil) })

	for range 2 {
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
	}

	want := "warning: personal project config " + path + `: unknown key "enabld" under "root_agent" is ignored; did you mean "enabled"? the config still loads (allowed root_agent keys: enabled, program)` + "\n"
	assert.Equal(t, want, stderr.String(), "printed once, not once per load")
	assert.Equal(t, 1, strings.Count(warnings.String(), `unknown key "enabld"`),
		"logged once, not once per load")

	SetInteractiveWarningWriter(nil)
	resetUnknownTableLeafWarnings()
	stderr.Reset()
	_, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	assert.Empty(t, stderr.String(), "a nil writer keeps the warning log-only")
}

// TestProjectUnknownLeavesReported: the loaded config reports each unknown
// leaf with its suggestion on every load — not just the first, which is all
// the memoized warning covers — so af doctor can list it. A clean file reports
// none.
func TestProjectUnknownLeavesReported(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nenabld = true\nprograam = 'claude'\n")
	for range 2 {
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		leaves := cfg.UnknownLeaves()
		require.Len(t, leaves, 2)
		assert.Equal(t, []string{"root_agent.enabld", "root_agent.prograam"},
			[]string{leaves[0].Table + "." + leaves[0].Key, leaves[1].Table + "." + leaves[1].Key})
		assert.Equal(t, "enabled", leaves[0].Suggestion)
		assert.Equal(t, "program", leaves[1].Suggestion)
		assert.Contains(t, leaves[0].Message, `did you mean "enabled"?`)
		assert.Contains(t, leaves[1].Message, `did you mean "program"?`)
	}

	// A clean file reports no leaves.
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = true\nprogram = 'claude'\n")
	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	assert.Empty(t, cfg.UnknownLeaves())
}

// TestProjectUnknownLeafCorrectKeysNoWarn confirms the fix does not warn on
// valid leaves and that their values round-trip through the typed decode
// unchanged. Case-folded spellings load cleanly (matching the typed decoder's
// case-insensitive field matching), an empty [root_agent] table sets nothing
// and warns nothing, and a personal file with no [root_agent] table produces
// no layer and no warning.
func TestProjectUnknownLeafCorrectKeysNoWarn(t *testing.T) {
	t.Run("lowercase", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "[root_agent]\nenabled = true\nprogram = 'codex'\n")
		resetUnknownTableLeafWarnings()
		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		layer := cfg.RootAgentLayer()
		require.NotNil(t, layer)
		assert.True(t, layer.EnabledSet)
		assert.True(t, layer.Value.Enabled)
		assert.Equal(t, "codex", layer.Value.Program)
		assert.Empty(t, warnings.String(), "correct leaves must not warn")
		assert.Empty(t, cfg.UnknownLeaves())
	})

	t.Run("case-insensitive", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "[root_agent]\nEnabled = true\nPROGRAM = 'codex'\n")
		resetUnknownTableLeafWarnings()
		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		layer := cfg.RootAgentLayer()
		require.NotNil(t, layer)
		// The typed decode matches field names case-insensitively, so the
		// values decode; the leaf check lowercases before lookup, so these
		// spellings do NOT earn a warning. EnabledSet is the pre-existing
		// exact-key shape lookup in rootAgentLayerFromShape (sub["enabled"]),
		// so a case-variant leaf does not set it — a behavior this fix does
		// not touch, kept here only to document that the case fold lives in
		// the leaf check, not in EnabledSet.
		assert.Equal(t, true, layer.Value.Enabled, "Enabled is case-insensitively decoded")
		assert.Equal(t, "codex", layer.Value.Program, "PROGRAM is case-insensitively decoded")
		assert.False(t, layer.EnabledSet, "EnabledSet is the exact-key shape lookup, unchanged by this fix")
		assert.Empty(t, warnings.String(), "case-folded correct spellings must not warn")
		assert.Empty(t, cfg.UnknownLeaves())
	})

	t.Run("empty table", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "[root_agent]\n")
		resetUnknownTableLeafWarnings()
		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		layer := cfg.RootAgentLayer()
		require.NotNil(t, layer, "an empty [root_agent] table is still present")
		assert.False(t, layer.EnabledSet, "an empty [root_agent] sets nothing")
		assert.Empty(t, warnings.String(), "an empty table must not warn")
		assert.Empty(t, cfg.UnknownLeaves())
	})

	t.Run("no table", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "default_program = \"codex\"\n")
		resetUnknownTableLeafWarnings()
		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		assert.Nil(t, cfg.RootAgentLayer(), "no [root_agent] table means no layer")
		assert.Empty(t, warnings.String(), "a file without [root_agent] must not warn")
		assert.Empty(t, cfg.UnknownLeaves())
	})
}

// TestProjectUnknownLeavesMixedWithCorrectKeys confirms a typo'd leaf does not
// suppress the correctly-spelled sibling: enabled still decodes while the
// typo'd prograam is ignored and warned about, exactly as the in-repo
// [docker]/[ssh] behavior is under [docker] image + runargs.
func TestProjectUnknownLeavesMixedWithCorrectKeys(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = true\nprograam = 'claude'\nprogram = 'codex'\n")
	resetUnknownTableLeafWarnings()
	warnings := captureLog(t, &aflog.WarningLog)

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	layer := cfg.RootAgentLayer()
	require.NotNil(t, layer)
	assert.True(t, layer.EnabledSet, "the correctly-spelled enabled still sets EnabledSet")
	assert.True(t, layer.Value.Enabled)
	assert.Equal(t, "codex", layer.Value.Program, "the correctly-spelled program wins; the typo'd prograam is ignored")

	assert.Contains(t, warnings.String(), `unknown key "prograam"`)
	assert.NotContains(t, warnings.String(), `"program" is ignored`,
		"the correctly-spelled program must not be warned about")

	leaves := cfg.UnknownLeaves()
	require.Len(t, leaves, 1)
	assert.Equal(t, "prograam", leaves[0].Key)
	assert.Equal(t, "program", leaves[0].Suggestion)
}

// TestProjectUnknownLeafDisableScenario is the headline impact scenario from
// the bug report: a user hand-edits the personal config to SILENCE a root the
// legacy entry enabled, but typos `enabled` as `enabld`. Before the fix the
// typo'd `enabld = false` was silently dropped, so the personal layer
// contributed nothing and ResolveRootAgent kept the legacy enabled root
// running forever with zero feedback. After the fix the load still succeeds
// (warn now, reject later) but warns loudly, and the resolution still shows
// the legacy root enabled — the warning is the feedback the user was missing.
func TestProjectUnknownLeafDisableScenario(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nenabld = false\n")
	resetUnknownTableLeafWarnings()
	warnings := captureLog(t, &aflog.WarningLog)

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	personal := cfg.RootAgentLayer()
	require.NotNil(t, personal)
	assert.False(t, personal.EnabledSet, "the typo'd enabld does not set enabled")

	resolved := ResolveRootAgent(RootAgentInputs{
		Legacy:   &RootAgentConfig{Program: "claude"}, // a present legacy entry enables
		Personal: personal,
	})
	assert.True(t, resolved.Enabled, "the typo'd disable is dropped, so the legacy root stays enabled")
	assert.Equal(t, RootAgentSourceLegacy, resolved.EnabledSource)

	assert.Contains(t, warnings.String(), `unknown key "enabld"`, "the silent drop now has a diagnostic")
	assert.Contains(t, warnings.String(), `did you mean "enabled"?`)
}

// TestProjectUnknownLeafDoesNotFailTheLoad pins the warn-now-reject-later
// contract: an unknown leaf never turns into a load error, so the rollback
// tolerance posture (a stored future-policy member is kept loadable and
// preserved in the file) and the unknown-member preservation tests are
// unaffected. The typo'd value is ignored, not stored.
func TestProjectUnknownLeafDoesNotFailTheLoad(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = true\nfuture_policy = 'keep'\nenabld = true\n")
	resetUnknownTableLeafWarnings()
	warnings := captureLog(t, &aflog.WarningLog)

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err, "warn-only: the load must succeed so rollback tolerance holds")
	require.NotNil(t, cfg)
	layer := cfg.RootAgentLayer()
	require.NotNil(t, layer)
	assert.True(t, layer.EnabledSet, "the correctly-spelled enabled still applies")
	assert.True(t, layer.Value.Enabled)
	// Both unknown leaves are reported and warned, in sorted order.
	assert.Contains(t, warnings.String(), `unknown key "enabld"`)
	assert.Contains(t, warnings.String(), `unknown key "future_policy"`)
	leaves := cfg.UnknownLeaves()
	require.Len(t, leaves, 2)
	assert.Equal(t, []string{"root_agent.enabld", "root_agent.future_policy"},
		[]string{leaves[0].Table + "." + leaves[0].Key, leaves[1].Table + "." + leaves[1].Key})
}

// TestProjectUnknownLeafDottedInlineTableShape verifies metadataForSource
// decodes a TOML inline-table [root_agent] (root_agent = {...}) as
// map[string]any so the leaf check's type assertion holds for that spelling
// too — the same property TestAuditNestedTypoShapeTOML pins for [docker]. A
// dotted root_agent = { enabld = true } must warn just like the [root_agent]
// header form.
func TestProjectUnknownLeafDottedInlineTableShape(t *testing.T) {
	t.Run("inline table warns", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "root_agent = { enabld = true }\n")
		resetUnknownTableLeafWarnings()
		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.False(t, cfg.RootAgentLayer().EnabledSet)
		assert.Contains(t, warnings.String(), `unknown key "enabld"`,
			"the inline-table form must be walked the same as the [header] form")
	})

	t.Run("shape is map[string]any", func(t *testing.T) {
		body := "root_agent = { enabld = true }\n"
		metadata, err := metadataForSource([]byte(body), "config.toml", FormatTOML)
		require.NoError(t, err)
		sub, ok := metadata.shape["root_agent"].(map[string]any)
		require.True(t, ok, `shape["root_agent"] must be map[string]any for an inline table`)
		assert.Contains(t, sub, "enabld", "the typo'd leaf must be present in the shape")
	})
}

// TestProjectPersonalAllowedTableLeavesMatchStruct is the drift guard for the
// personal [root_agent] leaf allowlist, mirroring
// TestInRepoAllowedTableLeavesMatchStructs: projectPersonalAllowedTableLeaves
// must be exactly the leaf keys derived from RootAgent's struct tags. A
// future field added to RootAgent without acknowledging the new leaf here
// fails this test demanding an explicit update; a hand-maintained literal that
// drops a field (the #1817 class) fails because the literal is missing the
// struct's field.
func TestProjectPersonalAllowedTableLeavesMatchStruct(t *testing.T) {
	want := map[string]map[string]bool{
		"root_agent": {"enabled": true, "program": true},
	}
	assert.Equal(t, want, projectPersonalAllowedTableLeaves,
		"projectPersonalAllowedTableLeaves must match RootAgent struct tags")
}

// TestProjectUnknownLeafWarningsMemoIsPerSource confirms the warning is
// memoized per (file, table, leaf): a second distinct offending leaf in the
// same file still warns, a re-load of the same file/leaf stays silent, and a
// different file with the same leaf warns independently — the same once-per-
// source contract the in-repo loader and the daemon's repeated loads rely on.
func TestProjectUnknownLeafWarningsMemoIsPerSource(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nenabld = true\nprograam = 'claude'\n")
	resetUnknownTableLeafWarnings()
	warnings := captureLog(t, &aflog.WarningLog)

	// First load: both leaves warn.
	_, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(warnings.String(), `unknown key "enabld"`))
	assert.Equal(t, 1, strings.Count(warnings.String(), `unknown key "prograam"`))

	// Second load of the same file: neither warns again.
	_, err = LoadProjectConfig(project.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(warnings.String(), `unknown key "enabld"`),
		"the same (file,table,leaf) warns exactly once per process")
	assert.Equal(t, 1, strings.Count(warnings.String(), `unknown key "prograam"`))

	// A different file with the same leaf warns independently.
	_, _, other := registeredTestProject(t)
	writePersonalConfig(t, other.ID, "[root_agent]\nenabld = true\n")
	_, err = LoadProjectConfig(other.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(warnings.String(), `unknown key "enabld"`),
		"a distinct file with the same leaf warns a second time")
}
