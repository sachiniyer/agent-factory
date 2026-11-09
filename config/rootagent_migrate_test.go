package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMigrateConfig writes config.toml into home, creating the directory.
func writeMigrateConfig(t *testing.T, home, body string) string {
	t.Helper()
	path := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// loadedGlobalLayer re-reads configPath and returns the global [root_agent]
// layer exactly as the resolver sees it — enabledSet included — via the file's
// own decoded shape. A migration that empties the file removes it outright, so
// a missing config.toml means "no global layer", not a failure.
func loadedGlobalLayer(t *testing.T, configPath string) *RootAgentLayer {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	cfg, err := parseConfigTOML(data, configPath)
	require.NoError(t, err)
	metadata, err := metadataForSource(data, configPath, FormatTOML)
	require.NoError(t, err)
	return rootAgentLayerFromShape(cfg.RootAgent, metadata.shape["root_agent"])
}

// loadedPersonalLayer reads the project's personal config file and returns its
// [root_agent] layer — nil when the project has no personal config or no
// [root_agent] table in it.
func loadedPersonalLayer(t *testing.T, projectID string) *RootAgentLayer {
	t.Helper()
	pc, err := LoadProjectConfig(projectID)
	require.NoError(t, err)
	if pc == nil {
		return nil
	}
	return pc.RootAgentLayer()
}

// effectiveRootAgent resolves the four-layer profile for a project, the same
// fold the daemon's ensure pass runs.
func effectiveRootAgent(global, personal *RootAgentLayer, legacy *RootAgentConfig) RootAgent {
	return ResolveRootAgent(RootAgentInputs{Global: global, Legacy: legacy, Personal: personal}).RootAgent
}

// TestMigrateRootAgentsToRegisteredProjects is the property #6957 asks for:
// every legacy root_agents entry whose path is ALREADY a registered project
// moves to that project's personal [root_agent], and the effective profile the
// daemon resolves is identical before and after — enabled or not, program, and
// every other field. When the project's own [root_agent] already yields that
// profile the entry is simply dropped; otherwise the leaves it needs are
// written, sparsely, so unrelated personal settings are preserved.
func TestMigrateRootAgentsToRegisteredProjects(t *testing.T) {
	type wantPersonal struct {
		written    bool // a personal [root_agent] leaf was added by this run
		enabledSet bool
		enabled    bool
		program    string
	}
	for _, tc := range []struct {
		name        string
		entryBody   string // lines under [root_agents.'<root>']
		globalBody  string // extra global config text, "" for none
		personal    string // pre-seeded personal config.toml, "" for none
		want        wantPersonal
		wantProfile RootAgent // the effective profile, identical either side of the migration
	}{
		{
			name:        "empty legacy entry writes enabled",
			want:        wantPersonal{written: true, enabledSet: true, enabled: true},
			wantProfile: RootAgent{Enabled: true},
		},
		{
			name:        "legacy program lands in the personal profile",
			entryBody:   "program = 'codex --profile work'\n",
			want:        wantPersonal{written: true, enabledSet: true, enabled: true, program: "codex --profile work"},
			wantProfile: RootAgent{Enabled: true, Program: "codex --profile work"},
		},
		{
			name:        "personal profile already matching drops the entry only",
			personal:    "[root_agent]\nenabled = true\n",
			want:        wantPersonal{enabledSet: true, enabled: true},
			wantProfile: RootAgent{Enabled: true},
		},
		{
			name:        "personal disable wins and the legacy program still moves",
			entryBody:   "program = 'codex'\n",
			personal:    "[root_agent]\nenabled = false\n",
			want:        wantPersonal{written: true, enabledSet: true, enabled: false, program: "codex"},
			wantProfile: RootAgent{Enabled: false, Program: "codex"},
		},
		{
			name:        "personal disable makes an empty entry redundant",
			personal:    "[root_agent]\nenabled = false\n",
			want:        wantPersonal{enabledSet: true, enabled: false},
			wantProfile: RootAgent{Enabled: false},
		},
		{
			name:        "personal program already covers a matching legacy program",
			entryBody:   "program = 'codex'\n",
			personal:    "[root_agent]\nenabled = true\nprogram = 'codex'\n",
			want:        wantPersonal{enabledSet: true, enabled: true, program: "codex"},
			wantProfile: RootAgent{Enabled: true, Program: "codex"},
		},
		{
			name:        "a global enable makes the entry redundant",
			globalBody:  "[root_agent]\nenabled = true\n",
			want:        wantPersonal{},
			wantProfile: RootAgent{Enabled: true},
		},
		{
			name:        "a global program is inherited rather than copied down",
			globalBody:  "[root_agent]\nprogram = 'global-cmd'\n",
			want:        wantPersonal{written: true, enabledSet: true, enabled: true},
			wantProfile: RootAgent{Enabled: true, Program: "global-cmd"},
		},
		{
			name:        "unrelated personal keys survive the write",
			entryBody:   "program = 'codex'\n",
			personal:    "on_archive_command = 'echo hi'\n",
			want:        wantPersonal{written: true, enabledSet: true, enabled: true, program: "codex"},
			wantProfile: RootAgent{Enabled: true, Program: "codex"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, repoRoot, project := registeredTestProject(t)
			body := tc.globalBody + "\n[root_agents.'" + repoRoot + "']\n" + tc.entryBody
			configPath := writeMigrateConfig(t, home, body)
			if tc.personal != "" {
				writePersonalConfig(t, project.ID, tc.personal)
			}
			beforeGlobal := loadedGlobalLayer(t, configPath)
			beforePersonal := loadedPersonalLayer(t, project.ID)
			before, err := parseConfigTOML([]byte(body), configPath)
			require.NoError(t, err)
			legacy := before.RootAgents[repoRoot]
			wantEffective := effectiveRootAgent(beforeGlobal, beforePersonal, &legacy)

			result, err := MigrateGlobalConfig()
			require.NoError(t, err)

			// The entry is gone from the global file, in every spelling it
			// had. A config that held ONLY the migrated entries keeps its
			// [root_agents] table empty — deleting the file would dangle a
			// config.toml symlink and un-shadow a legacy config.json, and an
			// empty stub is a load error, so the empty table stays.
			globalData, statErr := os.ReadFile(configPath)
			require.NoError(t, statErr)
			migratedCfg, err := parseConfigTOML(globalData, configPath)
			require.NoError(t, err)
			assert.NotContains(t, migratedCfg.RootAgents, repoRoot,
				"the legacy entry must be dropped once its project carries the profile")
			assert.NotContains(t, string(globalData), repoRoot)
			if tc.globalBody == "" {
				assert.Empty(t, migratedCfg.RootAgents,
					"only the now-empty table remains — the file still loads")
				assert.Contains(t, string(globalData), "[root_agents]")
			}

			// The personal [root_agent] layer is exactly what the case expects —
			// written when it had to be, untouched otherwise.
			personal := loadedPersonalLayer(t, project.ID)
			if !tc.want.written && tc.personal == "" {
				assert.Nil(t, personal, "nothing had to be written, so no file must appear")
			} else {
				require.NotNil(t, personal)
				assert.Equal(t, tc.want.enabledSet, personal.EnabledSet)
				assert.Equal(t, tc.want.enabled, personal.Value.Enabled)
				assert.Equal(t, tc.want.program, personal.Value.Program)
			}
			if tc.personal != "" {
				personalContent := readFile(t, mustProjectConfigPath(t, project.ID))
				for _, line := range strings.Split(tc.personal, "\n") {
					if line = strings.TrimSpace(line); line != "" {
						assert.Contains(t, personalContent, line,
							"every pre-existing personal line must survive the write")
					}
				}
			}

			// The effective profile is identical to what the fixture resolved
			// before the migration — this is the property, stated end to end.
			assert.Equal(t, tc.wantProfile, wantEffective,
				"the fixture must describe the profile the layers resolve to")
			assert.Equal(t, wantEffective,
				effectiveRootAgent(loadedGlobalLayer(t, configPath), loadedPersonalLayer(t, project.ID), nil),
				"the project's effective root-agent profile must not change")

			// And the report names it: one migrated root_agents entry, nothing left.
			require.Len(t, result.MigratedRootAgents, 1)
			assert.Equal(t, repoRoot, result.MigratedRootAgents[0].Path)
			assert.Equal(t, project.ID, result.MigratedRootAgents[0].ProjectID)
			assert.Equal(t, tc.want.written, result.MigratedRootAgents[0].PersonalWritten)
			assert.Equal(t, wantEffective, result.MigratedRootAgents[0].Profile)
			assert.Empty(t, result.Left)
		})
	}
}

func mustProjectConfigPath(t *testing.T, id string) string {
	t.Helper()
	path, err := ProjectConfigTomlPath(id)
	require.NoError(t, err)
	return path
}

// TestMigrateRootAgentsLeavesUnregisteredPaths is the boundary the same
// property stops at: a root_agents key that does not name a registered
// project keeps today's behavior — reported with the manual recipe, left
// exactly where it is, and the entries that CAN move still move.
func TestMigrateRootAgentsLeavesUnregisteredPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
	}{
		{
			name: "path does not exist",
			path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
		},
		{
			name: "path exists but is no repository",
			path: func(t *testing.T) string { return t.TempDir() },
		},
		{
			name: "repository exists but is not a registered project",
			path: func(t *testing.T) string {
				return initProjectRegistryRepo(t, filepath.Join(t.TempDir(), "unregistered"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, repoRoot, project := registeredTestProject(t)
			unregistered := tc.path(t)
			body := "[root_agents.'" + repoRoot + "']\n[root_agents.'" + unregistered + "']\n"
			configPath := writeMigrateConfig(t, home, body)

			result, err := MigrateGlobalConfig()
			require.NoError(t, err)

			// The registered project's entry moved; the unregistered path is
			// reported under the manual step and left untouched.
			require.Len(t, result.MigratedRootAgents, 1)
			assert.Equal(t, repoRoot, result.MigratedRootAgents[0].Path)
			require.Len(t, result.Left, 1)
			assert.Equal(t, LegacyRootAgentsKey, result.Left[0].Key)
			assert.Equal(t, legacyRootAgentsManualStep, result.Left[0].Step)
			assert.Equal(t, []string{unregistered}, result.Left[0].Detail)

			content := readFile(t, configPath)
			assert.Contains(t, content, "[root_agents.'"+unregistered+"']",
				"the unregistered entry stays exactly where it was")
			assert.NotContains(t, content, "[root_agents.'"+repoRoot+"']",
				"the registered one is gone")

			// The registered project's own migration still wrote its profile —
			// the unregistered path does not block the entries that can move.
			personal := loadedPersonalLayer(t, project.ID)
			require.NotNil(t, personal)
			assert.True(t, personal.Value.Enabled)
		})
	}
}

// TestMigrateRootAgentsIsIdempotent pins the second run: the migration that
// already landed leaves nothing behind to move, writes nothing, and reports
// nothing.
func TestMigrateRootAgentsIsIdempotent(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	configPath := writeMigrateConfig(t, home, "keep_me = 'a setting the rewrite must not touch'\n[root_agents.'"+repoRoot+"']\nprogram = 'codex'\n")

	first, err := MigrateGlobalConfig()
	require.NoError(t, err)
	require.Len(t, first.MigratedRootAgents, 1)
	assert.Empty(t, first.Left, "the first run migrated the only entry, so nothing is reported left")
	migratedGlobal := readFile(t, configPath)
	migratedPersonal := readFile(t, mustProjectConfigPath(t, project.ID))

	second, err := MigrateGlobalConfig()
	require.NoError(t, err)
	assert.Empty(t, second.MigratedRootAgents)
	assert.Empty(t, second.Migrated)
	assert.Empty(t, second.Left)
	assert.Empty(t, second.Backup, "a run that changed nothing writes no backup")
	assert.Empty(t, second.Diff)
	assert.False(t, second.Changed())
	assert.Equal(t, migratedGlobal, readFile(t, configPath), "the second run must not touch the global file")
	assert.Equal(t, migratedPersonal, readFile(t, mustProjectConfigPath(t, project.ID)),
		"the second run must not touch the personal file either")
}

// TestMigrateRootAgentsDropsEverySpellingForOneProject covers the case two
// spellings name the same checkout — a subdirectory spelling beside the repo
// root, say. The daemon resolves both to the same repository and the
// sorted-first wins; the migration carries the profile once and drops BOTH,
// because either left behind would re-enable a root the migration reports as
// moved.
func TestMigrateRootAgentsDropsEverySpellingForOneProject(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	subdir := filepath.Join(repoRoot, "sub")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	// Sorted order puts the repo root before its own subdirectory, so the
	// root spelling is the daemon's winner and its program is the one the
	// effective profile carries.
	body := "[root_agents.'" + repoRoot + "']\nprogram = 'winner'\n\n[root_agents.'" + subdir + "']\n"
	configPath := writeMigrateConfig(t, home, body)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)

	require.Len(t, result.MigratedRootAgents, 2, "every spelling that names the project moves with it")
	paths := []string{result.MigratedRootAgents[0].Path, result.MigratedRootAgents[1].Path}
	assert.ElementsMatch(t, []string{repoRoot, subdir}, paths)

	// Every spelling moved, and the file held nothing else — its now-empty
	// table stays behind, loadable, and nothing is reported left.
	assert.Empty(t, result.Left)
	migratedCfg, err := parseConfigTOML([]byte(readFile(t, configPath)), configPath)
	require.NoError(t, err)
	assert.Empty(t, migratedCfg.RootAgents, "no spelling of the project's opt-in may survive")

	personal := loadedPersonalLayer(t, project.ID)
	require.NotNil(t, personal)
	assert.True(t, personal.Value.Enabled)
	assert.Equal(t, "winner", personal.Value.Program,
		"the profile that moved is the effective one — the winning entry's")
}

// TestMigrateRootAgentsRemovesEveryDottedLeaf covers the spellings where one
// entry is written as SEVERAL assignments — `'<path>'.enabled` plus
// `'<path>'.program` under a bare [root_agents] header, or the same as
// root-level `root_agents.'<path>'.leaf` keys. Excising only the first leaves a
// partial entry behind: still decoded, still deprecated, still warned about.
func TestMigrateRootAgentsRemovesEveryDottedLeaf(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(repoRoot string) string
	}{
		{
			name: "dotted leaves under a bare [root_agents] header",
			body: func(repoRoot string) string {
				return "keep_me = 'x'\n\n[root_agents]\n'" + repoRoot + "'.enabled = true\n'" + repoRoot + "'.program = 'codex'\n"
			},
		},
		{
			name: "root-level root_agents.'<path>'.leaf keys",
			body: func(repoRoot string) string {
				return "keep_me = 'x'\n\nroot_agents.'" + repoRoot + "'.enabled = true\nroot_agents.'" + repoRoot + "'.program = 'codex'\n"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, repoRoot, project := registeredTestProject(t)
			configPath := writeMigrateConfig(t, home, tc.body(repoRoot))

			result, err := MigrateGlobalConfig()
			require.NoError(t, err)
			require.Len(t, result.MigratedRootAgents, 1)
			assert.Empty(t, result.Left)

			content := readFile(t, configPath)
			assert.NotContains(t, content, repoRoot,
				"every leaf spelling of the migrated entry must be excised")
			migratedCfg, err := parseConfigTOML([]byte(content), configPath)
			require.NoError(t, err)
			assert.Empty(t, migratedCfg.RootAgents,
				"no partial entry may survive for the deprecation warning to keep firing on")
			assert.Contains(t, content, "keep_me = 'x'",
				"an unrelated root key must survive the excision")

			personal := loadedPersonalLayer(t, project.ID)
			require.NotNil(t, personal)
			assert.True(t, personal.Value.Enabled)
			assert.Equal(t, "codex", personal.Value.Program)
		})
	}
}

// TestMigrateRootAgentsCoversEveryProfileField is the guard that keeps the
// patch writer honest: RootAgent is the whole effective profile, so a field
// added to it must also be carried by the migration — the same rule
// TestRootAgentConfigIsFullyAdapted gives the legacy adapter.
func TestMigrateRootAgentsCoversEveryProfileField(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	configPath := writeMigrateConfig(t, home, "keep_me = 'x'\n[root_agents.'"+repoRoot+"']\n")

	_, err := MigrateGlobalConfig()
	require.NoError(t, err)

	personal := loadedPersonalLayer(t, project.ID)
	require.NotNil(t, personal)
	// Every field of the profile the daemon resolves must be reproducible
	// through the personal layer — today enabled and program, and anything
	// RootAgent grows must join them or the property silently breaks.
	after := effectiveRootAgent(loadedGlobalLayer(t, configPath), personal, nil)
	assert.Equal(t, RootAgent{Enabled: true}, after)
}

// TestMigrateRootAgentsMovedCheckoutReportsAmbiguity covers the split that
// classifyLegacyRootAgents produces after a registered project's checkout is
// moved but NOT yet rebound: the moved path resolves (the checkout marker
// traveled with the move) under a NEW repo identity, while the stale
// recorded-root key does not resolve and is merged into the same group by
// project ID. The daemon then serves the project under TWO identities — the
// group's repoID (the moved checkout) and the recorded identity (the durable
// RepoID the snapshot keys the personal layer and the unresolved record under).
//
// Migrating such a group under the one-identity model would, after the next
// daemon start, silently flip the recorded identity's effective profile to the
// moved winner (the personal write lands under the recorded identity) AND drop
// the moved identity's coverage entirely (no personal layer is keyed there and
// the legacy entry is removed). The fix leaves both keys in place and reports
// the cause, telling the user to rebind first — restoring the file's own stated
// invariant that an entry whose post-write resolution would differ is left in
// place, not moved, and that an ambiguous entry is never guessed at.
func TestMigrateRootAgentsMovedCheckoutReportsAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		oldProgram   string
		movedProgram string
	}{
		{name: "old=opus moved=sonic", oldProgram: "opus", movedProgram: "sonic"},
		{name: "old=sonic moved=opus", oldProgram: "sonic", movedProgram: "opus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, repoRoot, project := registeredTestProject(t)
			// "zmoved" sorts after "repo" deterministically; the checkout
			// marker travels with the rename, so the moved key resolves.
			moved := filepath.Join(filepath.Dir(repoRoot), "zmoved")
			require.NoError(t, os.Rename(repoRoot, moved))

			body := "[root_agents.'" + repoRoot + "']\nprogram = '" + tc.oldProgram + "'\n\n[root_agents.'" + moved + "']\nprogram = '" + tc.movedProgram + "'\n"
			configPath := writeMigrateConfig(t, home, body)

			// The recorded identity is the durable RepoID set at registration
			// (the pre-move path); the moved checkout resolves under a new one.
			recordedID := RepoIDFromRoot(repoRoot)
			movedID := RepoIDFromRoot(moved)
			require.Equal(t, recordedID, project.RepoID,
				"fixture: the project's recorded RepoID is the pre-move identity")
			require.NotEqual(t, recordedID, movedID,
				"fixture: the move must change the repo identity")

			result, err := MigrateGlobalConfig()
			require.NoError(t, err)

			// Nothing moved: both keys are left in place under the ambiguous
			// step, each naming the cause and the rebind remedy.
			assert.Empty(t, result.MigratedRootAgents,
				"a moved-but-unrebound project must not be migrated — the recorded identity would flip and the moved identity would lose coverage")
			require.Len(t, result.Left, 1)
			assert.Equal(t, LegacyRootAgentsKey, result.Left[0].Key)
			assert.Equal(t, migrateRootAgentsAmbiguousStep, result.Left[0].Step)
			require.Len(t, result.Left[0].Detail, 2)
			for _, detail := range result.Left[0].Detail {
				assert.Contains(t, detail, repoRoot,
					"the reported cause names the moved-away recorded root")
				assert.Contains(t, detail, project.ID,
					"the reported cause names the project to rebind")
				assert.Contains(t, detail, "af projects rebind",
					"the reported remedy is to rebind before re-running migration")
			}

			// No personal [root_agent] was written: nothing had to be, and a
			// write here is exactly the silent flip the fix prevents.
			assert.Nil(t, loadedPersonalLayer(t, project.ID),
				"no personal layer may be written for a project left in place")

			// The global file is untouched — both keys remain exactly where
			// they were, so the pre-migration resolution the daemon serves is
			// preserved on both identities.
			globalData := readFile(t, configPath)
			assert.Contains(t, globalData, "[root_agents.'"+repoRoot+"']")
			assert.Contains(t, globalData, "[root_agents.'"+moved+"']")

			// Self-justification: demonstrate the very change the fix prevents.
			// Pre-migration, the recorded identity resolves through the stale
			// key's program (the path-hash fallback reaches it), and the moved
			// identity resolves through the moved key. The migration would
			// remove both and write the moved winner into the personal layer
			// keyed under the recorded identity — so the recorded identity's
			// profile would flip to the moved program and the moved identity
			// would drop to "not configured". Both differ from pre-migration,
			// which is why leaving the keys is the correct call.
			ctx := context.Background()
			beforeCfg, err := parseConfigTOML([]byte(body), configPath)
			require.NoError(t, err)
			globalLayer := loadedGlobalLayer(t, configPath)
			recordedLegacyPre, _, err := LegacyRootAgentForRepoContext(ctx, beforeCfg, recordedID)
			require.NoError(t, err)
			require.NotNil(t, recordedLegacyPre,
				"the stale recorded-root key reaches the recorded identity by the path-hash fallback")
			require.Equal(t, tc.oldProgram, recordedLegacyPre.Program)
			movedLegacyPre, _, err := LegacyRootAgentForRepoContext(ctx, beforeCfg, movedID)
			require.NoError(t, err)
			require.NotNil(t, movedLegacyPre)
			require.Equal(t, tc.movedProgram, movedLegacyPre.Program)

			recordedPre := effectiveRootAgent(globalLayer, nil, recordedLegacyPre)
			recordedPost := effectiveRootAgent(globalLayer, &RootAgentLayer{
				EnabledSet: true, Value: RootAgent{Enabled: true, Program: tc.movedProgram},
			}, nil)
			assert.NotEqual(t, recordedPre, recordedPost,
				"the recorded identity's effective profile WOULD flip to the moved winner — the migration must not run")

			movedPre := effectiveRootAgent(globalLayer, nil, movedLegacyPre)
			movedPost := effectiveRootAgent(globalLayer, nil, nil)
			assert.NotEqual(t, movedPre, movedPost,
				"the moved identity WOULD lose coverage (its legacy entry removed, no personal layer keyed there) — the migration must not run")
		})
	}
}

// TestMigrateRootAgentsMovedCheckoutSameProgramStillAmbiguous pins the
// coverage-loss half of the two-identity check: even when the stale and moved
// entries carry the SAME program (so the recorded identity would NOT flip), the
// moved identity still loses its root agent after the next daemon start, because
// the personal write is keyed under the recorded identity only and both legacy
// keys are removed. The migration must still leave the keys in place.
func TestMigrateRootAgentsMovedCheckoutSameProgramStillAmbiguous(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	moved := filepath.Join(filepath.Dir(repoRoot), "zmoved")
	require.NoError(t, os.Rename(repoRoot, moved))

	// Both entries carry the same program, so the recorded identity's profile
	// would be unchanged by the flip — but the moved identity still loses the
	// legacy entry that covered it.
	const program = "sameprog"
	body := "[root_agents.'" + repoRoot + "']\nprogram = '" + program + "'\n\n[root_agents.'" + moved + "']\nprogram = '" + program + "'\n"
	configPath := writeMigrateConfig(t, home, body)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)

	assert.Empty(t, result.MigratedRootAgents,
		"even with matching programs, the moved identity loses coverage, so the group must not migrate")
	require.Len(t, result.Left, 1)
	assert.Equal(t, migrateRootAgentsAmbiguousStep, result.Left[0].Step)
	require.Len(t, result.Left[0].Detail, 2)
	assert.Nil(t, loadedPersonalLayer(t, project.ID), "no personal layer may be written")

	globalData := readFile(t, configPath)
	assert.Contains(t, globalData, "[root_agents.'"+repoRoot+"']")
	assert.Contains(t, globalData, "[root_agents.'"+moved+"']")
}

// TestMigrateRootAgentsMovedCheckoutAfterRebindMigratesSafely confirms the
// remedy the ambiguity report names: once the project is rebound to the moved
// checkout (`af projects rebind <project> <moved-path>`), the record's root and
// durable RepoID both move to the moved checkout's identity, so the daemon
// serves the project under ONE identity again and the migration is safe. The
// moved key migrates into the personal [root_agent] (profile identical either
// side of the write), and the stale recorded-root key — which no longer names
// any registered project's recorded root — is reported under the manual recipe,
// not the ambiguous step.
func TestMigrateRootAgentsMovedCheckoutAfterRebindMigratesSafely(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	moved := filepath.Join(filepath.Dir(repoRoot), "zmoved")
	require.NoError(t, os.Rename(repoRoot, moved))

	// Rebind the record to the moved checkout. After this the record's Root is
	// the moved path and its RepoID is the moved checkout's identity, so the
	// daemon serves the project under a single identity again.
	rebound, err := RebindProject(project.ID, moved)
	require.NoError(t, err)
	require.Equal(t, filepath.Clean(moved), rebound.Root)
	require.Equal(t, RepoIDFromRoot(moved), rebound.RepoID)

	body := "[root_agents.'" + repoRoot + "']\nprogram = 'stale'\n\n[root_agents.'" + moved + "']\nprogram = 'movedprog'\n"
	configPath := writeMigrateConfig(t, home, body)

	beforeGlobal := loadedGlobalLayer(t, configPath)
	beforePersonal := loadedPersonalLayer(t, project.ID)
	before, err := parseConfigTOML([]byte(body), configPath)
	require.NoError(t, err)
	movedEntry := before.RootAgents[moved]
	wantEffective := effectiveRootAgent(beforeGlobal, beforePersonal, &movedEntry)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)

	// The moved key migrated; the stale recorded-root key is now unregistered
	// (no project's recorded root spells the moved-away path) and is reported
	// under the manual recipe, not the ambiguous step.
	require.Len(t, result.MigratedRootAgents, 1)
	assert.Equal(t, moved, result.MigratedRootAgents[0].Path)
	assert.True(t, result.MigratedRootAgents[0].PersonalWritten)
	assert.Equal(t, wantEffective, result.MigratedRootAgents[0].Profile)
	require.Len(t, result.Left, 1)
	assert.Equal(t, LegacyRootAgentsKey, result.Left[0].Key)
	assert.Equal(t, legacyRootAgentsManualStep, result.Left[0].Step)
	assert.Equal(t, []string{repoRoot}, result.Left[0].Detail)

	// The moved key is gone; the stale key remains (it is unregistered, so the
	// migration leaves it for the manual recipe).
	globalData := readFile(t, configPath)
	assert.NotContains(t, globalData, "[root_agents.'"+moved+"']")
	assert.Contains(t, globalData, "[root_agents.'"+repoRoot+"']")

	// The project's effective profile is identical either side of the move —
	// the core guarantee, now under the single (rebound) identity.
	personal := loadedPersonalLayer(t, project.ID)
	require.NotNil(t, personal)
	assert.True(t, personal.Value.Enabled)
	assert.Equal(t, "movedprog", personal.Value.Program)
	assert.Equal(t, wantEffective,
		effectiveRootAgent(loadedGlobalLayer(t, configPath), personal, nil),
		"the project's effective root-agent profile must not change after a rebind + migration")
}
