package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func frontmatterField(t *testing.T, doc, key string) string {
	t.Helper()
	require.True(t, strings.HasPrefix(doc, "---\n"), "doc must open with frontmatter")
	end := strings.Index(doc[4:], "\n---\n")
	require.GreaterOrEqual(t, end, 0, "frontmatter must close")
	for _, line := range strings.Split(doc[4:4+end], "\n") {
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return v
		}
	}
	t.Fatalf("frontmatter has no %q field", key)
	return ""
}

func TestEnsurePluginDirWritesProposalSkill(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pluginDir, err := ensurePluginDir(fullGuidanceCaps)
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(pluginDir, "skills", afProposalSkillName, "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, pluginSkills(fullGuidanceCaps)[afProposalSkillName], string(got))
	assert.Equal(t, afProposalSkillName, frontmatterField(t, string(got), "name"))
}

func TestProposalSkillCarriesTheSharedProposalsText(t *testing.T) {
	doc := pluginSkills(fullGuidanceCaps)[afProposalSkillName]
	assert.Contains(t, doc, afUsageProposalsInside)
	assert.Contains(t, afUsageReference, afUsageProposalsInside)
	for _, cmd := range []string{"af proposal start", "af proposal list", "af proposal attach"} {
		assert.Contains(t, doc, cmd)
	}
}

func TestProposalTriggersReachEveryOnDemandSurface(t *testing.T) {
	descriptions := map[string]string{
		"claude proposal skill":           frontmatterField(t, pluginSkills(fullGuidanceCaps)[afProposalSkillName], "description"),
		"amp/codex/gemini/devin SKILL.md": frontmatterField(t, afSkillDoc, "description"),
	}
	for surface, desc := range descriptions {
		for _, word := range []string{"make", "write", "draft", "start", "proposal", "plan", "spec", "design doc", "continue", "review", "update"} {
			assert.Contains(t, desc, word, "%s description must trigger on %q", surface, word)
		}
	}
	assert.Contains(t, frontmatterField(t, pluginCommands(fullGuidanceCaps)["af.md"], "description"), "proposals")
}

// Frontmatter values are unquoted YAML scalars, where ": " or " #" would end or
// break the value.
func TestPluginDescriptionsAreSafeYAMLScalars(t *testing.T) {
	docs := map[string]string{
		"af.md":          pluginCommands(fullGuidanceCaps)["af.md"],
		"proposal":       pluginSkills(fullGuidanceCaps)[afProposalSkillName],
		"agent SKILL.md": afSkillDoc,
	}
	for name, doc := range docs {
		desc := frontmatterField(t, doc, "description")
		assert.NotContains(t, desc, ": ", name)
		assert.NotContains(t, desc, " #", name)
	}
}

func TestPluginOutsideAfOmitsProposals(t *testing.T) {
	assert.NotContains(t, AfSkillDescription, "proposal")
	assert.NotContains(t, AfPluginUsageReference, "af proposal")
}

func TestEnsurePluginDirPrunesStaleSkills(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	stale := filepath.Join(home, "plugin", "skills", "retired")
	require.NoError(t, os.MkdirAll(stale, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "SKILL.md"), []byte("old"), 0644))

	keptDir := filepath.Join(home, "plugin", "skills", "retired-with-extra")
	require.NoError(t, os.MkdirAll(keptDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(keptDir, "SKILL.md"), []byte("old"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(keptDir, "notes.txt"), []byte("keep"), 0644))

	_, err := ensurePluginDir(fullGuidanceCaps)
	require.NoError(t, err)

	_, err = os.Stat(stale)
	assert.True(t, os.IsNotExist(err), "stale skill dir must be removed, got %v", err)
	_, err = os.Stat(filepath.Join(keptDir, "SKILL.md"))
	assert.True(t, os.IsNotExist(err), "stale SKILL.md must be removed, got %v", err)
	_, err = os.Stat(filepath.Join(keptDir, "notes.txt"))
	assert.NoError(t, err, "a non-skill file must survive the prune")
}

func TestEnsurePluginDirRefusesASymlinkedSkillFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	target := filepath.Join(t.TempDir(), "SKILL.md")
	require.NoError(t, os.WriteFile(target, []byte("user's own skill\n"), 0644))
	dir := filepath.Join(home, "plugin", "skills", afProposalSkillName)
	require.NoError(t, os.MkdirAll(dir, 0755))
	link := filepath.Join(dir, "SKILL.md")
	require.NoError(t, os.Symlink(target, link))

	_, err := ensurePluginDir(fullGuidanceCaps)
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrManagedFileSymlink)

	on, rerr := os.ReadFile(target)
	require.NoError(t, rerr)
	assert.Equal(t, "user's own skill\n", string(on))
}
