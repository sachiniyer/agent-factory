package config

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the fix for the silent-failure class of bug where the TOML
// loader (go-toml v2) folds table headers case-insensitively but the surgical
// writers located the table to replace/remove by an exact-byte header
// comparison. A hand-edited (or third-party-migrated) [HOSTS.box1] header
// decoded into the same Hosts entry as [hosts.box1], but `af hosts remove
// box1` and a whole-table `af config set hosts '{...}'` failed to find it,
// appended a fresh canonical [hosts] block, and left the stale [HOSTS.*]
// block on disk to silently merge with — or be unioned into — the new table
// on every later load. The writers now match headers case-insensitively, and
// the rewrite guard's whole-map exemption is name-aware so a surviving ghost
// fails the write instead of passing it.

// --- writer unit tests ---

func TestRemoveTOMLTopLevelValueFindsCaseVariantHeader(t *testing.T) {
	for _, header := range []string{"HOSTS", "Hosts", "hosts"} {
		t.Run(header, func(t *testing.T) {
			content := "keep = 'yes'\n\n[" + header + ".box1]\nhost = 'box1.tailnet.ts.net'\nuser = 'dev'\n"
			got, err := removeTOMLTopLevelValue(content, "hosts")
			require.NoError(t, err)
			assert.NotContains(t, got, "HOSTS", "the case-variant header must be removed")
			assert.NotContains(t, got, "Hosts")
			assert.NotContains(t, got, "box1.tailnet", "and so must the entry the loader merged in")
			assert.Contains(t, got, "keep = 'yes'", "an unrelated root key is untouched")
		})
	}
}

func TestRemoveTOMLTopLevelValueRemovesCaseVariantRootScalarList(t *testing.T) {
	// A root-level value spelling (session_env_passthrough = [...]) is also
	// matched by parts[0] == key, so the same case-insensitive fix removes a
	// SESSION_ENV_PASSTHROUGH = [...] root value too.
	content := "SESSION_ENV_PASSTHROUGH = ['OLD']\nkeep = 'yes'\n[NETWORK]\nlisten_addr = '127.0.0.1:8443'\n"
	got, err := removeTOMLTopLevelValue(content, "session_env_passthrough")
	require.NoError(t, err)
	assert.NotContains(t, got, "OLD")
	assert.NotContains(t, got, "SESSION_ENV_PASSTHROUGH")
	assert.Contains(t, got, "keep = 'yes'")
	assert.Contains(t, got, "[NETWORK]", "a different table is not touched")
	assert.Contains(t, got, "listen_addr = '127.0.0.1:8443'")
}

func TestSetTOMLStructuredReplacesCaseVariantHostsTable(t *testing.T) {
	content := "keep = 'yes'\n\n[HOSTS.box1]\nhost = 'box1.tailnet.ts.net'\nuser = 'dev'\n"
	got, err := setTOMLStructured(content, "hosts", "[hosts]\nbox2 = { host = 'box2.tailnet.ts.net' }\n")
	require.NoError(t, err)
	assert.NotContains(t, got, "HOSTS", "the stale case-variant block is removed, not merged")
	assert.NotContains(t, got, "box1.tailnet")
	assert.Contains(t, got, "[hosts]")
	assert.Contains(t, got, "box2.tailnet.ts.net")
	assert.Contains(t, got, "keep = 'yes'", "an unrelated ROOT key (before the table) survives the surgical rewrite")
	var parsed map[string]any
	require.NoError(t, toml.Unmarshal([]byte(got), &parsed))
	hosts, _ := parsed["hosts"].(map[string]any)
	assert.Contains(t, hosts, "box2")
	assert.NotContains(t, hosts, "box1", "no ghost on the decoded result either")
}

// preserveStructuredMembers carries unknown members of the OLD target table
// into its replacement. It locates the old table by decoding the file into a
// generic map, whose keys go-toml v2 preserves EXACTLY as the header spells
// them (only struct-FIELD resolution folds), so a [ROOT_AGENT] header sits under
// "ROOT_AGENT" not "root_agent": the preserve step must look it up
// case-insensitively or drop the unknown member.
func TestPreserveStructuredMembersRetainsUnknownMemberUnderCaseVariantHeader(t *testing.T) {
	content := "[ROOT_AGENT]\nenabled = true\nfuture_policy = 'keep'\n"
	definition := "[root_agent]\nenabled = false\n"
	got, err := preserveStructuredMembers(content, "root_agent", definition)
	require.NoError(t, err)
	assert.Contains(t, got, "future_policy = 'keep'", "the case-variant table's unknown member is preserved")
	assert.Contains(t, got, "enabled = false")

	// Control: a canonical lowercase header must preserve identically, so the
	// fix does not regress the existing path.
	canonical, err := preserveStructuredMembers(
		"[root_agent]\nenabled = true\nfuture_policy = 'keep'\n", "root_agent", definition)
	require.NoError(t, err)
	assert.Equal(t, canonical, got, "case-variant and canonical headers preserve identically")
}

// --- scalar leaf writer (setTOMLScalar / deleteTOMLScalar) ---

func TestSetTOMLScalarUpdatesLeafUnderCaseVariantSection(t *testing.T) {
	for _, header := range []string{"WEB", "Web", "web"} {
		t.Run(header, func(t *testing.T) {
			content := "[" + header + "]\nterminal_font_size_phone = 6.5\n"
			got := setTOMLScalar(content, "web", "terminal_font_size_phone", "8.0")
			assert.Contains(t, got, "terminal_font_size_phone = 8.0", "the leaf value is updated in place")
			assert.Equal(t, 1, strings.Count(got, "terminal_font_size_phone ="),
				"no duplicate leaf line is appended")
			assert.Equal(t, 1, strings.Count(got, "["),
				"no duplicate [web] block is appended next to the case-variant header")
		})
	}
}

func TestDeleteTOMLScalarRemovesLeafUnderCaseVariantSection(t *testing.T) {
	content := "[WEB]\nterminal_font_size_phone = 6.5\nterminal_font_size_desktop = 12.0\n"
	got, removed := deleteTOMLScalar(content, "web", "terminal_font_size_phone")
	require.True(t, removed)
	assert.NotContains(t, got, "terminal_font_size_phone = 6.5")
	assert.Contains(t, got, "[WEB]", "the case-variant header stays (surgical: only the leaf moves)")
	assert.Contains(t, got, "terminal_font_size_desktop = 12.0", "a sibling leaf is untouched")
}

// --- end-to-end through the real global config writers ---

func TestEditGlobalHostsRemovesCaseVariantHostEntry(t *testing.T) {
	path := writeTempConfig(t, "[HOSTS.box1]\nhost = 'box1.tailnet.ts.net'\nuser = 'dev'\n")
	before, err := LoadConfig()
	require.NoError(t, err)
	assert.Contains(t, before.Hosts, "box1", "the loader folds [HOSTS.box1] into Hosts")

	_, _, err = EditGlobalHosts(HostsEdit{Op: HostsEditRemove, Name: "box1"})
	require.NoError(t, err, "a case-variant header must not make `af hosts remove` silently no-op")

	after, err := LoadConfig()
	require.NoError(t, err)
	assert.Empty(t, after.Hosts, "the compromised host is gone from every later load")
	assert.NotContains(t, after.Hosts, "box1")

	onDisk := readFile(t, path)
	assert.NotContains(t, onDisk, "HOSTS", "the stale block is not left on disk")
	assert.NotContains(t, onDisk, "Hosts")
	assert.NotContains(t, onDisk, "box1.tailnet", "the stale entry is not left on disk either")
}

func TestSetGlobalConfigValueHostsReplacesCaseVariantTable(t *testing.T) {
	path := writeTempConfig(t, "[HOSTS.box1]\nhost = 'box1.tailnet.ts.net'\nuser = 'dev'\n")

	_, err := SetGlobalConfigValue("hosts", `{"box2":{"host":"box2.tailnet.ts.net","user":"dev"}}`)
	require.NoError(t, err, "a case-variant header must not turn a replace into a silent merge")

	after, err := LoadConfig()
	require.NoError(t, err)
	assert.Contains(t, after.Hosts, "box2")
	assert.NotContains(t, after.Hosts, "box1", "the replaced-away host does not come back as a ghost")

	onDisk := readFile(t, path)
	assert.NotContains(t, onDisk, "HOSTS", "the stale [HOSTS.*] block is removed, not unioned")
	assert.NotContains(t, onDisk, "box1.tailnet")
}

// A whole-table replace that KEEPS an entry already present (same value) must
// still pass the tightened, name-aware guard: a correct write's post-load value
// equals the intended payload, so nothing false-fires.
func TestSetGlobalConfigValueHostsKeepsExistingEntryUnderCaseVariantHeader(t *testing.T) {
	writeTempConfig(t, "[HOSTS.box1]\nhost = 'box1.tailnet.ts.net'\nuser = 'dev'\n")
	_, err := SetGlobalConfigValue("hosts",
		`{"box1":{"host":"box1.tailnet.ts.net","user":"dev"},"box2":{"host":"box2.tailnet.ts.net"}}`)
	require.NoError(t, err)
	after, err := LoadConfig()
	require.NoError(t, err)
	assert.Contains(t, after.Hosts, "box1")
	assert.Contains(t, after.Hosts, "box2")
}

func TestSetGlobalConfigValueWebLeafEditsCaseVariantWebSection(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 1\n[WEB]\nterminal_font_size_phone = 6.5\n")
	_, err := SetGlobalConfigValue("web.terminal_font_size_phone", "8")
	require.NoError(t, err, "a leaf edit under a case-variant [WEB] header must not append a duplicate")

	after, err := LoadConfig()
	require.NoError(t, err)
	assert.InDelta(t, 8.0, after.Web.TerminalFontSizePhone, 0.0001)

	onDisk := readFile(t, path)
	assert.Equal(t, 1, strings.Count(onDisk, "terminal_font_size_phone ="),
		"the leaf is edited in place, not duplicated")
	assert.NotContains(t, onDisk, "[web]\n[WEB]", "no canonical duplicate block is appended")
}

// Control: the canonical lowercase spelling the whole suite already exercised
// continues to behave exactly as before.
func TestSetGlobalConfigValueHostsLowercaseHeaderRegression(t *testing.T) {
	path := writeTempConfig(t, "[hosts.box1]\nhost = 'box1.tailnet.ts.net'\nuser = 'dev'\n")
	_, err := SetGlobalConfigValue("hosts", `{"box2":{"host":"box2.tailnet.ts.net"}}`)
	require.NoError(t, err)
	after, err := LoadConfig()
	require.NoError(t, err)
	assert.NotContains(t, after.Hosts, "box1")
	assert.Contains(t, after.Hosts, "box2")
	assert.NotContains(t, readFile(t, path), "box1.tailnet")
}

// --- project path: the personal-project loader rejects case-variant headers
// at load (its top-level-key allowlist is checked case-sensitively), so the
// case-variant ghost the global writers/guard are hardened against cannot
// arise here. Pin that boundary so a regression that opens it is caught. ---

func TestProjectConfigRejectsCaseVariantTableHeader(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[PROGRAM_OVERRIDES]\nclaude = 'old'\n")
	_, err := LoadProjectConfig(project.ID)
	require.Error(t, err, "the project loader admits only canonical-cased table headers")
	assert.Contains(t, err.Error(), "PROGRAM_OVERRIDES")
}

// --- rewrite guard: name-aware whole-map exemption ---

func TestConfigRewriteDriftForCatchesGhostHostEntry(t *testing.T) {
	before := DefaultConfig()
	before.Hosts = map[string]HostConfig{"box1": {Host: "box1.tailnet.ts.net"}}
	after := DefaultConfig()
	// The writer appended [hosts]box2 but failed to remove [HOSTS.box1]: the
	// loader folded both into after.Hosts — box2 is the intended new entry,
	// box1 is the ghost that survived the replace.
	after.Hosts = map[string]HostConfig{
		"box1": {Host: "box1.tailnet.ts.net"},
		"box2": {Host: "box2.tailnet.ts.net"},
	}
	intended := DefaultConfig()
	intended.Hosts = map[string]HostConfig{"box2": {Host: "box2.tailnet.ts.net"}}

	drift := configRewriteDriftFor(before, after, intended, "hosts")
	assert.Contains(t, drift, "hosts", "the ghost's survival is named as drift, not masked")

	// Correct write: after == intended, nothing fires.
	correct := DefaultConfig()
	correct.Hosts = map[string]HostConfig{"box2": {Host: "box2.tailnet.ts.net"}}
	assert.Empty(t, configRewriteDriftFor(before, correct, intended, "hosts"),
		"a whole-table replace whose post-load value matches the payload is clean")

	// Original whole-map exemption (no intended value) masked the ghost: this
	// documents the blind spot the fix closes.
	assert.Empty(t, configRewriteDrift(before, after, "hosts"),
		"without an intended payload the guard copies the ghost in and passes — the bug")
}

func TestConfigRewriteDriftForIgnoresIntendedForLoadPrunedMaps(t *testing.T) {
	// keys (map[string]any) is pruned by the loader from values the writer
	// accepted (removed-PR/dead-shift bindings), so a correct write's post-load
	// value can legitimately be a subset of the payload. The guard must ignore
	// the intended value for this field and compare against `after`, or every
	// correct write would false-fire.
	before := DefaultConfig()
	before.Keys = map[string]any{"quit": "Q"}
	after := DefaultConfig()
	after.Keys = map[string]any{"quit": "Q"}
	intended := DefaultConfig()
	intended.Keys = map[string]any{"quit": "Q", "phantom": "shift+a"}
	assert.Empty(t, configRewriteDriftFor(before, after, intended, "keys"),
		"keys is gated out so the intended payload does not false-fire on a load-pruned subset")

	// root_agents is pruned by registration, so it is gated out by name.
	rootBefore := DefaultConfig()
	rootBefore.RootAgents = map[string]RootAgentConfig{"/repo": {Program: "claude"}}
	rootAfter := snapshotConfig(rootBefore) // a correct write that does not touch root_agents
	rootIntended := DefaultConfig()
	rootIntended.RootAgents = map[string]RootAgentConfig{"/other": {Program: "codex"}}
	assert.Empty(t, configRewriteDriftFor(rootBefore, rootAfter, rootIntended, "root_agents"),
		"root_agents is gated out so registration pruning cannot false-fire")
}

func TestConfigRewriteDriftForIntendedDoesNotReachScalarOrDottedKeys(t *testing.T) {
	// A scalar top-level field (default_host) and a single dynamic-map entry
	// (program_overrides.claude) keep the original exemption regardless of the
	// intended value: the whole-map fix is map-gated, and the intended payload
	// is only meaningful for a whole-table write.
	before := DefaultConfig()
	before.ProgramOverrides = map[string]string{"claude": "/bin/claude", "codex": "/bin/codex"}
	after := snapshotConfig(before)
	after.ProgramOverrides["claude"] = "/opt/claude"

	intended := snapshotConfig(before)
	intended.DefaultHost = "phantom"
	assert.Empty(t, configRewriteDriftFor(before, after, intended, "program_overrides.claude"),
		"a single dynamic-map entry is exempted from after, and intended is not consulted")

	// A scalar field compares after-to-after via the original exemption; an
	// intended value for it must not move the expectation.
	scalarBefore := DefaultConfig()
	scalarBefore.DefaultHost = "box1"
	scalarAfter := snapshotConfig(scalarBefore) // unchanged
	scalarIntended := snapshotConfig(scalarBefore)
	scalarIntended.DefaultHost = "ghost"
	assert.Empty(t, configRewriteDriftFor(scalarBefore, scalarAfter, scalarIntended, "branch_prefix"),
		"a scalar key's exemption does not read the intended value")
}
