package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// #3672: the skill and plugin files af regenerates on every session launch
// refuse a symlinked destination.

// writeAfMarkedFile decides whether it may overwrite by looking for afSkillMarker
// in the file at our path — and os.ReadFile follows a link to find it. So a
// MARKED target behind a link would authorize replacing the LINK, on the
// strength of content that belongs to the target. Refusing keeps the ownership
// answer and the write pointed at the same inode.
func TestWriteAfMarkedFileRefusesASymlinkedSkillFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "SKILL.md")
	marked := "---\nname: agent-factory\n---\n<!-- " + afSkillMarker + " -->\nold body\n"
	require.NoError(t, os.WriteFile(target, []byte(marked), 0644))

	link := filepath.Join(t.TempDir(), "SKILL.md")
	require.NoError(t, os.Symlink(target, link))

	wrote, err := writeAfMarkedFile(link, "new body\n")
	require.Error(t, err, "a marked TARGET must not authorize replacing the LINK")
	assert.False(t, wrote)
	assert.ErrorIs(t, err, config.ErrManagedFileSymlink)
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), target)

	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink)

	on, rerr := os.ReadFile(target)
	require.NoError(t, rerr)
	assert.Equal(t, marked, string(on), "and the target keeps its content")
}

// The delete side of the same file, and the #3672 asymmetry pointed the other
// way: removeAfSkillDir reads the marker THROUGH the link, so a plain os.Remove
// would unlink the user's link on the strength of the target's content.
func TestRemoveAfSkillDirRefusesToUnlinkALink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "SKILL.md")
	marked := "<!-- " + afSkillMarker + " -->\n"
	require.NoError(t, os.WriteFile(target, []byte(marked), 0644))

	skillDir := filepath.Join(t.TempDir(), afSkillDirName)
	require.NoError(t, os.MkdirAll(skillDir, 0755))
	link := filepath.Join(skillDir, "SKILL.md")
	require.NoError(t, os.Symlink(target, link))

	removeAfSkillDir(skillDir, link)

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"af must not delete a link it could never have written through")
	assert.FileExists(t, target)
}

// The plugin directory is regenerated wholesale on every claude session launch —
// manifest rewritten, commands rewritten, undeclared ones pruned — so a link
// inside it is a path af would otherwise clobber on the next launch.
func TestEnsurePluginDirRefusesASymlinkedCommandFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	target := filepath.Join(t.TempDir(), "af.md")
	require.NoError(t, os.WriteFile(target, []byte("user's own command\n"), 0644))

	commandsDir := filepath.Join(home, "plugin", "commands")
	require.NoError(t, os.MkdirAll(commandsDir, 0755))
	var link string
	for name := range pluginCommands {
		link = filepath.Join(commandsDir, name)
		break
	}
	require.NotEmpty(t, link, "pluginCommands must declare at least one command to link")
	require.NoError(t, os.Symlink(target, link))

	_, err := ensurePluginDir()
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrManagedFileSymlink)
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), target)

	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink)

	on, rerr := os.ReadFile(target)
	require.NoError(t, rerr)
	assert.Equal(t, "user's own command\n", string(on))
}
