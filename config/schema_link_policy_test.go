package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrationWriteBackFollowsALinkWhenThePlanSaysSo is the config-level half of
// #3718: LoadAndMigrateSchemaFile takes the answer from the plan, so a store
// whose ordinary writes follow a symlink gets a write-back that follows too.
func TestMigrationWriteBackFollowsALinkWhenThePlanSaysSo(t *testing.T) {
	afHome := t.TempDir()
	elsewhere := t.TempDir()

	target := filepath.Join(elsewhere, "store.json")
	legacy := []byte(`{"name":"alpha"}`)
	require.NoError(t, os.WriteFile(target, legacy, 0644))

	link := filepath.Join(afHome, "store.json")
	require.NoError(t, os.Symlink(target, link))

	registry := NewSchemaMigrationRegistry()
	require.NoError(t, registry.Register(0, migrateObjectToVersion(1)))
	upgraded, result, err := LoadAndMigrateSchemaFile(SchemaMigrationPlan{
		StoreName:      "store.json",
		Path:           link,
		CurrentVersion: 1,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      registry,
		Validate:       validateObjectVersion(1),
		LinkPolicy:     SchemaWriteFollowLink,
	})
	require.NoError(t, err)
	require.True(t, result.Migrated)

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the write-back replaced the link at %s", link)

	onTarget, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.JSONEq(t, string(upgraded), string(onTarget))

	// The backup is af's scratch copy of a file af only rewrites, so it stays
	// beside the LINK rather than being dropped into the user's directory.
	assert.Equal(t, link+".bak.schema-v0", result.BackupPath)
	backup, err := os.ReadFile(result.BackupPath)
	require.NoError(t, err)
	assert.Equal(t, legacy, backup)
}

// TestInstancesMigrationStillReplacesASymlink is the no-change pin for the OTHER
// caller of the same helper.
//
// instances.json is af-managed state at a path af chose, so #3672 leaves it on
// the plain writer's side and #3718 must not move it. The pin is behavioural
// rather than a comparison of struct fields: a future edit that flipped the
// default, or reached for the follow writer here because it sounded safer, would
// change what lands on disk and this is what notices.
func TestInstancesMigrationStillReplacesASymlink(t *testing.T) {
	tempHome := t.TempDir()
	elsewhere := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	repoID := RepoIDFromRoot("/repo/alpha")
	link, err := repoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0755))

	target := filepath.Join(elsewhere, InstancesFileName)
	legacy := []byte(`[{"title":"alpha","path":"/repo/alpha"}]`)
	require.NoError(t, os.WriteFile(target, legacy, 0644))
	require.NoError(t, os.Symlink(target, link))

	assert.Equal(t, SchemaWriteReplaceLink, NewInstancesSchemaMigrationPlan(link).LinkPolicy)

	result, err := MigrateRepoInstancesForDaemonLoad(repoID)
	require.NoError(t, err)
	require.True(t, result.Migrated)

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Zero(t, info.Mode()&os.ModeSymlink,
		"instances.json is af-managed; #3718 must not move it to the follow side")

	onDisk, err := os.ReadFile(link)
	require.NoError(t, err)
	var envelope instancesEnvelope
	require.NoError(t, json.Unmarshal(onDisk, &envelope))
	assert.Equal(t, InstancesSchemaVersion, envelope.SchemaVersion)

	untouched, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, legacy, untouched, "the replacing writer must leave the link's old target alone")
}

// TestUnsetLinkPolicyReplaces pins the ZERO VALUE. Following a link is a promise
// a store asks for; a plan that says nothing must get the behaviour every store
// had before the field existed.
func TestUnsetLinkPolicyReplaces(t *testing.T) {
	afHome := t.TempDir()
	elsewhere := t.TempDir()

	target := filepath.Join(elsewhere, "store.json")
	legacy := []byte(`{"name":"alpha"}`)
	require.NoError(t, os.WriteFile(target, legacy, 0644))
	link := filepath.Join(afHome, "store.json")
	require.NoError(t, os.Symlink(target, link))

	registry := NewSchemaMigrationRegistry()
	require.NoError(t, registry.Register(0, migrateObjectToVersion(1)))
	_, result, err := LoadAndMigrateSchemaFile(SchemaMigrationPlan{
		StoreName:      "store.json",
		Path:           link,
		CurrentVersion: 1,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      registry,
		Validate:       validateObjectVersion(1),
	})
	require.NoError(t, err)
	require.True(t, result.Migrated)

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Zero(t, info.Mode()&os.ModeSymlink)
	untouched, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, legacy, untouched)
}

// TestUnknownLinkPolicyIsRejectedBeforeTheFileIsTouched pins that the refusal
// happens in plan validation rather than at the writer, so a plan nobody
// recognises costs no backup file and no rewritten store.
func TestUnknownLinkPolicyIsRejectedBeforeTheFileIsTouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	legacy := []byte(`{"name":"alpha"}`)
	require.NoError(t, os.WriteFile(path, legacy, 0644))

	registry := NewSchemaMigrationRegistry()
	require.NoError(t, registry.Register(0, migrateObjectToVersion(1)))
	_, result, err := LoadAndMigrateSchemaFile(SchemaMigrationPlan{
		StoreName:      "store.json",
		Path:           path,
		CurrentVersion: 1,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      registry,
		Validate:       validateObjectVersion(1),
		LinkPolicy:     SchemaWriteLinkPolicy(42),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown schema write link policy 42")
	assert.Empty(t, result.BackupPath)

	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, legacy, onDisk)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".lock") {
			names = append(names, entry.Name())
		}
	}
	assert.Equal(t, []string{"store.json"}, names)
}

// TestFollowLinkPolicyStaysWithTheStoresThatAskedForIt is the fence, and it is
// the reason #3718 is a plan FIELD rather than a writer the plan carries.
//
// TestFollowingWriterStaysInsideTheConfigPackage fences AtomicWriteFileFollowingLink
// by name, so the follow promise cannot spread by someone calling the writer
// because it sounded safer. A policy constant reaches the same writer without
// naming it, so without this the field would be that fence with a hole in it:
// any package could opt its store into following by setting one field, and
// nothing would notice.
//
// config/ is where the policy and the writer live. task/ is the one store the
// decision was made FOR (#3672 for its ordinary writes, #3718 for the
// write-back). Anything else belongs on #3672's table first.
func TestFollowLinkPolicyStaysWithTheStoresThatAskedForIt(t *testing.T) {
	const followPolicy = "SchemaWriteFollowLink"

	root, err := filepath.Abs("..")
	require.NoError(t, err)
	allowed := map[string]bool{
		filepath.Join(root, "config"): true,
		filepath.Join(root, "task"):   true,
	}

	var outside []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// Skip only trees that cannot hold Go by construction; the .go
			// suffix below is what decides, so no package name can earn an
			// exception here.
			switch d.Name() {
			case ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || allowed[filepath.Dir(path)] {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), followPolicy) {
			rel, _ := filepath.Rel(root, path)
			outside = append(outside, rel)
		}
		return nil
	})
	require.NoError(t, err)

	assert.Empty(t, outside,
		"%s routes the schema-migration write-back through AtomicWriteFileFollowingLink "+
			"without naming it, so setting it is the same decision the writer's own fence "+
			"guards (#3660, #3672, #3718).\n"+
			"A store af MANAGES must keep the replacing writer: following means \"af will "+
			"rewrite whatever this points at\", a promise none of those callers offered.\n"+
			"If a new store genuinely is user-authored, say so on #3672 and add its directory here.",
		followPolicy)
}
