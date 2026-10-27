package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the trailing-comma regression suite for the inline-table
// surgical editor (setTOMLInlineTableMember / deleteTOMLInlineTableMember in
// toml_surgical.go).
//
// go-toml/v2 (the parser this project pins in go.mod) accepts a trailing comma
// in an inline table (`section = { a = 1, }`) even though the TOML 1.0 spec
// forbids it, so a config.toml a user hand-edits that way still loads and
// runs. Before the fix, inserting a NEW member into such a table reused the
// existing comma's slot but then added another ", " — producing ",,", which
// the write gate's re-parse rejected with `unexpected comma in inline table`
// and blocked the edit. Deleting the only member likewise stranded the comma
// (`ssh = { , }`). These tests prove both paths now emit valid TOML from input
// the loader itself accepts, without changing the byte output of the
// comma-free path the existing suites already pin.

// hasNoDoubleComma guards the whole defect: an inserted member must never sit
// immediately after an existing trailing comma, which is what ",," records.
// loadsTOML already catches this via the parser, but the specific message
// points a future debugger straight at the cause rather than a generic parse
// error.
func hasNoDoubleComma(t *testing.T, content string) {
	t.Helper()
	if strings.Contains(content, ",,") {
		t.Fatalf("edited config contains a double comma (,,):\n%s", content)
	}
}

// --- Insert path: inserting a NEW member into a trailing-comma inline table ---

// TestSetTOMLInlineTableInsertPreservesTrailingCommaSeparator is the direct
// reproduction of the reported insert defect, now fixed. A trailing-comma
// inline table that the loader accepts must accept a new member without
// emitting ",,".
func TestSetTOMLInlineTableInsertPreservesTrailingCommaSeparator(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		section  string
		leaf     string
		encoded  string
		want     string
		keepFrag string
	}{
		{
			name:    "insert after a single trailing-comma member",
			line:    "ssh = { future = \"keep\", }\n",
			section: "ssh", leaf: "host_key_verification", encoded: "'accept-new'",
			want:     "ssh = { future = \"keep\", host_key_verification = 'accept-new' }\n",
			keepFrag: `future = "keep"`,
		},
		{
			name:    "insert after several trailing-comma members",
			line:    "ssh = { a = 1, b = 2, }\n",
			section: "ssh", leaf: "host_key_verification", encoded: "'strict'",
			want:     "ssh = { a = 1, b = 2, host_key_verification = 'strict' }\n",
			keepFrag: "b = 2",
		},
		{
			name:    "insert when trailing comma has no following space",
			line:    "ssh = { future = \"keep\",}\n",
			section: "ssh", leaf: "host_key_verification", encoded: "'strict'",
			want:     "ssh = { future = \"keep\", host_key_verification = 'strict'}\n",
			keepFrag: `future = "keep"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := setTOMLScalar(tc.line, tc.section, tc.leaf, tc.encoded)
			assert.Equal(t, tc.want, got, "exact bytes for the trailing-comma insert")
			hasNoDoubleComma(t, got)
			loadsTOML(t, got)
			assert.Contains(t, got, tc.keepFrag, "existing member is preserved")
		})
	}
}

// TestSetTOMLInlineTableInsertCommaFreeIsByteIdentical guards the fix against a
// change to the common, comma-free path. The separator must stay ", " when the
// body does not end in a comma, matching what the existing
// TestBackendAliasSetPreservesInlineTableSiblings "insert" case already pins.
func TestSetTOMLInlineTableInsertCommaFreeIsByteIdentical(t *testing.T) {
	got := setTOMLScalar("ssh = { future = \"keep\" }\n", "ssh", "host_key_verification", "'strict'")
	want := "ssh = { future = \"keep\", host_key_verification = 'strict' }\n"
	assert.Equal(t, want, got)
	hasNoDoubleComma(t, got)
	loadsTOML(t, got)
}

// TestInlineTableTrailingCommaInsertE2E is the end-to-end reproduction through
// the public SetGlobalConfigValue API the CLI `af config set` reaches. A
// trailing-comma inline table that loads must be editable, not refused with
// "internal error: edited config would not load".
func TestInlineTableTrailingCommaInsertE2E(t *testing.T) {
	seed := "schema_version = 1\nssh = { future = \"keep\", }\n"
	path := writeTempConfig(t, seed)

	// Precondition: the seed itself must parse — the bug is that the loader
	// accepts the trailing comma but the editor then rejects it.
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = parseConfigTOML(before, path)
	require.NoError(t, err, "seed file with a trailing-comma inline table must parse")

	result, err := SetGlobalConfigValue("ssh.host_key_verification", "accept-new")
	require.NoError(t, err, "an edit on a loadable trailing-comma inline table must not be rejected")
	assert.Equal(t, "ssh.host_key_verification", result.Key)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(written)
	hasNoDoubleComma(t, content)
	loadsTOML(t, content)
	assert.Contains(t, content, `ssh = { future = "keep", host_key_verification = 'accept-new' }`)
	assert.Equal(t, 1, strings.Count(content, `future = "keep"`), "existing member is preserved once")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, SSHHostKeyAcceptNew, cfg.SSHHostKeyVerification, "new value is effective")
}

// TestInlineTableTrailingCommaInsertAcrossSections guards breadth: the
// dispatch condition at toml_scalar_edit.go holds for every settable scalar
// section (ssh, docker, sandbox, network), so each must accept a trailing
// comma in its root-block inline-table spelling.
func TestInlineTableTrailingCommaInsertAcrossSections(t *testing.T) {
	tests := []struct {
		name    string
		seed    string
		key     string
		value   string
		wantKey string
	}{
		{
			name: "ssh",
			seed: "schema_version = 1\nssh = { future = \"keep\", }\n",
			key:  "ssh.host_key_verification", value: "accept-new",
			wantKey: "host_key_verification = 'accept-new'",
		},
		{
			name: "docker",
			seed: "schema_version = 1\ndocker = { future = \"keep\", }\n",
			key:  "docker.mount_agent_credentials", value: "true",
			wantKey: "mount_agent_credentials = true",
		},
		{
			name: "sandbox",
			seed: "schema_version = 1\nsandbox = { future = \"keep\", }\n",
			key:  "sandbox.ssh", value: "ssh new",
			wantKey: "ssh = 'ssh new'",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.seed)
			_, err := SetGlobalConfigValue(tc.key, tc.value)
			require.NoError(t, err)
			written, err := os.ReadFile(path)
			require.NoError(t, err)
			content := string(written)
			hasNoDoubleComma(t, content)
			loadsTOML(t, content)
			assert.Contains(t, content, tc.wantKey)
			assert.Contains(t, content, `future = "keep"`, "sibling member survives")
		})
	}
}

// --- Update path (no regression): editing an EXISTING member of a
// trailing-comma table was already correct and must stay byte-identical. ---

// TestSetTOMLInlineTableUpdateOnTrailingCommaBody pins that the target >= 0
// branch — which rewrites only the member's own value range — leaves the
// trailing comma untouched and the result loadable.
func TestSetTOMLInlineTableUpdateOnTrailingCommaBody(t *testing.T) {
	got := setTOMLScalar(
		"ssh = { host_key_verification = \"insecure\", }\n",
		"ssh", "host_key_verification", "'strict'",
	)
	want := "ssh = { host_key_verification = 'strict', }\n"
	assert.Equal(t, want, got, "update preserves the trailing comma")
	hasNoDoubleComma(t, got)
	loadsTOML(t, got)
}

// --- Delete path: removing a member from a trailing-comma inline table ---

// TestDeleteTOMLInlineTableSingleMemberTrailingComma is the direct
// reproduction of the delete defect, now fixed. Removing the only member of a
// trailing-comma table must leave an empty table, not `ssh = { , }`.
func TestDeleteTOMLInlineTableSingleMemberTrailingComma(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "trailing comma", line: "ssh = { host_key_verification = \"insecure\", }\n", want: "ssh = {}\n"},
		{name: "comma-free", line: "ssh = { host_key_verification = \"insecure\" }\n", want: "ssh = {}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, removed := deleteTOMLScalar(tc.line, "ssh", "host_key_verification")
			assert.True(t, removed)
			assert.Equal(t, tc.want, got)
			loadsTOML(t, got)
			assert.NotContains(t, got, "host_key_verification")
		})
	}
}

// TestInlineTableTrailingCommaDeleteSingleMemberE2E is the end-to-end delete
// reproduction through the public UnsetGlobalConfigValue API.
func TestInlineTableTrailingCommaDeleteSingleMemberE2E(t *testing.T) {
	seed := "schema_version = 1\nssh = { host_key_verification = \"accept-new\", }\n"
	path := writeTempConfig(t, seed)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = parseConfigTOML(before, path)
	require.NoError(t, err, "seed file with a trailing-comma inline table must parse")

	result, err := UnsetGlobalConfigValue("ssh.host_key_verification")
	require.NoError(t, err)
	assert.Equal(t, "ssh.host_key_verification", result.Key)
	assert.True(t, result.Removed)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(written)
	loadsTOML(t, content)
	assert.Contains(t, content, "ssh = {}", "the emptied table is collapsed, no stranded comma")
	assert.NotContains(t, content, "host_key_verification")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, SSHHostKeyStrict, cfg.SSHHostKeyVerification, "unset falls back to the strict default")
}
