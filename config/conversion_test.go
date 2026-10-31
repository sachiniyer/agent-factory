package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aflog "github.com/sachiniyer/agent-factory/log"
)

// Regression + behavior tests for the one-time json→toml conversion (#1030
// PR 2). Sachin's decision: on conversion, config.toml becomes canonical and
// the original config.json is renamed to config.json.bak.

// seedJSONHome points a fresh AGENT_FACTORY_HOME at a temp dir holding only a
// config.json with the given content, and returns the config dir.
func seedJSONHome(t *testing.T, jsonContent string) string {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	fastShell(t)
	configDir, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName), []byte(jsonContent), 0644))
	return configDir
}

func TestConversion_MigratesLegacyJSON(t *testing.T) {
	configDir := seedJSONHome(t, `{
		"default_program": "codex",
		"auto_update": false,
		"daemon_poll_interval": 2500,
		"program_overrides": {"claude": "/opt/claude --dsp", "codex": "/opt/codex --quiet"},
		"log_max_size_mb": 12,
		"log_max_backups": 3,
		"branch_prefix": "team/",
		"worktree_root": "subdirectory",
		"detach_keys": "ctrl-q",
		"update_channel": "preview",
		"limit_patterns": {"claude": "custom limit banner"},
		"limit_auto_resume": true,
		"limit_retry_interval": "45m",
		"root_agents": {"/tmp/repo": {"program": "codex"}}
	}`)
	infoBuf := captureLog(t, &aflog.InfoLog)
	warnBuf := captureLog(t, &aflog.WarningLog)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Settings are preserved through the conversion.
	assert.Equal(t, "codex", cfg.DefaultProgram)
	assert.False(t, cfg.AutoUpdate)
	assert.Equal(t, 2500, cfg.DaemonPollInterval)
	assert.Equal(t, "/opt/claude --dsp", cfg.ProgramOverrides["claude"])
	assert.Equal(t, "/opt/codex --quiet", cfg.ProgramOverrides["codex"])
	assert.Equal(t, 12, cfg.LogMaxSizeMB)
	assert.Equal(t, 3, cfg.LogMaxBackups)
	assert.Equal(t, "team/", cfg.BranchPrefix)
	assert.Equal(t, WorktreeRootSubdirectory, cfg.WorktreeRoot)
	assert.Equal(t, "ctrl-q", cfg.DetachKeys)
	assert.Equal(t, UpdateChannelPreview, cfg.UpdateChannel)
	assert.Equal(t, "custom limit banner", cfg.LimitPatterns["claude"])
	assert.True(t, cfg.LimitAutoResume)
	assert.Equal(t, "45m", cfg.LimitRetryInterval)
	require.Contains(t, cfg.RootAgents, "/tmp/repo")
	rootAgent := cfg.RootAgents["/tmp/repo"]
	assert.Equal(t, "codex", rootAgent.Program)
	assert.NotContains(t, warnBuf.String(), "worktree_root", "worktree_root must be recognized and preserved, not warned as dropped")
	assert.Equal(t, 1, strings.Count(warnBuf.String(), "root_agents is the legacy path map"),
		"one JSON-to-TOML conversion must emit one migration notice")
	assert.Contains(t, warnBuf.String(), "conversion wrote")
	assert.NotContains(t, warnBuf.String(), "leaves this one in place",
		"the conversion warning must describe the write that just occurred, not the read-only remedy")

	// config.toml is now canonical; config.json is moved aside to .bak.
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	assert.FileExists(t, tomlPath)
	assert.NoFileExists(t, filepath.Join(configDir, ConfigFileName))
	assert.FileExists(t, filepath.Join(configDir, ConfigFileName+".bak"))
	assert.Contains(t, infoBuf.String(), "migrated config to TOML")
	tomlData, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Contains(t, string(tomlData), "worktree_root = 'subdirectory'")

	// The .bak still holds the original bytes, verbatim.
	bak, err := os.ReadFile(filepath.Join(configDir, ConfigFileName+".bak"))
	require.NoError(t, err)
	assert.Contains(t, string(bak), `"default_program": "codex"`)

	// Re-loading now reads config.toml canonically, with no duplicate-config
	// warning (config.json is gone).
	reloadWarnBuf := captureLog(t, &aflog.WarningLog)
	cfg2, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg2.DefaultProgram)
	assert.Equal(t, WorktreeRootSubdirectory, cfg2.WorktreeRoot)
	require.Contains(t, cfg2.RootAgents, "/tmp/repo",
		"the legacy path map must survive the JSON upgrade and canonical TOML reload")
	assert.Equal(t, "codex", cfg2.RootAgents["/tmp/repo"].Program)
	assert.NotContains(t, reloadWarnBuf.String(), "both", "a clean conversion leaves no duplicate-config warning")
}

func TestConversion_PreservesExistingBackup(t *testing.T) {
	// Greptile on #1148: a second conversion must never clobber the backup
	// from the first. Scenario — the user converted once (real settings →
	// config.json.bak), later a downgrade regenerated a defaults config.json,
	// and now a new af converts again. The ORIGINAL backup must survive; the
	// new (defaults) backup lands beside it under a non-colliding name.
	configDir := seedJSONHome(t, `{"default_program": "codex"}`)
	original := []byte(`{"default_program": "aider", "auto_update": false}`)
	bakPath := filepath.Join(configDir, ConfigFileName+".bak")
	require.NoError(t, os.WriteFile(bakPath, original, 0644))

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram)

	// The original .bak is byte-for-byte intact.
	got, err := os.ReadFile(bakPath)
	require.NoError(t, err)
	assert.Equal(t, original, got, "the original backup must never be overwritten")

	// The just-converted config.json landed beside it as config.json.bak.1.
	bak1, err := os.ReadFile(bakPath + ".1")
	require.NoError(t, err)
	assert.Contains(t, string(bak1), `"default_program": "codex"`)

	// config.toml is canonical; the live config.json is gone.
	assert.FileExists(t, filepath.Join(configDir, TomlConfigFileName))
	assert.NoFileExists(t, filepath.Join(configDir, ConfigFileName))
}

func TestConversion_InvalidJSONIsNotConvertedOrRenamed(t *testing.T) {
	// A config.json that fails to parse must NOT be converted and NOT renamed:
	// the user needs to fix it in place, and clobbering it would lose data.
	configDir := seedJSONHome(t, `{"default_program": "codex", oops}`)

	cfg, err := LoadConfig()
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "parse config file")

	assert.FileExists(t, filepath.Join(configDir, ConfigFileName), "invalid config.json must stay put")
	assert.NoFileExists(t, filepath.Join(configDir, TomlConfigFileName), "no config.toml on a failed conversion")
	assert.NoFileExists(t, filepath.Join(configDir, ConfigFileName+".bak"))
}

func TestConversion_EnumErrorIsNotConverted(t *testing.T) {
	// A config.json that parses but fails validation (legacy path-in-program)
	// is likewise left in place with an actionable error, never converted.
	configDir := seedJSONHome(t, `{"default_program": "/usr/bin/claude --flag"}`)

	cfg, err := LoadConfig()
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "program_overrides")

	assert.FileExists(t, filepath.Join(configDir, ConfigFileName))
	assert.NoFileExists(t, filepath.Join(configDir, TomlConfigFileName))
}

func TestConversion_WarnsOnUnknownJSONKeyBeforeDropping(t *testing.T) {
	// The frozen JSON reader must name a key it does not recognize, so a
	// setting added to an old config.json is not silently lost on conversion.
	seedJSONHome(t, `{"default_program": "codex", "some_future_key": "v"}`)
	warnBuf := captureLog(t, &aflog.WarningLog)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram)
	assert.Contains(t, warnBuf.String(), "some_future_key")
	assert.Contains(t, warnBuf.String(), "unknown key")
}

func TestConversion_CrashAfterWriteBeforeRenameLeavesTOMLCanonical(t *testing.T) {
	// Simulate a crash between the atomic config.toml write and the .bak
	// rename: both files remain. The current load still returns the converted
	// config, and the NEXT load treats config.toml as canonical (with the
	// duplicate-config warning) — no data is lost in either state.
	configDir := seedJSONHome(t, `{"default_program": "codex"}`)

	convertRenameFailForTest = func() error { return assert.AnError }
	warnBuf := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadConfig()
	convertRenameFailForTest = nil
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram)

	// config.toml written; config.json still present (rename "failed").
	assert.FileExists(t, filepath.Join(configDir, TomlConfigFileName))
	assert.FileExists(t, filepath.Join(configDir, ConfigFileName))
	assert.Contains(t, warnBuf.String(), "could not move the original")

	// Next load: config.toml wins, config.json flagged as ignored.
	warn2 := captureLog(t, &aflog.WarningLog)
	cfg2, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg2.DefaultProgram)
	assert.Contains(t, warn2.String(), "both")
	assert.Contains(t, warn2.String(), "is canonical")
}

func TestConversion_DowngradeThenReupgradePreservesTOML(t *testing.T) {
	// After conversion the user downgrades to a pre-TOML af, which finds no
	// config.json and materializes a defaults config.json beside the user's
	// config.toml (the #837 loud-materialize path in the old binary). On the
	// next new-af run, config.toml must still win — the user's real settings
	// survive the round trip; only a "both files" warning results.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	fastShell(t)
	configDir, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte("default_program = 'codex'\n"), 0644))
	// The stand-in for what an old binary would regenerate: a defaults JSON.
	require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName), []byte(`{"default_program": "claude"}`), 0644))

	warnBuf := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram, "the user's config.toml must win over a downgrade-regenerated config.json")
	assert.Contains(t, warnBuf.String(), "both")
}

func TestConversion_LostRaceAdoptsWinnersTOML(t *testing.T) {
	// A concurrent process wins the conversion lock first: by the time this
	// process enters the lock body, config.toml already exists and config.json
	// has been renamed. This process must adopt the winner's config.toml and
	// NOT re-convert or re-rename.
	configDir := seedJSONHome(t, `{"default_program": "codex"}`)

	convertRaceHookForTest = func() {
		// The winner's effect: config.toml written, config.json moved to .bak.
		require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte("default_program = 'aider'\n"), 0644))
		require.NoError(t, os.Rename(filepath.Join(configDir, ConfigFileName), filepath.Join(configDir, ConfigFileName+".bak")))
	}
	t.Cleanup(func() { convertRaceHookForTest = nil })

	cfg, err := LoadConfig()
	require.NoError(t, err)
	// The winner wrote aider, not our codex — we adopted its file.
	assert.Equal(t, "aider", cfg.DefaultProgram)
	assert.FileExists(t, filepath.Join(configDir, TomlConfigFileName))
	assert.NoFileExists(t, filepath.Join(configDir, ConfigFileName))
}

// retargetableJSONHome stages the conversion's supported arrangement: an
// AGENT_FACTORY_HOME that is itself a symlink "home → A", where A holds a
// legacy config.json (and no config.toml, so the one-time conversion runs),
// beside a peer directory B holding its OWN active config.json and a
// pre-existing config.json.bak that must survive any retarget. The home is a
// symlink holding regular files, so resolveWriteTarget(home/config.toml) returns
// the path unchanged — the #3697 premise, not the #3688 one — which is exactly
// the shape that left the conversion's path-based config.json legs uncovered.
//
// It mirrors retargetableHome (which stages the config.toml shape for the
// migrate/pin tests) for the conversion's legacy sibling.
func retargetableJSONHome(t *testing.T, aJSON, bJSON, bBak string) (tomlLink, home, first, second string) {
	t.Helper()
	root := t.TempDir()
	first = filepath.Join(root, "A")
	second = filepath.Join(root, "B")
	require.NoError(t, os.Mkdir(first, 0o755))
	require.NoError(t, os.Mkdir(second, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(first, ConfigFileName), []byte(aJSON), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(second, ConfigFileName), []byte(bJSON), 0644))
	if bBak != "" {
		require.NoError(t, os.WriteFile(filepath.Join(second, ConfigFileName+".bak"), []byte(bBak), 0644))
	}
	home = filepath.Join(root, "af-home")
	require.NoError(t, os.Symlink(first, home))
	tomlLink = filepath.Join(home, TomlConfigFileName)
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)

	// Premise, asserted rather than assumed: config.toml in A is a regular
	// file (it does not exist yet, so the home link is the only link), so the
	// file-level #3688 pin has nothing to pin and the conversion's path-based
	// legacy legs are the ones the pinned directory must cover.
	resolved, err := resolveWriteTarget(tomlLink)
	require.NoError(t, err)
	require.Equal(t, tomlLink, resolved,
		"premise: the home is the link, config.toml is not, so resolveWriteTarget returns the path unchanged")
	return tomlLink, home, first, second
}

// TestConversionKeepsTheLegacyRenameInsideTheLockedDirectory is the conversion's
// counterpart to TestMigrationBackupLandsInTheLockedDirectory, and the end-to-end
// reproduction of the bug report: convertJSONToTOML locks config.toml and then
// reads, backs up and renames its SIBLING config.json. Those three steps were
// path-based, so a home link retargeted mid-conversion (stow, chezmoi, a dotfiles
// branch switch) resolved each one through the moved link — adopting the
// retargeted directory's config.json, choosing a backup slot there, and
// renaming its active config.json to a .bak — while LoadConfig reported success
// and left that home with neither config.json nor config.toml.
//
// The body now reads the legacy file, picks the backup slot, confirms, and
// renames through the pinned directory: a mid-conversion retarget lands every
// step in the locked directory (followed nowhere) or is refused loudly, never
// silently renaming another home's active config.
func TestConversionKeepsTheLegacyRenameInsideTheLockedDirectory(t *testing.T) {
	const aJSON = `{"default_program": "codex"}`
	const bJSON = `{"default_program": "aider"}`
	const preciousBak = "schema_version = 1\n# do not lose me\n"

	_, home, first, second := retargetableJSONHome(t, aJSON, bJSON, preciousBak)
	bakInB := filepath.Join(second, ConfigFileName+".bak")

	// Retarget the home link inside locked.write — after the config.toml
	// write's confirm and before its bytes land, the window confirm cannot
	// close — the same window TestFollowedLockPinsTheDirectoryItLocked drives
	// for the plain write. After this, the link names B while the locked
	// directory is still A.
	followedWriteRaceHookForTest = func() { retargetHome(t, home, second) }
	t.Cleanup(func() { followedWriteRaceHookForTest = nil })

	warnBuf := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadConfig()
	require.NoError(t, err,
		"the conversion must succeed: config.toml landed in the locked directory under the lock")
	assert.Equal(t, "codex", cfg.DefaultProgram,
		"the returned config is the one converted from the LOCKED directory's config.json, "+
			"read through the pinned fd — not the retargeted directory's")

	// The locked directory A got its conversion; nothing of it reached B.
	assert.Contains(t, readString(t, filepath.Join(first, TomlConfigFileName)), "codex",
		"config.toml landed in the directory the lock covers")
	// The rename was refused loudly: the confirm before the move caught the
	// retarget, so the legacy config.json stays in the locked directory
	// (config.toml is canonical next load) rather than being moved aside by a
	// link that no longer names this home.
	assert.FileExists(t, filepath.Join(first, ConfigFileName),
		"the locked directory's config.json is left in place when the link moved before the rename")
	assert.NoFileExists(t, filepath.Join(first, ConfigFileName+".bak"),
		"no backup was created in the locked directory when the move was refused rather than misdirected")
	assert.Contains(t, warnBuf.String(), "could not move the original",
		"a refused rename is logged, not silent — the user is told the link move cost the move-aside")
	// The refusal names the pinned source the lock still covers (the original is
	// in the locked directory A, not where the moved link now reaches) and does
	// NOT point a "delete it yourself" instruction at the link — that now reaches
	// B's active config.json, and following it could delete another home's only
	// config.
	assert.Contains(t, warnBuf.String(), "the original remains at",
		"the refusal names where the original actually sits (the pinned directory), not the moved link")
	assert.Contains(t, warnBuf.String(), filepath.Join(first, ConfigFileName),
		"the named location is the locked directory's config.json, not the link that now reaches B")
	assert.Contains(t, warnBuf.String(), filepath.Join(first, TomlConfigFileName),
		"the refusal names the pinned TOML the lock covers, not the link that now reaches B")
	assert.NotContains(t, warnBuf.String(), "yourself to silence the duplicate-config warning",
		"a retarget refusal must not invite the user to delete through a link that no longer names the original")

	// Nothing of the conversion reached B: its active config.json, its precious
	// .bak, and the absence of a config.toml/config.json.bak.* all survive.
	assert.Equal(t, bJSON, readString(t, filepath.Join(second, ConfigFileName)),
		"B's active config.json must survive untouched — the move never followed the link there")
	assert.Equal(t, preciousBak, readString(t, bakInB),
		"B's pre-existing .bak must survive — the slot was chosen against the locked directory, not the retargeted one")
	assert.NoFileExists(t, filepath.Join(second, TomlConfigFileName),
		"no config.toml was written in the retargeted directory")
	assert.Equal(t, []string{ConfigFileName, ConfigFileName + ".bak"}, entryNames(t, second),
		"nothing of the conversion — no config.toml, no .bak.N, no .lock — was left in the retargeted directory")
}

// TestConversionWithNoRetargetStillMovesConfigJSONAside is the happy-path pin
// after the pinned-rename change: when the home link does not move, the confirm
// passes and the legacy config.json is still renamed to its .bak inside the
// pinned directory, exactly as before. The previous regression tests cover the
// values; this one covers the move-aside against the new pinned-rename plumbing.
func TestConversionWithNoRetargetStillMovesConfigJSONAside(t *testing.T) {
	const aJSON = `{"default_program": "codex"}`
	const bJSON = `{"default_program": "aider"}`
	// B is staged but unused: the home stays pointed at A throughout.
	_, _, first, _ := retargetableJSONHome(t, aJSON, bJSON, "")

	infoBuf := captureLog(t, &aflog.InfoLog)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram)

	// config.toml written, the legacy config.json moved aside to .bak.
	assert.Contains(t, readString(t, filepath.Join(first, TomlConfigFileName)), "codex")
	assert.NoFileExists(t, filepath.Join(first, ConfigFileName),
		"the legacy config.json was renamed to .bak")
	assert.Contains(t, readString(t, filepath.Join(first, ConfigFileName+".bak")), aJSON,
		"the .bak beside the locked file holds the original config.json verbatim")
	assert.Contains(t, infoBuf.String(), "migrated config to TOML",
		"a clean conversion reports the move, not a refused-rename warning")

	// A clean reload reads config.toml canonically with no duplicate-config
	// warning, because config.json is gone.
	reloadWarn := captureLog(t, &aflog.WarningLog)
	_, err = LoadConfig()
	require.NoError(t, err)
	assert.NotContains(t, reloadWarn.String(), "both",
		"the reload after a clean conversion has no duplicate-config warning (config.json is gone)")
	assert.NotContains(t, reloadWarn.String(), "could not move the original",
		"the reload after a clean conversion has no refused-rename warning")
}

// TestConversion_RefusesTOMLSymlinkOutOfTheHome pins the guard against the
// one-shot race a freshly introduced config.toml symlink opens (review on this
// PR). The conversion locks config.toml and reads/renames its sibling config.json
// through the pinned directory; that is correct only while config.json is a
// sibling of the locked file. A config.toml installed as a symlink to ANOTHER
// directory between LoadConfig observing it missing and the lock pinning it
// points the pinned fd at the symlink target's directory rather than the home
// holding the legacy config.json — so readSibling would open an unrelated file
// there (or, absent one, return ENOENT and silently leave the real legacy
// configuration unconverted). The guard refuses that up front instead of
// mis-reading or mis-renaming.
//
// It calls convertJSONToTOML directly to stage the post-pin state LoadConfig's
// own toml-existence check would otherwise short-circuit (a config.toml that
// already exists is read, not converted): here config.toml is a symlink to an
// empty file in another directory, the shape the lock pins in the race.
func TestConversion_RefusesTOMLSymlinkOutOfTheHome(t *testing.T) {
	const aJSON = `{"default_program":"codex"}`

	for _, tc := range []struct {
		name         string
		stageSibling string // content for an unrelated config.json placed in the symlink target dir; "" to leave it absent
		errContains  string
	}{
		{
			name:         "no config.json in the symlink target directory",
			stageSibling: "",
			errContains:  "is not in the directory the lock covers",
		},
		{
			name:         "an unrelated config.json in the symlink target directory",
			stageSibling: `{"default_program":"impostor"}`,
			errContains:  "is not the legacy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			require.NoError(t, os.Mkdir(home, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), []byte(aJSON), 0o644))
			t.Setenv("AGENT_FACTORY_HOME", home)
			fastShell(t)

			// config.toml in the home is a symlink to an empty file in another
			// directory — the shape a peer process installing config.toml as a
			// symlink during the conversion window pins the lock onto.
			elsedir := filepath.Join(root, "elsewhere")
			require.NoError(t, os.Mkdir(elsedir, 0o755))
			tomlTarget := filepath.Join(elsedir, TomlConfigFileName)
			require.NoError(t, os.WriteFile(tomlTarget, []byte(""), 0o644))
			tomlLink := filepath.Join(home, TomlConfigFileName)
			require.NoError(t, os.Symlink(tomlTarget, tomlLink))
			if tc.stageSibling != "" {
				require.NoError(t, os.WriteFile(filepath.Join(elsedir, ConfigFileName), []byte(tc.stageSibling), 0o644))
			}

			configPath := filepath.Join(home, ConfigFileName)
			cfg, err := convertJSONToTOML(configPath, tomlLink, configPath, tomlLink)
			require.Error(t, err, "a config.toml symlinked out of the home must not let the conversion read or rename an unrelated file")
			assert.Nil(t, cfg)
			assert.Contains(t, err.Error(), tc.errContains,
				"the refusal names the directory the lock covers vs. the one holding the legacy config.json")

			// Nothing was converted: the home's legacy config.json is untouched,
			// no config.toml/.bak/.lock was left in the home, and the symlink
			// target directory is unchanged apart from its pre-existing empty toml.
			assert.Equal(t, aJSON, readString(t, configPath),
				"the real legacy config.json is left in place, not renamed by a misdirected move")
			assert.NoFileExists(t, filepath.Join(home, ConfigFileName+".bak"),
				"no backup was created in the home")
			info, lerr := os.Lstat(tomlLink)
			require.NoError(t, lerr)
			assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
				"the config.toml symlink the peer installed is left for the operator to repair")
			if tc.stageSibling == "" {
				assert.NoFileExists(t, filepath.Join(elsedir, ConfigFileName),
					"nothing was created in the directory the lock mistakenly covered")
			} else {
				assert.Equal(t, tc.stageSibling, readString(t, filepath.Join(elsedir, ConfigFileName)),
					"the unrelated config.json in the symlink target directory is untouched")
			}
		})
	}
}

// TestConversionRenameRefusedAfterConfirmRechecksLink reproduces the window the
// post-confirm recheck added for the codex finding on line 624: confirm()
// passes before the rename, but a home link retarget between confirm and
// renameSibling leaves prettyConfigPath pointing at another home's active
// config.json. Without the recheck, the "delete it yourself" instruction
// would tell the user to delete the wrong file. The recheck turns the
// refusal into a retarget refusal and names the pinned source. The hook
// also removes the source the rename reaches for, so renameSibling returns
// ENOENT; the ENOENT is classified independently of the retarget (the codex
// finding on line 645), so the warning says the original may no longer exist
// at the pinned source rather than asserting it remains and recommending a
// rerun that has nothing to move.
func TestConversionRenameRefusedAfterConfirmRechecksLink(t *testing.T) {
	const aJSON = `{"default_program": "codex"}`
	const bJSON = `{"default_program": "aider"}`
	const preciousBak = "schema_version = 1\n# do not lose me\n"

	_, home, first, second := retargetableJSONHome(t, aJSON, bJSON, preciousBak)

	// Retarget the home link and remove the source between the pre-rename
	// confirm and renameSibling — the check-then-act window the recheck must
	// close. The retarget alone does not make renameSibling fail (it goes
	// through the pinned fd), so remove the source the rename reaches for
	// too; that is the concurrent-removal case the finding names, and it
	// makes the ENOENT classification below observable.
	convertRenameRaceHookForTest = func() {
		retargetHome(t, home, second)
		require.NoError(t, os.Remove(filepath.Join(first, ConfigFileName)))
	}
	t.Cleanup(func() { convertRenameRaceHookForTest = nil })

	warnBuf := captureLog(t, &aflog.WarningLog)
	_, err := LoadConfig()
	require.NoError(t, err,
		"the conversion must succeed: config.toml landed in the locked directory under the lock")

	// The recheck caught the retarget: the refusal names the pinned source the
	// lock still covers (the original in the locked directory A) and does NOT
	// invite the user to delete through the link that now reaches B's active
	// config.json.
	assert.Contains(t, warnBuf.String(), "could not move the original",
		"a refused rename is logged, not silent — the user is told the link move cost the move-aside")
	assert.Contains(t, warnBuf.String(), "may no longer exist at",
		"an ENOENT rename under a retarget says the original may be gone, not that it remains — the source was removed during conversion")
	assert.Contains(t, warnBuf.String(), filepath.Join(first, ConfigFileName),
		"the named location is the locked directory's config.json, not the link that now reaches B")
	assert.NotContains(t, warnBuf.String(), "yourself to silence the duplicate-config warning",
		"a retarget refusal must not invite the user to delete through a link that no longer names the original")
	assert.NotContains(t, warnBuf.String(), "re-run to move it aside",
		"there is nothing to move aside when the source was removed during conversion")

	// The rename failed (the source was removed concurrently), so no
	// backup was created in the locked directory.
	assert.NoFileExists(t, filepath.Join(first, ConfigFileName+".bak"),
		"no backup was created in the locked directory when the rename failed rather than misdirected")
	// The locked directory A got its conversion; nothing reached B.
	assert.Contains(t, readString(t, filepath.Join(first, TomlConfigFileName)), "codex",
		"config.toml landed in the directory the lock covers")
	assert.Equal(t, bJSON, readString(t, filepath.Join(second, ConfigFileName)),
		"B's active config.json must survive untouched — the move never followed the link there")
	assert.Equal(t, preciousBak, readString(t, filepath.Join(second, ConfigFileName+".bak")),
		"B's pre-existing .bak must survive — the slot was chosen against the locked directory, not the retargeted one")
	assert.NoFileExists(t, filepath.Join(second, TomlConfigFileName),
		"no config.toml was written in the retargeted directory")
}

// TestConversionAcceptsConcurrentJSONUpdate pins the directory-level comparison
// added for the codex finding on filelock.go:181 — confirmSiblingDir now
// compares the parent directory siblingPath reaches with dirFD instead of the
// file's own inode, so an editor that atomically replaces config.json (same
// directory, different inode) is accepted rather than tripping a false
// out-of-home-symlink refusal.
func TestConversionAcceptsConcurrentJSONUpdate(t *testing.T) {
	const aJSON = `{"default_program": "codex"}`
	const replacement = `{"default_program": "aider"}`

	root := t.TempDir()
	home := filepath.Join(root, "home")
	require.NoError(t, os.Mkdir(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), []byte(aJSON), 0o644))
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)

	configDir, err := GetConfigDir()
	require.NoError(t, err)
	configPath := filepath.Join(configDir, ConfigFileName)
	tomlPath := filepath.Join(configDir, TomlConfigFileName)

	// Replace config.json atomically (rename tmp → config.json) between the
	// Stat(siblingPath) and the directory comparison in confirmSiblingDir —
	// same directory, different inode. Without the directory-level check the
	// conversion would abort with a false out-of-home-symlink error.
	confirmSiblingDirStatHookForTest = func() {
		tmp := configPath + ".tmp"
		require.NoError(t, os.WriteFile(tmp, []byte(replacement), 0o644))
		require.NoError(t, os.Rename(tmp, configPath))
	}
	t.Cleanup(func() { confirmSiblingDirStatHookForTest = nil })

	cfg, err := LoadConfig()
	require.NoError(t, err,
		"the conversion must accept an in-place atomic config.json rewrite in the pinned directory")
	assert.Equal(t, "aider", cfg.DefaultProgram,
		"the converted config reflects the replacement, not the pre-stat original")
	assert.FileExists(t, tomlPath, "config.toml was written from the replacement")
}

// TestConversionAcceptsConcurrentlyRemovedSibling pins the parent-directory
// classification added for the codex finding on filelock.go:182 — when an
// editor or remover deletes config.json after confirmSiblingDir's Stat
// succeeds but before its Fstatat lookup, the ENOENT from Fstatat is in the
// SAME directory the lock covers, so it is a concurrently removed sibling
// (readSibling answers with defaults) rather than the freshly introduced
// TOML-symlink race. Without the classification the ENOENT would abort
// startup with a misleading out-of-home-symlink error.
func TestConversionAcceptsConcurrentlyRemovedSibling(t *testing.T) {
	const aJSON = `{"default_program": "codex"}`

	root := t.TempDir()
	home := filepath.Join(root, "home")
	require.NoError(t, os.Mkdir(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), []byte(aJSON), 0o644))
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)

	configDir, err := GetConfigDir()
	require.NoError(t, err)
	configPath := filepath.Join(configDir, ConfigFileName)
	tomlPath := filepath.Join(configDir, TomlConfigFileName)

	// Remove config.json between confirmSiblingDir's Stat(siblingPath) and its
	// Fstatat lookup — the check-then-act window an editor or rm slips into.
	confirmSiblingDirStatHookForTest = func() {
		require.NoError(t, os.Remove(configPath))
	}
	t.Cleanup(func() { confirmSiblingDirStatHookForTest = nil })

	cfg, err := LoadConfig()
	require.NoError(t, err,
		"a concurrently removed sibling is readSibling's to answer with defaults, not a refusal that aborts startup")
	assert.Equal(t, DefaultConfig().DefaultProgram, cfg.DefaultProgram,
		"the load falls back to built-in defaults when the legacy config.json vanishes under the lock")
	assert.NoFileExists(t, tomlPath,
		"no conversion is written when the legacy config.json is gone before the read")
	assert.NoFileExists(t, configPath,
		"the concurrently removed config.json stays gone")
}

// TestConversionOrdinaryRenameFailureNamesPinnedSource pins the codex finding
// on config_load.go:648: an ordinary rename failure (no retarget) must not
// point a "delete it yourself" instruction through the mutable home link,
// which by the time the user follows it could reach another home's active
// config. Every rename failure now names the pinned source the lock still
// covers instead, the same as a retarget refusal.
func TestConversionOrdinaryRenameFailureNamesPinnedSource(t *testing.T) {
	const aJSON = `{"default_program": "codex"}`
	const bJSON = `{"default_program": "aider"}`

	// B is staged but unused: the home stays pointed at A throughout, so the
	// pre-rename confirm passes and the rename fails for an ordinary reason.
	_, _, first, _ := retargetableJSONHome(t, aJSON, bJSON, "")

	convertRenameFailForTest = func() error { return assert.AnError }
	t.Cleanup(func() { convertRenameFailForTest = nil })

	warnBuf := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadConfig()
	require.NoError(t, err,
		"the conversion must succeed: config.toml landed in the locked directory under the lock")
	assert.Equal(t, "codex", cfg.DefaultProgram)

	assert.Contains(t, warnBuf.String(), "could not move the original",
		"a failed rename is logged, not silent")
	assert.Contains(t, warnBuf.String(), "the original remains at",
		"an ordinary rename failure names where the original sits (the pinned directory), not the mutable link")
	assert.Contains(t, warnBuf.String(), filepath.Join(first, ConfigFileName),
		"the named location is the locked directory's config.json, not the link that could later reach another home")
	assert.NotContains(t, warnBuf.String(), "yourself to silence the duplicate-config warning",
		"an ordinary rename failure must not point a deletion instruction through the mutable home link")
}

// TestConversion_RefusesTOMLSymlinkOutOfTheHomeWhenLegacyVanished pins the guard
// added for the codex finding on filelock.go:167 — the early ENOENT branch of
// confirmSiblingDir. When config.json is removed after LoadConfig read it while
// another process installs config.toml as a symlink to a different directory,
// dirFD points at the symlink target's directory; the prior Stat(configPath)
// reports ENOENT and, without the directory-identity check, readSibling would
// open an unrelated config.json there and the rename would move it aside. The
// guard now refuses that one-shot symlink race up front rather than mis-reading
// or mis-renaming, the same as the present-sibling branch.
//
// It calls convertJSONToTOML directly to stage the post-pin state LoadConfig's
// own toml-existence check would otherwise short-circuit: here config.toml is a
// symlink to an empty file in another directory and the legacy config.json is
// GONE from the home (removed after LoadConfig read it).
func TestConversion_RefusesTOMLSymlinkOutOfTheHomeWhenLegacyVanished(t *testing.T) {
	const impostor = `{"default_program":"impostor"}`

	root := t.TempDir()
	home := filepath.Join(root, "home")
	require.NoError(t, os.Mkdir(home, 0o755))
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)

	// config.toml in the home is a symlink to an empty file in another
	// directory — the shape a peer process installing config.toml as a
	// symlink during the conversion window pins the lock onto. The legacy
	// config.json is gone from the home (removed after LoadConfig read it).
	elsedir := filepath.Join(root, "elsewhere")
	require.NoError(t, os.Mkdir(elsedir, 0o755))
	tomlTarget := filepath.Join(elsedir, TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlTarget, []byte(""), 0o644))
	tomlLink := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.Symlink(tomlTarget, tomlLink))
	// An unrelated config.json in the symlink target directory: without the
	// guard, readSibling would open and convert this one.
	require.NoError(t, os.WriteFile(filepath.Join(elsedir, ConfigFileName), []byte(impostor), 0o644))

	configPath := filepath.Join(home, ConfigFileName)
	cfg, err := convertJSONToTOML(configPath, tomlLink, configPath, tomlLink)
	require.Error(t, err, "a vanished legacy config.json under a config.toml symlinked out of the home must not let the conversion read or rename an unrelated file")
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "is not in the directory the lock covers",
		"the refusal names the directory the lock covers vs. the one the vanished legacy path reaches")

	// Nothing was converted: no backup was created in the home, and the
	// symlink target directory's unrelated config.json is untouched.
	assert.NoFileExists(t, filepath.Join(home, ConfigFileName+".bak"),
		"no backup was created in the home")
	assert.Equal(t, impostor, readString(t, filepath.Join(elsedir, ConfigFileName)),
		"the unrelated config.json in the symlink target directory is untouched")
	info, lerr := os.Lstat(tomlLink)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"the config.toml symlink the peer installed is left for the operator to repair")
}

// TestConversionRenameFailureENOENTSaysOriginalMayNoLongerExist pins the codex
// finding on config_load.go:656: when an editor removes config.json after the
// locked TOML write but before renameSibling — home stable, so the pre-rename
// confirm passes — Renameat returns ENOENT and the original no longer sits at
// the pinned source. The warning must not assert the original remains there
// and recommend a rerun that has nothing to move; it distinguishes that ENOENT
// from an ordinary rename failure.
func TestConversionRenameFailureENOENTSaysOriginalMayNoLongerExist(t *testing.T) {
	const aJSON = `{"default_program": "codex"}`
	const bJSON = `{"default_program": "aider"}`

	// B is staged but unused: the home stays pointed at A throughout, so the
	// pre-rename confirm passes and the rename fails because the source was
	// concurrently removed, not because the link moved.
	_, _, first, _ := retargetableJSONHome(t, aJSON, bJSON, "")

	// Remove config.json between confirm and renameSibling — the
	// check-then-act window an editor or rm slips into. The home stays on A,
	// so the recheck confirm passes and the refusal is classified as an
	// ordinary (non-retarget) rename failure carrying ENOENT.
	convertRenameRaceHookForTest = func() {
		require.NoError(t, os.Remove(filepath.Join(first, ConfigFileName)))
	}
	t.Cleanup(func() { convertRenameRaceHookForTest = nil })

	warnBuf := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadConfig()
	require.NoError(t, err,
		"the conversion must succeed: config.toml landed in the locked directory under the lock")
	assert.Equal(t, "codex", cfg.DefaultProgram)

	assert.Contains(t, warnBuf.String(), "could not move the original",
		"a failed rename is logged, not silent")
	assert.Contains(t, warnBuf.String(), "may no longer exist at",
		"a rename failure caused by a concurrently removed source says the original may be gone, not that it remains")
	assert.NotContains(t, warnBuf.String(), "re-run to move it aside",
		"there is nothing to move aside when the source was removed during conversion")
	assert.NotContains(t, warnBuf.String(), "yourself to silence the duplicate-config warning",
		"an ordinary rename failure must not point a deletion instruction through the mutable home link")

	// No backup was created in the locked directory: the rename failed
	// because the source was gone, not because it was misdirected.
	assert.NoFileExists(t, filepath.Join(first, ConfigFileName+".bak"),
		"no backup was created when the source was removed before the rename")
	assert.Contains(t, readString(t, filepath.Join(first, TomlConfigFileName)), "codex",
		"config.toml landed in the directory the lock covers")
}

// TestConversionRenameSucceedsAfterRetargetReportsRetarget pins the codex finding
// on config_load.go:615: when the home link retargets between the pre-rename
// confirm and renameSibling but the source is NOT removed, renameSibling still
// SUCCEEDS — it goes through the pinned fd, so the move lands in the locked
// directory whatever the link did. Without a post-rename recheck the
// conversion would log an unqualified success through prettyTomlPath /
// prettyHomePath(bakPath), which after a retarget reach the NEW home, while the
// files this conversion touched stay in the locked directory. The post-rename
// recheck catches the retarget and names the pinned locations instead of an
// unqualified success.
func TestConversionRenameSucceedsAfterRetargetReportsRetarget(t *testing.T) {
	const aJSON = `{"default_program": "codex"}`
	const bJSON = `{"default_program": "aider"}`

	_, home, first, second := retargetableJSONHome(t, aJSON, bJSON, "")

	// Retarget the home link between the pre-rename confirm and
	// renameSibling, but leave the source in place — so the rename goes
	// through the pinned fd and SUCCEEDS. The retarget alone does not make
	// renameSibling fail; only the post-rename recheck catches it.
	convertRenameRaceHookForTest = func() {
		retargetHome(t, home, second)
	}
	t.Cleanup(func() { convertRenameRaceHookForTest = nil })

	warnBuf := captureLog(t, &aflog.WarningLog)
	infoBuf := captureLog(t, &aflog.InfoLog)
	cfg, err := LoadConfig()
	require.NoError(t, err,
		"the conversion must succeed: config.toml landed in the locked directory under the lock")
	assert.Equal(t, "codex", cfg.DefaultProgram,
		"the returned config is the one converted from the locked directory's config.json, read through the pinned fd")

	// The post-rename recheck caught the retarget: the warning names the
	// pinned locations the lock still covers, not the moved link.
	assert.Contains(t, warnBuf.String(), "the home symlink moved during the conversion",
		"a successful rename under a retarget is reported as a retarget, not an unqualified success")
	assert.Contains(t, warnBuf.String(), filepath.Join(first, TomlConfigFileName),
		"the warning names the pinned TOML the lock covers, not the link that now reaches B")
	assert.Contains(t, warnBuf.String(), filepath.Join(first, ConfigFileName+".bak"),
		"the warning names the pinned backup the lock covers, not the link that now reaches B")
	assert.NotContains(t, infoBuf.String(), "migrated config to TOML: wrote",
		"the unqualified success message is suppressed when the home moved during the conversion")

	// The rename landed in the locked directory A; nothing reached B.
	assert.Contains(t, readString(t, filepath.Join(first, TomlConfigFileName)), "codex",
		"config.toml landed in the directory the lock covers")
	assert.NoFileExists(t, filepath.Join(first, ConfigFileName),
		"the locked directory's config.json was moved aside to its .bak inside the pinned directory")
	assert.FileExists(t, filepath.Join(first, ConfigFileName+".bak"),
		"the backup landed in the locked directory, not the retargeted one")
	assert.Equal(t, bJSON, readString(t, filepath.Join(second, ConfigFileName)),
		"B's active config.json must survive untouched — the move never followed the link there")
	assert.NoFileExists(t, filepath.Join(second, TomlConfigFileName),
		"no config.toml was written in the retargeted directory")
}
