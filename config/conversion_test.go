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
