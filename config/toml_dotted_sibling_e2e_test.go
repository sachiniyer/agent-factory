package config

import (
	"os"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests guard the dotted-sibling insert defect at setTOMLScalar's CALL
// SITE (config/configset.go). When a root table is opened by a SIBLING
// top-level dotted key (program_overrides.codex = …) but the target leaf has
// neither a dotted key nor a [section] header, the surgical editor's insert
// branch appends a fresh [section] block over the dotted table, which TOML
// forbids ("table program_overrides already exists as defined by a dotted
// key"). The pre-write parse gate refused those bytes, so a
// documented-supported edit (af config set program_overrides.claude … on a
// hand-edited dotted-key file) was blocked with an opaque "internal error".
//
// The fix mirrors the existing migrate.go guard at the scalar-set call sites
// (scalarWrite.apply and scalarWrite.applyProject): when the section is
// dotted-opened, the new leaf joins the table in the same dotted form. The
// shared helper tomlRootDottedTable already exists; the fix adds no new
// machinery. These tests exercise the PUBLIC write paths (where the
// user-visible defect lived) and assert the on-disk bytes load and keep the
// dotted form.

// loadsTOML fails the test unless content parses as valid TOML. A valid edit
// must always leave a file that loads — the on-disk safety contract — so each
// test finishes by checking this.
func loadsTOML(t *testing.T, content string) {
	t.Helper()
	var m map[string]any
	if err := toml.Unmarshal([]byte(content), &m); err != nil {
		t.Fatalf("edited config would not load: %v\n%s", err, content)
	}
}

// noHeader asserts content does not re-open a dotted-opened table with a
// [section] header — the whole of the defect.
func noHeader(t *testing.T, content, section string) {
	t.Helper()
	if strings.Contains(content, "["+section+"]") {
		t.Fatalf("must not append a [%s] header over a dotted-key table:\n%s", section, content)
	}
}

// TestDottedSiblingSetGlobalAddsNewLeaf is the core end-to-end reproduction of
// the reported bug, now fixed: a config.toml that hand-edits program_overrides
// as a dotted-key table (the form docs/configuration.md tells operators to
// paste) and then receives a NEW sibling leaf via the CLI dotted single-entry
// form must succeed, stay in dotted form, and load.
func TestDottedSiblingSetGlobalAddsNewLeaf(t *testing.T) {
	seed := "default_program = 'claude'\nprogram_overrides.codex = 'codex --model gpt-5'\n"
	path := writeTempConfig(t, seed)

	_, err := SetGlobalConfigValue("program_overrides.claude", "/bin/claude")
	require.NoError(t, err, "a documented-supported edit on a valid dotted-key config must not be rejected")

	got, _ := os.ReadFile(path)
	content := string(got)
	loadsTOML(t, content)
	noHeader(t, content, "program_overrides")
	assert.Contains(t, content, "program_overrides.codex = 'codex --model gpt-5'",
		"the existing sibling dotted key is preserved byte-for-byte")
	assert.Contains(t, content, "program_overrides.claude = '/bin/claude'",
		"the new leaf joins the table in dotted form")
	assert.True(t, strings.Contains(content, "program_overrides.codex = 'codex --model gpt-5'\nprogram_overrides.claude"),
		"the new dotted key lands adjacent to its sibling, not after a [section] block:\n%s", content)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex --model gpt-5", cfg.ProgramOverrides["codex"], "sibling value survives")
	assert.Equal(t, "/bin/claude", cfg.ProgramOverrides["claude"], "new value is effective")
}

// TestDottedSiblingSetGlobalHeaderFormStillWorks guards the common path the
// fix must NOT change: a section declared by a [section] header with a missing
// leaf (the form af's own emitted defaults take). tomlRootDottedTable is false,
// so the new leaf is appended under the header, not rewritten to a dotted key.
// This catches a guard that is too aggressive — one that rewrites header form
// to dotted form.
func TestDottedSiblingSetGlobalHeaderFormStillWorks(t *testing.T) {
	seed := "default_program = 'claude'\n\n[program_overrides]\ncodex = 'codex'\n"
	path := writeTempConfig(t, seed)

	_, err := SetGlobalConfigValue("program_overrides.claude", "/bin/claude")
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	content := string(got)
	loadsTOML(t, content)
	assert.Contains(t, content, "[program_overrides]", "header form is kept, not rewritten to dotted")
	assert.Contains(t, content, "claude = '/bin/claude'", "the new leaf lands under the header")
	assert.NotContains(t, content, "program_overrides.claude",
		"header form must not be rewritten to dotted form")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "/bin/claude", cfg.ProgramOverrides["claude"])
}

// TestDottedSiblingSetGlobalRootKeyUnaffected guards the fix's section != ""
// gate: a root scalar key (section == "") is never rerouted through the
// dotted-table branch, even when the config also contains an unrelated
// dotted-key table. Without the gate the guard would emit an invalid dotted
// key (".leaf") instead of an ordinary root assignment.
func TestDottedSiblingSetGlobalRootKeyUnaffected(t *testing.T) {
	seed := "program_overrides.codex = 'codex'\ndefault_program = 'claude'\n"
	path := writeTempConfig(t, seed)

	_, err := SetGlobalConfigValue("default_program", "gemini")
	require.NoError(t, err)

	got, _ := os.ReadFile(path)
	content := string(got)
	loadsTOML(t, content)
	assert.Contains(t, content, "default_program = 'gemini'")
	assert.Contains(t, content, "program_overrides.codex = 'codex'", "untouched dotted table survives")
	noHeader(t, content, "program_overrides")
}

// TestDottedSiblingProjectPathE2E is the end-to-end project-path reproduction:
// a registered project's personal config.toml is hand-edited to the dotted
// form, then SetProjectConfigValue (the public API `af config set --project`
// reaches) adds a new leaf. Before the fix the project-path parse gate refused
// the dotted-key redeclaration; after, the leaf joins in dotted form. This pins
// the applyProject call site, the second location of the guard.
func TestDottedSiblingProjectPathE2E(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "program_overrides.codex = 'codex --model gpt-5'\n")

	_, err := SetProjectConfigValue(project.ID, "program_overrides.claude", "/bin/claude")
	require.NoError(t, err)

	cfgPath, _ := ProjectConfigTomlPath(project.ID)
	got, _ := os.ReadFile(cfgPath)
	content := string(got)
	loadsTOML(t, content)
	noHeader(t, content, "program_overrides")
	assert.Contains(t, content, "program_overrides.codex = 'codex --model gpt-5'")
	assert.Contains(t, content, "program_overrides.claude = '/bin/claude'")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "codex --model gpt-5", resolved.ProgramOverrides["codex"])
	assert.Equal(t, "/bin/claude", resolved.ProgramOverrides["claude"])
}

// TestInsertTOMLDottedLeafDoesNotOverwriteQuotedRootKey pins the distinction
// between a two-component dotted path (program_overrides.claude = …) and a
// root-level quoted key whose literal name contains a dot
// ("program_overrides.claude" = …). TOML treats these as different keys and
// both can coexist. The old rerouting path called
// setTOMLScalar("", "program_overrides.claude", …), whose regex-based keyRe
// matched the quoted key's line and overwrote it. insertTOMLDottedLeaf bypasses
// that regex and locates siblings via tomlAssignmentPath (TOML-aware), so the
// quoted root key survives and the new dotted entry is added beside the sibling.
func TestInsertTOMLDottedLeafDoesNotOverwriteQuotedRootKey(t *testing.T) {
	// A config that has BOTH a dotted sibling (opens the program_overrides table)
	// and an unrelated quoted root key whose name happens to contain a dot.
	// Both are syntactically and semantically distinct in TOML.
	input := "program_overrides.codex = 'codex'\n\"program_overrides.claude\" = 'unrelated'\n"
	got := insertTOMLDottedLeaf(input, "program_overrides", "claude", "'/bin/claude'")

	// The quoted root key must be left untouched.
	assert.Contains(t, got, `"program_overrides.claude" = 'unrelated'`,
		"quoted root key must not be overwritten")
	// The new dotted entry must be present.
	assert.Contains(t, got, "program_overrides.claude = '/bin/claude'",
		"new dotted leaf must be inserted")
	// The existing sibling must be preserved.
	assert.Contains(t, got, "program_overrides.codex = 'codex'",
		"existing dotted sibling must be preserved")
	// The result must be valid TOML.
	loadsTOML(t, got)
}

// TestDottedSiblingProjectPathHeaderFormStillWorks is the project-path
// counterpart of TestDottedSiblingSetGlobalHeaderFormStillWorks: a header-form
// personal config keeps the [section] header. The guard only reroutes a
// dotted-opened table, so applyProject must not rewrite header form to dotted.
func TestDottedSiblingProjectPathHeaderFormStillWorks(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[program_overrides]\ncodex = 'codex'\n")

	_, err := SetProjectConfigValue(project.ID, "program_overrides.claude", "/bin/claude")
	require.NoError(t, err)

	cfgPath, _ := ProjectConfigTomlPath(project.ID)
	got, _ := os.ReadFile(cfgPath)
	content := string(got)
	loadsTOML(t, content)
	assert.Contains(t, content, "[program_overrides]")
	assert.Contains(t, content, "claude = '/bin/claude'")
	assert.NotContains(t, content, "program_overrides.claude")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "/bin/claude", resolved.ProgramOverrides["claude"])
}
