package config

import (
	"path/filepath"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The [account_handoff] table is a parser/schema slice of the automatic
// account-handoff feature: the values are validated, resolved, and writable,
// but nothing consumes them yet. Every fixture below is handwritten from the
// table's record shape only — no real session titles, conversation text, ids,
// or account names appear.

func TestAccountHandoffAbsentTableIsOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "default_program = \"claude\"\n")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, defaultAccountHandoffConfig(), cfg.AccountHandoff,
		"an absent table must resolve to the compiled defaults, byte-for-byte today's behaviour")
	assert.Equal(t, AccountHandoffModeOff, cfg.AccountHandoff.Mode,
		"absent table ≡ off — the engine slice may read Mode and see no opt-in")

	resolved, err := ResolveGlobalConfig()
	require.NoError(t, err)
	assert.Equal(t, defaultAccountHandoffConfig(), resolved.AccountHandoff)

	value, ok := resolved.ResolvedValue("account_handoff")
	require.True(t, ok)
	for _, c := range value.Candidates {
		if c.Layer == SourceGlobal.String() {
			assert.False(t, c.Present, "the global layer must report the absent table as absent")
		}
	}
}

func TestAccountHandoffParsesFullTable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, `
[account_handoff]
mode                 = "shadow"
include_pinned       = true
target_order         = "emptiest"
confirm_wall         = false
wall_percent         = 90
move_idle_at_percent = 80
prefer_below_percent = 60
probe_ttl            = "15m"
default_login        = "move"
require_carry        = true
exclude_sessions     = ["alpha-session", "beta-session"]
max_moves            = 5
max_moves_window     = "1h"
refused_backoff      = "45m"
on_failure           = "skip"
report_to            = "project-a/status-session"
`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	got := cfg.AccountHandoff
	assert.Equal(t, "shadow", got.Mode)
	assert.True(t, got.IncludePinned)
	assert.Equal(t, "emptiest", got.TargetOrder)
	assert.False(t, got.ConfirmWall)
	assert.Equal(t, 90, got.WallPercent)
	assert.Equal(t, 80, got.MoveIdleAtPercent)
	assert.Equal(t, 60, got.PreferBelowPercent)
	assert.Equal(t, "15m", got.ProbeTTL)
	assert.Equal(t, "move", got.DefaultLogin)
	assert.True(t, got.RequireCarry)
	assert.Equal(t, []string{"alpha-session", "beta-session"}, got.ExcludeSessions)
	assert.Equal(t, 5, got.MaxMoves)
	assert.Equal(t, "1h", got.MaxMovesWindow)
	assert.Equal(t, "45m", got.RefusedBackoff)
	assert.Equal(t, "skip", got.OnFailure)
	assert.Equal(t, "project-a/status-session", got.ReportTo)
}

func TestAccountHandoffRejectsLiveMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "[account_handoff]\nmode = \"live\"\n")

	_, err := LoadConfig()
	require.Error(t, err, "mode = \"live\" is rejected until the live-mover slice ships")
	assert.Contains(t, err.Error(), "account_handoff.mode")
	assert.Contains(t, err.Error(), "live")
}

func TestAccountHandoffRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"mode":             "mode = \"shadowy\"\n",
		"target_order":     "target_order = \"fullest\"\n",
		"default_login":    "default_login = \"hijack\"\n",
		"on_failure":       "on_failure = \"retry\"\n",
		"wall low":         "wall_percent = 30\n",
		"wall high":        "wall_percent = 101\n",
		"idle low":         "move_idle_at_percent = 10\n",
		"prefer low":       "prefer_below_percent = 40\n",
		"ordering":         "move_idle_at_percent = 98\nwall_percent = 97\n",
		"prefer ordering":  "prefer_below_percent = 96\nmove_idle_at_percent = 95\n",
		"max_moves":        "max_moves = -1\n",
		"max_moves zero":   "max_moves = 0\n",
		"probe_ttl":        "probe_ttl = \"soon\"\n",
		"max_moves_window": "max_moves_window = \"an hourish\"\n",
		"refused_backoff":  "refused_backoff = \"2hours-ish\"\n",
		"exclude empty":    "exclude_sessions = [\"\"]\n",
	}
	for name, leaves := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			writeGlobalTOML(t, home, "[account_handoff]\n"+leaves)

			_, err := LoadConfig()
			require.Error(t, err, "invalid leaf must fail the load")
			assert.Contains(t, err.Error(), "account_handoff")
		})
	}
}

// TestAccountHandoffRejectsMaxMovesZero pins the resolved name decision:
// 0 is rejected outright — "no moves" is mode = "off", and many readers
// take 0 for unlimited — so the error points at the right disable.
func TestAccountHandoffRejectsMaxMovesZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "[account_handoff]\nmax_moves = 0\n")

	_, err := LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 1")
	assert.Contains(t, err.Error(), `mode = "off"`,
		"the rejection names the real disable path")

	_, err = SetGlobalConfigValue("account_handoff.max_moves", "0")
	require.Error(t, err, "the leaf writer applies the same rejection")
}

func TestAccountHandoffZeroDisablesIdleAndWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, `
[account_handoff]
move_idle_at_percent = 0
max_moves_window = "0"
probe_ttl = ""
`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.AccountHandoff.MoveIdleAtPercent, "0 disables proactive moves")
	assert.Equal(t, "0", cfg.AccountHandoff.MaxMovesWindow, "0 means no moves cap window")
	assert.Equal(t, "", cfg.AccountHandoff.ProbeTTL, "empty disables probes")
}

func TestAccountHandoffInRepoRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, "[account_handoff]\nmode = \"shadow\"\n")

	_, _, err := LoadInRepoConfig(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account_handoff")
	assert.Contains(t, err.Error(), "global setting",
		"identity-policy tables get the actionable global-only rejection, not a generic unknown-key error")
	assert.NotContains(t, err.Error(), "unknown key")
	assert.Contains(t, err.Error(), filepath.Join(home, TomlConfigFileName))
}

func TestAccountHandoffPersonalMergesPerLeaf(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, `
[account_handoff]
mode = "shadow"
wall_percent = 99
exclude_sessions = ["global-session"]
`)
	writePersonalConfig(t, project.ID, `
[account_handoff]
mode = "off"
prefer_below_percent = 70
`)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	got := resolved.AccountHandoff
	assert.Equal(t, "off", got.Mode, "the personal leaf wins over the global leaf")
	assert.Equal(t, 70, got.PreferBelowPercent, "a personal-only leaf applies")
	assert.Equal(t, 99, got.WallPercent, "the global leaf still wins where the personal file is silent")
	assert.Equal(t, []string{"global-session"}, got.ExcludeSessions,
		"a leaf the personal file never set inherits the global value — the table merges field-wise")

	value, ok := resolved.ResolvedValue("account_handoff")
	require.True(t, ok)
	assert.Equal(t, SourceProjectPersonal.String(), value.Origins["mode"].Layer)
	assert.Equal(t, SourceProjectPersonal.String(), value.Origins["prefer_below_percent"].Layer)
	assert.Equal(t, SourceGlobal.String(), value.Origins["wall_percent"].Layer)

	leaf, ok := resolved.ResolvedValuePath("account_handoff.mode")
	require.True(t, ok)
	assert.Equal(t, SourceProjectPersonal.String(), leaf.Winner.Layer)
	assert.Equal(t, "off", leaf.Value)
}

func TestAccountHandoffPersonalAbsentLeavesStayDefault(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[account_handoff]\nmode = \"shadow\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	got := resolved.AccountHandoff
	assert.Equal(t, "shadow", got.Mode)
	assert.Equal(t, defaultAccountHandoffConfig().WallPercent, got.WallPercent,
		"unwritten personal leaves inherit the compiled default, not a zero value")
}

func TestAccountHandoffPersonalRejectsInvalid(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[account_handoff]\nmode = \"live\"\n")

	_, err := LoadProjectConfig(project.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account_handoff")
}

func TestAccountHandoffPersonalUnknownLeafWarns(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[account_handoff]\nmode = \"shadow\"\nwall_percnt = 90\n")

	warnings := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err, "a typo'd leaf warns but does not fail the load — the #4599 contract")
	require.NotNil(t, cfg)
	assert.Contains(t, warnings.String(), "wall_percnt")
	assert.Contains(t, warnings.String(), "account_handoff")
	assert.NotEmpty(t, cfg.UnknownLeaves(), "af doctor reads the same leaf list the warning names")
}

func TestAccountHandoffGlobalUnknownLeafWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "[account_handoff]\nmode = \"shadow\"\nwall_percnt = 90\n")

	warnings := captureLog(t, &aflog.WarningLog)
	_, err := LoadConfig()
	require.NoError(t, err, "the global layer applies the same load-tolerant unknown-key contract")
	assert.Contains(t, warnings.String(), "account_handoff.wall_percnt",
		"the strict probe names the leaf, not just the table")
}

// TestAccountHandoffCurrentValuePresenceAware pins the editor render: an absent
// table shows {}, and a sparse table shows only the leaves the file wrote —
// not the compiled defaults for the other fourteen.
func TestAccountHandoffCurrentValuePresenceAware(t *testing.T) {
	t.Run("absent renders {}", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		writeGlobalTOML(t, home, "default_program = \"claude\"\n")

		cfg, err := LoadConfig()
		require.NoError(t, err)
		shown, ok := CurrentValue(cfg, "account_handoff")
		require.True(t, ok)
		assert.Equal(t, "{}", shown)
	})

	t.Run("sparse renders only written leaves", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		writeGlobalTOML(t, home, "[account_handoff]\nmode = \"shadow\"\nmove_idle_at_percent = 0\n")

		cfg, err := LoadConfig()
		require.NoError(t, err)
		shown, ok := CurrentValue(cfg, "account_handoff")
		require.True(t, ok)
		assert.Equal(t, `{"mode":"shadow","move_idle_at_percent":0}`, shown,
			"an explicit 0 is a written leaf, not an omitted one")
	})

	t.Run("personal sparse render matches", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "[account_handoff]\nmode = \"shadow\"\n")

		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		shown, ok := projectStructuredCurrentValue(cfg, "account_handoff")
		require.True(t, ok)
		assert.Equal(t, `{"mode":"shadow"}`, shown)
	})
}

func TestAccountHandoffConfigSetLeaf(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "default_program = \"claude\"\n")

	_, err := SetGlobalConfigValue("account_handoff.mode", "shadow")
	require.NoError(t, err)
	_, err = SetGlobalConfigValue("account_handoff.wall_percent", "98")
	require.NoError(t, err)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "shadow", cfg.AccountHandoff.Mode)
	assert.Equal(t, 98, cfg.AccountHandoff.WallPercent)
	assert.Equal(t, defaultAccountHandoffConfig().ProbeTTL, cfg.AccountHandoff.ProbeTTL)
}

func TestAccountHandoffConfigSetRejectsLiveAndBadLeaf(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "default_program = \"claude\"\n")

	_, err := SetGlobalConfigValue("account_handoff.mode", "live")
	require.Error(t, err, "the write path rejects mode = live with the same rule the loader applies")
	_, err = SetGlobalConfigValue("account_handoff.wall_percent", "30")
	require.Error(t, err)
	_, err = SetGlobalConfigValue("account_handoff.bogus", "1")
	require.Error(t, err, "an unknown leaf is not a settable key")
}

func TestAccountHandoffConfigSetWholeTablePatches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "[account_handoff]\nwall_percent = 98\nmode = \"off\"\n")

	_, err := SetGlobalConfigValue("account_handoff", `{"mode":"shadow","probe_ttl":"20m"}`)
	require.NoError(t, err)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "shadow", cfg.AccountHandoff.Mode)
	assert.Equal(t, "20m", cfg.AccountHandoff.ProbeTTL)
	assert.Equal(t, 98, cfg.AccountHandoff.WallPercent,
		"a whole-table write is a field-wise patch: omitted members keep their stored value")

	_, err = SetGlobalConfigValue("account_handoff", `{"mode":"live"}`)
	require.Error(t, err, "the JSON form applies the same live rejection")

	_, err = SetGlobalConfigValue("account_handoff", `{"prefer_below_percent":99}`)
	require.Error(t, err, "a write that leaves prefer_below >= move_idle is refused, not clamped")
}

func TestAccountHandoffProjectSetMergesPerLeaf(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "[account_handoff]\nmode = \"shadow\"\nwall_percent = 98\n")

	_, err := SetProjectConfigValue(project.ID, "account_handoff.target_order", "emptiest")
	require.NoError(t, err)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "emptiest", resolved.AccountHandoff.TargetOrder)
	assert.Equal(t, "shadow", resolved.AccountHandoff.Mode, "the global leaf still applies")
	assert.Equal(t, 98, resolved.AccountHandoff.WallPercent)

	_, err = SetProjectConfigValue(project.ID, "account_handoff.mode", "live")
	require.Error(t, err, "the per-project write path rejects live the same way")
}

// TestAccountHandoffNoTableNoChange pins the slice's core contract: config
// that never names [account_handoff] is exactly today's config — same resolved
// values, same winner — so shipping the table cannot change any install that
// has not opted in.
func TestAccountHandoffNoTableNoChange(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	writeGlobalTOML(t, home, "limit_auto_resume = true\nlimit_account_candidates = [\"work\"]\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, defaultAccountHandoffConfig(), resolved.AccountHandoff)

	value, ok := resolved.ResolvedValue("account_handoff")
	require.True(t, ok)
	// A field-wise merge has no single Winner — every leaf's origin is the
	// built-in default.
	assert.Equal(t, SourceBuiltIn.String(), value.Origins["mode"].Layer)
	for _, c := range value.Candidates {
		switch c.Layer {
		case SourceBuiltIn.String():
			assert.True(t, c.Present)
		default:
			assert.False(t, c.Present, "%s must report the absent table as absent", c.Layer)
		}
	}
}
