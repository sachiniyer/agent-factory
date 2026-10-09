package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the inline-table multiline-member regression suite for the
// surgical editor (tomlInlineMembers / setTOMLInlineTableMember /
// deleteTOMLInlineTableMember in toml_surgical.go).
//
// tomlInlineMembers splits an inline-table body on top-level commas while
// ignoring commas inside strings/braces/brackets. It used to track string
// state with a per-quote toggle, which read a """ / ''' delimiter as three
// independent open/close flips: an unescaped " (inside """...""") or ' (inside
// '''...''') mid-string flipped the scanner "outside", so a comma sitting
// INSIDE a one-line multiline member was recorded as a member separator. That
// made setTOMLInlineTableMember edit a truncated range and emit unloadable
// TOML, which the pre-write re-parse gate converted into a refusal — so
// `af config set sandbox.ssh new` on a valid, loadable hand-edited config was
// blocked with `internal error: edited config would not load (no changes
// written)`.
//
// The scanner now reuses the same open-delimiter state machine
// scanTrailingComment already drives (#3459), so """ / ''' are scanned as
// three-byte UNITS and commas inside them stay ignored. The cases below assert
// the member split, the update path, the delete path, the insert path
// (regression guard), and the end-to-end public SetGlobalConfigValue /
// UnsetGlobalConfigValue paths. The escaped-quote cases are controls that
// pass both before and after the fix and pin that the defect is specifically
// about UNESCAPED embedded quotes.

// --- Direct scanner: tomlInlineMembers ---

// TestTomlInlineMembersSplitsMultilineStringMembers pins the scanner itself.
// A comma inside a one-line multiline string member (triple-quoted basic or
// literal) is content, never a separator, so the body splits into exactly the
// two real members with the multiline member intact. Before the fix this
// returned three (or two with a truncated first) members.
func TestTomlInlineMembersSplitsMultilineStringMembers(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		members []string
	}{
		{
			name:    "basic multiline member with embedded quote and comma",
			body:    ` ssh = """He said "hi," then left.""" , future = "kept" `,
			members: []string{`ssh = """He said "hi," then left."""`, `future = "kept"`},
		},
		{
			name:    "literal multiline member with embedded apostrophe and comma",
			body:    ` ssh = '''He said 'hi,' then left.''' , future = "kept" `,
			members: []string{`ssh = '''He said 'hi,' then left.'''`, `future = "kept"`},
		},
		{
			name: "basic multiline member with a trailing quote run",
			body: ` ssh = """He said "hi,""""" , future = "kept" `,
			// """He said "hi,""""" has 5 trailing quotes (2 content + 3
			// closing); the punctuator is the LAST run, so the whole member is
			// one entry whose raw text retains all five quotes.
			members: []string{"ssh = \"\"\"He said \"hi,\"\"\"\"\"", `future = "kept"`},
		},
		{
			// Control: \" inside """ never closes the string, so the existing
			// escape handling already kept this to two members. Pinned to prove
			// the fix did not change the escaped path.
			name:    "basic multiline member with escaped quotes",
			body:    ` ssh = """He said \"hi,\" then left.""" , future = "kept" `,
			members: []string{`ssh = """He said \"hi,\" then left."""`, `future = "kept"`},
		},
		{
			// Control: an ordinary basic string with an escaped comma was
			// already handled correctly by the escape flag.
			name:    "plain basic string with escaped comma",
			body:    ` ssh = "He said \"hi,\" then left." , future = "kept" `,
			members: []string{`ssh = "He said \"hi,\" then left."`, `future = "kept"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tomlInlineMembers(tc.body)
			require.Len(t, got, len(tc.members), "member count for body: %q", tc.body)
			for i, want := range tc.members {
				assert.Equal(t, want, tc.body[got[i].trimStart:got[i].trimEnd],
					"member %d must be intact (no mid-string split)", i)
			}
		})
	}
}

// --- Direct editor: UPDATE path (the corruption path) ---

// TestSetTOMLScalarUpdatesMultilineInlineTableMember is the direct
// reproduction of the reported corruption. Updating a member whose value is a
// one-line multiline string (triple-quoted basic or literal) containing a
// comma must replace only that member and leave a loadable table with the
// sibling intact; before the fix the member was truncated at the in-string
// comma and the re-parse gate refused the edit.
func TestSetTOMLScalarUpdatesMultilineInlineTableMember(t *testing.T) {
	want := "sandbox = { ssh = 'ssh new' , future = \"kept\" }\n"
	tests := []struct {
		name string
		line string
	}{
		{
			name: "basic delimiter, embedded quote and comma",
			line: "sandbox = { ssh = \"\"\"He said \"hi,\" then left.\"\"\" , future = \"kept\" }\n",
		},
		{
			name: "literal delimiter, embedded apostrophe and comma",
			line: "sandbox = { ssh = '''He said 'hi,' then left.''' , future = \"kept\" }\n",
		},
		{
			name: "basic delimiter, trailing quote run",
			line: "sandbox = { ssh = \"\"\"He said \"hi,\"\"\"\"\" , future = \"kept\" }\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := setTOMLScalar(tc.line, "sandbox", "ssh", "'ssh new'")
			assert.Equal(t, want, got, "exact bytes for the multiline-member update")
			loadsTOML(t, got)
			assert.Contains(t, got, `future = "kept"`, "sibling member is preserved")
			assert.Equal(t, 1, strings.Count(got, `future = "kept"`), "sibling is preserved once")
		})
	}
}

// TestSetTOMLScalarUpdatesMultilineInlineTableMemberEscapedQuotes is a control:
// escaped \" inside a """ member never closed the string under the old toggle
// either, so this path was already correct. Pinned to prove the fix left the
// escaped path byte-identical.
func TestSetTOMLScalarUpdatesMultilineInlineTableMemberEscapedQuotes(t *testing.T) {
	line := "sandbox = { ssh = \"\"\"He said \\\"hi,\\\" then left.\"\"\" , future = \"kept\" }\n"
	got := setTOMLScalar(line, "sandbox", "ssh", "'ssh new'")
	want := "sandbox = { ssh = 'ssh new' , future = \"kept\" }\n"
	assert.Equal(t, want, got)
	loadsTOML(t, got)
	assert.Contains(t, got, `future = "kept"`)
}

// --- Direct editor: INSERT path (regression guard) ---

// TestSetTOMLScalarInsertsNextToMultilineMemberWithComma guards the insert
// path. Inserting a NEW member next to a sibling whose value is """a,b"""
// must reuse the whole body (the insert path does not carve on per-member
// boundaries), so the comma inside the sibling is irrelevant and the result
// is valid TOML. This passes both before and after the fix and pins that the
// scanner change did not disturb the insert path.
func TestSetTOMLScalarInsertsNextToMultilineMemberWithComma(t *testing.T) {
	line := "docker = { future = \"\"\"a,b\"\"\" }\n"
	got := setTOMLScalar(line, "docker", "mount_agent_credentials", "true")
	want := "docker = { future = \"\"\"a,b\"\"\", mount_agent_credentials = true }\n"
	assert.Equal(t, want, got)
	loadsTOML(t, got)
	assert.Contains(t, got, `future = """a,b"""`, "multiline sibling survives intact")
	assert.Contains(t, got, "mount_agent_credentials = true")
}

// --- Direct editor: DELETE path ---

// TestDeleteTOMLScalarMultilineInlineTableMember deletes the multiline member
// itself. Before the fix the member was truncated at the in-string comma and
// the deletion left `sandbox = { " then left.""" , future = "kept" }` —
// unloadable. The fix leaves the sibling as the sole member.
func TestDeleteTOMLScalarMultilineInlineTableMember(t *testing.T) {
	line := "sandbox = { ssh = \"\"\"He said \"hi,\" then left.\"\"\" , future = \"kept\" }\n"
	got, removed := deleteTOMLScalar(line, "sandbox", "ssh")
	assert.True(t, removed)
	assert.Equal(t, "sandbox = { future = \"kept\" }\n", got)
	loadsTOML(t, got)
	assert.NotContains(t, got, "ssh")
	assert.Contains(t, got, `future = "kept"`)
}

// TestDeleteTOMLScalarSiblingPreservesMultilineMember is a guard for the
// other delete branch: removing a SIBLING while a """...""" member with a
// comma stays must leave that multiline member byte-intact. (This happened to
// pass before the fix because the delete reconstruction rejoined the
// fragment; it is pinned to keep it that way.)
func TestDeleteTOMLScalarSiblingPreservesMultilineMember(t *testing.T) {
	line := "sandbox = { ssh = \"\"\"He said \"hi,\" then left.\"\"\" , future = \"kept\" }\n"
	got, removed := deleteTOMLScalar(line, "sandbox", "future")
	assert.True(t, removed)
	assert.Equal(t, "sandbox = { ssh = \"\"\"He said \"hi,\" then left.\"\"\" }\n", got)
	loadsTOML(t, got)
	assert.Contains(t, got, `ssh = """He said "hi," then left."""`)
	assert.NotContains(t, got, "future")
}

// --- End-to-end: public SetGlobalConfigValue / UnsetGlobalConfigValue ---

// TestSetGlobalConfigValueInlineMultilineMember drives the public `af config
// set` path (SetGlobalConfigValue) the CLI calls, in both delimiter styles,
// the trailing-quote-run variant, and the escaped-quote control. A valid,
// loadable config whose inline-table member is a one-line multiline string
// with a comma must be editable, not refused with "internal error: edited
// config would not load".
func TestSetGlobalConfigValueInlineMultilineMember(t *testing.T) {
	tests := []struct {
		name string
		seed string
	}{
		{
			name: "basic delimiter, embedded quote and comma",
			seed: "schema_version = 1\nsandbox = { ssh = \"\"\"He said \"hi,\" then left.\"\"\" , future = \"kept\" }\n",
		},
		{
			name: "literal delimiter, embedded apostrophe and comma",
			seed: "schema_version = 1\nsandbox = { ssh = '''He said 'hi,' then left.''' , future = \"kept\" }\n",
		},
		{
			name: "basic delimiter, trailing quote run",
			seed: "schema_version = 1\nsandbox = { ssh = \"\"\"He said \"hi,\"\"\"\"\" , future = \"kept\" }\n",
		},
		{
			// Control: this seed was editable before the fix too, and must stay so.
			name: "escaped quotes (control)",
			seed: "schema_version = 1\nsandbox = { ssh = \"\"\"He said \\\"hi,\\\" then left.\"\"\" , future = \"kept\" }\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.seed)

			// Precondition: the seed itself must parse — the bug is that the
			// loader accepts this inline table but the editor then rejects it.
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			_, err = parseConfigTOML(before, path)
			require.NoError(t, err, "seed with a multiline inline-table member must parse")

			res, err := SetGlobalConfigValue("sandbox.ssh", "ssh new")
			require.NoError(t, err, "an edit on a loadable inline-table member must not be rejected")
			assert.Equal(t, "sandbox.ssh", res.Key)
			assert.Equal(t, "ssh new", res.Value)

			written, err := os.ReadFile(path)
			require.NoError(t, err)
			content := string(written)
			loadsTOML(t, content)
			assert.Contains(t, content, "ssh = 'ssh new'", "the target member is updated")
			assert.Contains(t, content, `future = "kept"`, "the sibling member survives")
			assert.Equal(t, 1, strings.Count(content, `future = "kept"`), "sibling is preserved once")

			cfg, err := LoadConfig()
			require.NoError(t, err)
			assert.Equal(t, "ssh new", cfg.SandboxSSH, "the new value is effective")
		})
	}
}

// TestUnsetGlobalConfigValueInlineMultilineMember drives the public `af config
// unset` path. Removing the inline-table member that is a one-line multiline
// string with a comma must leave the sibling as the sole member and a file
// that still loads; before the fix the member was truncated and the re-parse
// gate refused the unset.
func TestUnsetGlobalConfigValueInlineMultilineMember(t *testing.T) {
	seed := "schema_version = 1\nsandbox = { ssh = \"\"\"He said \"hi,\" then left.\"\"\" , future = \"kept\" }\n"
	path := writeTempConfig(t, seed)

	before, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = parseConfigTOML(before, path)
	require.NoError(t, err, "seed with a multiline inline-table member must parse")

	res, err := UnsetGlobalConfigValue("sandbox.ssh")
	require.NoError(t, err, "an unset on a loadable inline-table member must not be rejected")
	assert.Equal(t, "sandbox.ssh", res.Key)
	assert.True(t, res.Removed)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(written)
	loadsTOML(t, content)
	assert.Contains(t, content, "sandbox = { future = \"kept\" }", "the sibling is the sole member")
	assert.NotContains(t, content, "ssh = ")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.SandboxSSH, "the removed value falls back to the empty default")
}

// TestSetGlobalConfigValueInlineMultilineMemberInsertAcrossSections is a
// breadth regression guard: the inline-table dispatch condition holds for
// every settable scalar section, and inserting a member next to a """..."""
// sibling that contains a comma must still emit valid TOML for each.
func TestSetGlobalConfigValueInlineMultilineMemberInsertAcrossSections(t *testing.T) {
	tests := []struct {
		name    string
		seed    string
		key     string
		value   string
		wantKey string
	}{
		{
			name:    "ssh",
			seed:    "schema_version = 1\nssh = { future = \"\"\"a,b\"\"\" }\n",
			key:     "ssh.host_key_verification",
			value:   "accept-new",
			wantKey: "host_key_verification = 'accept-new'",
		},
		{
			name:    "docker",
			seed:    "schema_version = 1\ndocker = { future = \"\"\"a,b\"\"\" }\n",
			key:     "docker.mount_agent_credentials",
			value:   "true",
			wantKey: "mount_agent_credentials = true",
		},
		{
			name:    "sandbox",
			seed:    "schema_version = 1\nsandbox = { future = \"\"\"a,b\"\"\" }\n",
			key:     "sandbox.ssh",
			value:   "ssh new",
			wantKey: "ssh = 'ssh new'",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.seed)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			_, err = parseConfigTOML(before, path)
			require.NoError(t, err, "seed must parse")

			_, err = SetGlobalConfigValue(tc.key, tc.value)
			require.NoError(t, err)
			written, err := os.ReadFile(path)
			require.NoError(t, err)
			content := string(written)
			loadsTOML(t, content)
			assert.Contains(t, content, tc.wantKey, "the new member is inserted")
			assert.Contains(t, content, `future = """a,b"""`, "the multiline sibling survives intact")
		})
	}
}
