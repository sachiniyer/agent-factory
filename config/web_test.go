package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWebTerminalFontSizeDefaults pins the compiled-in sizes — the same pair
// the web client falls back to — so a default change lands deliberately.
func TestWebTerminalFontSizeDefaults(t *testing.T) {
	cfg := DefaultConfig()
	require.Equal(t, 6.5, cfg.Web.TerminalFontSizePhone)
	require.Equal(t, 13.0, cfg.Web.TerminalFontSizeDesktop)

	writeTempConfig(t, "default_program = 'claude'\n")
	loaded, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 6.5, loaded.Web.TerminalFontSizePhone,
		"an absent [web] table must behave as the defaults")
	require.Equal(t, 13.0, loaded.Web.TerminalFontSizeDesktop)
}

// TestSetGlobalWebFontLeafWritesTomlFloat is the write the terminal's A−/A+
// controls issue: the leaf must land as a real TOML float — "8.0", not "8" —
// because an integer token would refuse to decode into the float64 field at
// load and break the file for everything else.
func TestSetGlobalWebFontLeafWritesTomlFloat(t *testing.T) {
	path := writeTempConfig(t, "# keep me\n[web]\nterminal_font_size_phone = 6.5\n")

	result, err := SetGlobalConfigValue("web.terminal_font_size_phone", "8")
	require.NoError(t, err)
	// The echo is the short canonical form `af config get` renders — "8" —
	// while the FILE spelling carries the decimal (asserted below).
	require.Equal(t, "8", result.Value)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(written), "terminal_font_size_phone = 8.0",
		"the canonical TOML form carries the decimal so it stays a float")
	require.Contains(t, string(written), "# keep me")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 8.0, cfg.Web.TerminalFontSizePhone)
	require.Equal(t, 13.0, cfg.Web.TerminalFontSizeDesktop,
		"a phone write must not disturb the desktop leaf")
}

func TestSetGlobalWebFontLeafRejectsNonNumber(t *testing.T) {
	path := writeTempConfig(t, "[web]\nterminal_font_size_phone = 6.5\n")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, err = SetGlobalConfigValue("web.terminal_font_size_desktop", "big")
	require.ErrorContains(t, err, "expected a number")
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, string(before), string(after), "a rejected write must leave the file untouched")
}

func TestSetGlobalWebFontLeafRejectsOutOfRange(t *testing.T) {
	path := writeTempConfig(t, "[web]\nterminal_font_size_phone = 6.5\n")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	for _, value := range []string{"2", "40", "nan", "inf"} {
		_, err := SetGlobalConfigValue("web.terminal_font_size_phone", value)
		require.Error(t, err, "value %q", value)
	}
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, string(before), string(after))
}

// TestSetGlobalWebWholeTableValidatesEveryLeaf covers the compact-JSON form the
// config panes post: one bad member refuses the whole write, under that
// member's own key name.
func TestSetGlobalWebWholeTableValidatesEveryLeaf(t *testing.T) {
	writeTempConfig(t, "[web]\nterminal_font_size_phone = 6.5\n")

	_, err := SetGlobalConfigValue("web", `{"terminal_font_size_phone":8,"terminal_font_size_desktop":14}`)
	require.NoError(t, err)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 8.0, cfg.Web.TerminalFontSizePhone)
	require.Equal(t, 14.0, cfg.Web.TerminalFontSizeDesktop)

	_, err = SetGlobalConfigValue("web", `{"terminal_font_size_phone":8,"terminal_font_size_desktop":99}`)
	require.ErrorContains(t, err, "web.terminal_font_size_desktop")
	loaded, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 14.0, loaded.Web.TerminalFontSizeDesktop,
		"a refused whole-table write must not have half-applied")
}

// TestSetGlobalWebWholeTableDefaultsOmittedLeaves covers the documented
// whole-table form written with a single leaf: the omitted leaf must take the
// same compiled default the loader gives an absent [web] key — not the zero
// value the validation would refuse. Both omission directions are pinned,
// and an explicitly-present invalid value still fails exactly as the leaf
// form would.
func TestSetGlobalWebWholeTableDefaultsOmittedLeaves(t *testing.T) {
	path := writeTempConfig(t, "default_program = 'claude'\n")

	_, err := SetGlobalConfigValue("web", `{"terminal_font_size_phone":8}`)
	require.NoError(t, err,
		"an omitted leaf must default — the loader treats an absent key the same way")
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(written), "terminal_font_size_phone = 8")
	require.Contains(t, string(written), "terminal_font_size_desktop = 13",
		"the omitted desktop leaf lands as its compiled default")
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 8.0, cfg.Web.TerminalFontSizePhone)
	require.Equal(t, 13.0, cfg.Web.TerminalFontSizeDesktop)

	_, err = SetGlobalConfigValue("web", `{"terminal_font_size_desktop":14}`)
	require.NoError(t, err)
	cfg, err = LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 6.5, cfg.Web.TerminalFontSizePhone,
		"omitting phone must not carry over the earlier 8 — each write decodes onto defaults")
	require.Equal(t, 14.0, cfg.Web.TerminalFontSizeDesktop)

	// A present-but-invalid member still fails strictly — seeding defaults is
	// not leniency toward what was actually written.
	_, err = SetGlobalConfigValue("web", `{"terminal_font_size_phone":0}`)
	require.Error(t, err)
	_, err = SetGlobalConfigValue("web", `{"terminal_font_size_desktop":null}`)
	require.Error(t, err)
	loaded, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 14.0, loaded.Web.TerminalFontSizeDesktop,
		"the refused writes must not have half-applied")
}

// TestWebFontSizeLoadSanitizesHandEdits exercises the lenient half of the
// hand-edit-lenient / typed-strict asymmetry: a value the validator would
// refuse is still loaded, warned about, and dropped back to the class default
// rather than breaking the whole config.
func TestWebFontSizeLoadSanitizesHandEdits(t *testing.T) {
	for name, content := range map[string]string{
		"out of range": "[web]\nterminal_font_size_phone = 99\nterminal_font_size_desktop = 14\n",
		"non-finite":   "[web]\nterminal_font_size_phone = nan\nterminal_font_size_desktop = 14\n",
	} {
		t.Run(name, func(t *testing.T) {
			writeTempConfig(t, content)
			cfg, err := LoadConfig()
			require.NoError(t, err)
			require.Equal(t, 6.5, cfg.Web.TerminalFontSizePhone)
			require.Equal(t, 14.0, cfg.Web.TerminalFontSizeDesktop,
				"the sibling leaf must survive another leaf's sanitization")
		})
	}
}

func TestWebSettableFormsAreDiscoverable(t *testing.T) {
	keys := strings.Join(SettableKeys(), ",")
	require.Contains(t, keys, "web")
	require.Contains(t, keys, "web.terminal_font_size_phone")
	require.Contains(t, keys, "web.terminal_font_size_desktop")
}

func TestWebUnsettableSubkeyNamesWholeTableForm(t *testing.T) {
	writeTempConfig(t, "default_program = 'claude'\n")
	_, err := SetGlobalConfigValue("web.unknown", "7")
	require.Error(t, err)
	require.Contains(t, err.Error(), "whole table")
	require.Contains(t, err.Error(), "af config set web")
}

// TestWebLeafGetProjectsWithPerLeafOrigins pins the dotted read the PR adds:
// `af config get web.terminal_font_size_phone` resolves through
// ResolvedValuePath, and the provenance walk is scoped to web so a leaf the
// global document never set still reports built-in provenance rather than
// borrowing the whole-table winner.
func TestWebLeafGetProjectsWithPerLeafOrigins(t *testing.T) {
	repoRoot := setupProvenanceTest(t, `
schema_version = 1

[web]
terminal_font_size_phone = 8
`)
	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)

	phone, ok := resolved.ResolvedValuePath("web.terminal_font_size_phone")
	require.True(t, ok, "a configured leaf must project")
	require.Equal(t, 8.0, phone.Value)
	require.NotNil(t, phone.Winner)
	require.Equal(t, SourceGlobal.String(), phone.Winner.Layer)

	desktop, ok := resolved.ResolvedValuePath("web.terminal_font_size_desktop")
	require.True(t, ok, "an unconfigured leaf must project from the built-in")
	require.Equal(t, 13.0, desktop.Value)
	require.NotNil(t, desktop.Winner)
	require.Equal(t, SourceBuiltIn.String(), desktop.Winner.Layer)

	_, ok = resolved.ResolvedValuePath("web.bogus")
	require.False(t, ok, "a leaf outside the schema must not project")
}

// TestRootAgentLeafGetUnchangedByWebScope proves the per-leaf origin walk the
// web table added under resolveReplace did not rewire root_agent: its leaves
// still project through the composite merge they always used.
func TestRootAgentLeafGetUnchangedByWebScope(t *testing.T) {
	repoRoot := setupProvenanceTest(t, `
schema_version = 1

[root_agent]
enabled = true
program = "claude"
`)
	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)

	enabled, ok := resolved.ResolvedValuePath("root_agent.enabled")
	require.True(t, ok, "root_agent.enabled must still project")
	require.Equal(t, true, enabled.Value)
	require.NotNil(t, enabled.Winner)
	require.Equal(t, SourceGlobal.String(), enabled.Winner.Layer)

	program, ok := resolved.ResolvedValuePath("root_agent.program")
	require.True(t, ok)
	require.Equal(t, "claude", program.Value)
}

// TestWebFontSizeSettableOnlyGlobally pins the global-only rule: the personal
// project layer must refuse the key rather than store a display preference a
// cloned repository could impose on the operator.
func TestWebFontSizeSettableOnlyGlobally(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	for _, attempt := range []struct {
		name string
		run  func() error
	}{
		{"leaf", func() error {
			_, err := SetProjectConfigValue(repoRoot, "web.terminal_font_size_phone", "8")
			return err
		}},
		{"table", func() error {
			_, err := SetProjectConfigValue(repoRoot, "web", `{"terminal_font_size_phone":8}`)
			return err
		}},
		{"unset", func() error {
			_, err := UnsetProjectConfigValue(project.ID, "web.terminal_font_size_phone")
			return err
		}},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			require.Error(t, attempt.run())
		})
	}
}
