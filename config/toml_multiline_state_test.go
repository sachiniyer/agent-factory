package config

import (
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the #3662 regression suite. Every surgical helper in this package
// walks config.toml a line at a time and decides what each line IS — a
// `[section]` header, a `key = value` assignment — without tracking TOML
// multiline-string state. A line that merely LOOKS like syntax while sitting
// inside an open ''' or """ block was therefore read as syntax, and `af config
// set` edited it: the user's multiline value was rewritten, the key they asked
// to change was left alone, and the command reported success.
//
// The fixtures below all put a decoy inside a multiline value. They are valid
// TOML, and a shell script that happens to contain `branch_prefix = "…"` or a
// `[section]` line is an ordinary thing to store in on_archive_command.

// issueReproConfig is the exact reproduction from #3662.
const issueReproConfig = `schema_version = 1
on_archive_command = '''echo start
branch_prefix = "decoy"
echo done'''
branch_prefix = 'me/'
`

// TestSetGlobalConfigValueLeavesMultilineStringContentAlone is the issue's
// reproduction, end to end and in both delimiter styles: the decoy line inside
// the multiline value is content, so the edit must land on the real
// `branch_prefix` key below it and the stored command must come back
// byte-identical.
func TestSetGlobalConfigValueLeavesMultilineStringContentAlone(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		command string
	}{
		{
			name:    "literal delimiter",
			content: issueReproConfig,
			command: "echo start\nbranch_prefix = \"decoy\"\necho done",
		},
		{
			name:    "basic delimiter",
			content: "schema_version = 1\non_archive_command = \"\"\"echo start\nbranch_prefix = 'decoy'\necho done\"\"\"\nbranch_prefix = 'me/'\n",
			command: "echo start\nbranch_prefix = 'decoy'\necho done",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempConfig(t, tt.content)
			t.Setenv("SHELL", "/bin/sh")

			res, err := SetGlobalConfigValue("branch_prefix", "new/")
			require.NoError(t, err)
			assert.Equal(t, "new/", res.Value)

			cfg, err := LoadConfig()
			require.NoError(t, err)
			assert.Equal(t, "new/", cfg.BranchPrefix, "the key the user asked to change must actually change")
			assert.Equal(t, tt.command, cfg.OnArchiveCommand,
				"a line inside a multiline string is content; `config set` must not rewrite it")

			written := readFile(t, path)
			assert.Contains(t, written, "echo start", "the multiline value must survive intact")
			assert.Contains(t, written, "echo done")
		})
	}
}

// TestUnsetGlobalConfigValueLeavesMultilineStringContentAlone is the same
// blindness on the delete side: `af config unset` scans for the key's line with
// the same helper, so a decoy spelled like the target inside a multiline value
// was removed instead of the real assignment.
func TestUnsetGlobalConfigValueLeavesMultilineStringContentAlone(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		command string
	}{
		{
			name: "literal delimiter",
			content: `schema_version = 1
on_archive_command = '''echo start
listen_addr = "decoy:1"
echo done'''
listen_addr = '0.0.0.0:8443'
`,
			command: "echo start\nlisten_addr = \"decoy:1\"\necho done",
		},
		{
			name:    "basic delimiter",
			content: "schema_version = 1\non_archive_command = \"\"\"echo start\nlisten_addr = 'decoy:1'\necho done\"\"\"\nlisten_addr = '0.0.0.0:8443'\n",
			command: "echo start\nlisten_addr = 'decoy:1'\necho done",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempConfig(t, tt.content)
			t.Setenv("SHELL", "/bin/sh")

			res, err := UnsetGlobalConfigValue("network.listen_addr")
			require.NoError(t, err)
			assert.True(t, res.Removed)

			cfg, err := LoadConfig()
			require.NoError(t, err)
			assert.Equal(t, DefaultConfig().ListenAddr, cfg.ListenAddr,
				"the real listen_addr line is the one that must be removed")
			assert.Equal(t, tt.command, cfg.OnArchiveCommand,
				"a line inside a multiline string is content; `config unset` must not remove it")
			assert.Contains(t, readFile(t, path), "echo done")
		})
	}
}

// TestMigrateLeavesMultilineStringContentAlone covers the third writer. The
// migration reads the deprecated key's raw bytes with tomlRootScalarRawValue and
// removes its line with deleteTOMLScalar; both used to stop at the decoy, which
// its own value-drift guard then caught — so today the command refuses a
// perfectly valid config instead of migrating it.
func TestMigrateLeavesMultilineStringContentAlone(t *testing.T) {
	const body = `schema_version = 1
on_archive_command = '''echo start
sandbox_ssh = "decoy"
echo done'''
sandbox_ssh = 'ssh -T real'
`
	path := migrateHome(t, body)
	before, err := parseConfigTOML([]byte(body), path)
	require.NoError(t, err)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)
	require.Len(t, result.Migrated, 1)
	assert.Equal(t, "sandbox.ssh", result.Migrated[0].To)

	content := readFile(t, path)
	assert.Contains(t, content, "[sandbox]")
	assert.Contains(t, content, "ssh = 'ssh -T real'", "the real value moves, not the decoy")
	assert.Contains(t, content, "sandbox_ssh = \"decoy\"", "the decoy is string content and stays put")

	after, err := parseConfigTOML([]byte(content), path)
	require.NoError(t, err)
	assert.Empty(t, configValueDrift(before, after))
}

// TestSetTOMLScalarIgnoresDecoySectionHeaderInsideMultilineString is the second
// half of the same blindness, and the one #3653 could only work around: a
// `[section]` line inside a multiline value made setTOMLScalar believe that
// section was open, so a leaf destined for it was inserted INTO the string.
func TestSetTOMLScalarIgnoresDecoySectionHeaderInsideMultilineString(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
	}{
		{
			name: "literal delimiter",
			content: `on_archive_command = '''echo start
[network]
echo done'''
`,
		},
		{
			name:    "basic delimiter",
			content: "on_archive_command = \"\"\"echo start\n[network]\necho done\"\"\"\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := setTOMLScalar(tt.content, "network", "listen_addr", "'0.0.0.0:8443'")
			assert.Contains(t, got, tt.content, "the multiline value must be left byte-identical")
			var shape map[string]any
			require.NoError(t, toml.Unmarshal([]byte(got), &shape))
			network, ok := shape["network"].(map[string]any)
			require.True(t, ok, "a real [network] table must have been appended:\n%s", got)
			assert.Equal(t, "0.0.0.0:8443", network["listen_addr"])
		})
	}
}

// TestDeleteTOMLScalarIgnoresDecoyInsideMultilineString pins the delete helper
// directly, including the section-scoping half: a decoy `[section]` header
// inside a string used to put every following line under the wrong section.
func TestDeleteTOMLScalarIgnoresDecoyInsideMultilineString(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		section string
		leaf    string
		want    string
	}{
		{
			name: "root key decoy, literal delimiter",
			content: `on_archive_command = '''echo start
branch_prefix = "decoy"
echo done'''
branch_prefix = 'me/'
`,
			leaf: "branch_prefix",
			want: `on_archive_command = '''echo start
branch_prefix = "decoy"
echo done'''
`,
		},
		{
			name:    "root key decoy, basic delimiter",
			content: "on_archive_command = \"\"\"echo start\nbranch_prefix = 'decoy'\necho done\"\"\"\nbranch_prefix = 'me/'\n",
			leaf:    "branch_prefix",
			want:    "on_archive_command = \"\"\"echo start\nbranch_prefix = 'decoy'\necho done\"\"\"\n",
		},
		{
			name: "decoy section header hides the real section",
			content: `on_archive_command = '''echo start
[network]
echo done'''

[network]
listen_addr = '0.0.0.0:8443'
`,
			section: "network",
			leaf:    "listen_addr",
			want: `on_archive_command = '''echo start
[network]
echo done'''

[network]
`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, removed := deleteTOMLScalar(tt.content, tt.section, tt.leaf)
			assert.True(t, removed)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestTomlRootScalarRawValueIgnoresDecoyInsideMultilineString and its dotted
// twin pin the two helpers #3653 added, which share the blindness.
func TestTomlRootScalarRawValueIgnoresDecoyInsideMultilineString(t *testing.T) {
	const content = `on_archive_command = '''echo start
sandbox_ssh = "decoy"
echo done'''
sandbox_ssh = 'ssh -T real'
`
	value, ok := tomlRootScalarRawValue(content, "sandbox_ssh")
	require.True(t, ok)
	assert.Equal(t, "'ssh -T real'", value, "the raw value must come from the real assignment")
}

func TestTomlRootDottedTableIgnoresDecoyInsideMultilineString(t *testing.T) {
	const content = `on_archive_command = '''echo start
sandbox.other = "decoy"
echo done'''
sandbox_ssh = 'ssh -T real'
`
	assert.False(t, tomlRootDottedTable(content, "sandbox"),
		"a dotted key inside a multiline string does not open a table")

	const real = "sandbox.other = 'x'\n"
	assert.True(t, tomlRootDottedTable(real, "sandbox"), "a real dotted key still counts")
}

// TestSetTOMLScalarIgnoresDecoyKeyInsideMultilineString pins the set helper
// directly for both delimiter styles and for a decoy inside a [section].
func TestSetTOMLScalarIgnoresDecoyKeyInsideMultilineString(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		section string
		leaf    string
		want    string
	}{
		{
			name: "root decoy, literal delimiter",
			content: `on_archive_command = '''echo start
branch_prefix = "decoy"
echo done'''
branch_prefix = 'me/'
`,
			leaf: "branch_prefix",
			want: `on_archive_command = '''echo start
branch_prefix = "decoy"
echo done'''
branch_prefix = 'new/'
`,
		},
		{
			name:    "root decoy, basic delimiter",
			content: "on_archive_command = \"\"\"echo start\nbranch_prefix = 'decoy'\necho done\"\"\"\nbranch_prefix = 'me/'\n",
			leaf:    "branch_prefix",
			want:    "on_archive_command = \"\"\"echo start\nbranch_prefix = 'decoy'\necho done\"\"\"\nbranch_prefix = 'new/'\n",
		},
		{
			name: "decoy inside a sectioned multiline value",
			content: `[sandbox]
ssh = '''echo start
branch_prefix = "decoy"
echo done'''
`,
			leaf: "branch_prefix",
			want: `branch_prefix = 'new/'
[sandbox]
ssh = '''echo start
branch_prefix = "decoy"
echo done'''
`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, setTOMLScalar(tt.content, tt.section, tt.leaf, "'new/'"))
		})
	}
}

// TestSetGlobalConfigValueDecoySectionHeaderRoundTrip is the [section]-decoy
// half end to end: setting a grouped key while a multiline value contains that
// very header must produce a real table, not a line spliced into the string.
func TestSetGlobalConfigValueDecoySectionHeaderRoundTrip(t *testing.T) {
	const body = `schema_version = 1
on_archive_command = '''echo start
[network]
listen_addr = "decoy:1"
echo done'''
`
	writeTempConfig(t, body)
	t.Setenv("SHELL", "/bin/sh")

	_, err := SetGlobalConfigValue("network.listen_addr", "0.0.0.0:8443")
	require.NoError(t, err)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:8443", cfg.ListenAddr)
	assert.Equal(t, "echo start\n[network]\nlisten_addr = \"decoy:1\"\necho done", cfg.OnArchiveCommand)
}
