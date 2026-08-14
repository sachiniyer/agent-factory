package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registeredTestProject sets an isolated AF home, initializes a git repo, and
// registers it, returning the home, the canonical repo root, and the project.
func registeredTestProject(t *testing.T) (home, repoRoot string, project Project) {
	t.Helper()
	base := t.TempDir()
	home = filepath.Join(base, "af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoRoot = initProjectRegistryRepo(t, filepath.Join(base, "repo"))
	p, err := RegisterProject(repoRoot)
	require.NoError(t, err)
	return home, repoRoot, p
}

// writePersonalConfig writes raw TOML directly into a project's personal config
// file, bypassing the write path so loader/edge behavior can be exercised
// independently of SetProjectConfigValue.
func writePersonalConfig(t *testing.T, id, content string) string {
	t.Helper()
	path, err := ProjectConfigTomlPath(id)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestProjectConfigTomlPathValidatesID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	id := "prj_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	path, err := ProjectConfigTomlPath(id)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ProjectRegistryDirName, id, TomlConfigFileName), path)

	_, err = ProjectConfigTomlPath("not-a-project-id")
	require.Error(t, err, "an invalid id must never resolve to a path component")
}

func TestLoadProjectConfigAbsentIsNoLayer(t *testing.T) {
	_, _, project := registeredTestProject(t)
	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	require.Nil(t, cfg, "a project with no personal file contributes no layer")
}

func TestLoadProjectConfigParsesAllowedKeys(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, `default_program = "codex"
branch_prefix = "feat/"

[program_overrides]
claude = "/usr/local/bin/claude --verbose"
`)
	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "codex", cfg.DefaultProgram)
	assert.Equal(t, "feat/", cfg.BranchPrefix)
	assert.Equal(t, "/usr/local/bin/claude --verbose", cfg.ProgramOverrides["claude"])
	assert.True(t, cfg.IsSet("default_program"))
	assert.True(t, cfg.IsSet("branch_prefix"))
	assert.True(t, cfg.IsSet("program_overrides"))
	assert.False(t, cfg.IsSet("worktree_root"))
}

func TestLoadProjectConfigEmptyFileIsError(t *testing.T) {
	for name, content := range map[string]string{
		"whitespace-only": "   \n",
		"comment-only":    "# no personal overrides yet\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, project := registeredTestProject(t)
			writePersonalConfig(t, project.ID, content)
			_, err := LoadProjectConfig(project.ID)
			require.Error(t, err)
			require.Contains(t, err.Error(), "empty")
		})
	}
}

func TestLoadProjectConfigRejectsGlobalOnlyKey(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "listen_addr = \"0.0.0.0:8443\"\n")
	_, err := LoadProjectConfig(project.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "listen_addr")
	require.Contains(t, err.Error(), "cannot be set per project")
}

func TestLoadProjectConfigRejectsRepoContractKey(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "backend = \"docker\"\n")
	_, err := LoadProjectConfig(project.ID)
	require.Error(t, err, "a repo-contract key never admits the personal layer")
	require.Contains(t, err.Error(), "backend")
}

func TestLoadProjectConfigRejectsInvalidProgramEnum(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "default_program = \"not-an-agent\"\n")
	_, err := LoadProjectConfig(project.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "default_program")
}

// TestLoadProjectConfigAllowsCloudSelector pins the deliberate divergence from
// the in-repo loader: a machine-local, user-owned personal file may carry a
// cloud-credential env-assignment in a program_overrides value, exactly as the
// global config may. Only a checked-in in-repo file is refused, because that is
// the file a cloned repository could weaponize.
func TestLoadProjectConfigAllowsCloudSelector(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"CLAUDE_CODE_USE_BEDROCK=1 claude\"\n")
	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err, "a personal file is the user's own, like the global config")
	require.NotNil(t, cfg)
	assert.Equal(t, "CLAUDE_CODE_USE_BEDROCK=1 claude", cfg.ProgramOverrides["claude"])
}

func TestResolveProjectSelectorByID(t *testing.T) {
	_, _, project := registeredTestProject(t)
	got, err := ResolveProjectSelector(project.ID)
	require.NoError(t, err)
	assert.Equal(t, project.ID, got.ID)
}

func TestResolveProjectSelectorByPathAndSubdir(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	got, err := ResolveProjectSelector(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, project.ID, got.ID)

	nested := filepath.Join(repoRoot, "services", "api")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	fromSub, err := ResolveProjectSelector(nested)
	require.NoError(t, err, "a subdirectory selects the whole project")
	assert.Equal(t, project.ID, fromSub.ID)
}

func TestResolveProjectSelectorUnknownID(t *testing.T) {
	registeredTestProject(t)
	_, err := ResolveProjectSelector("prj_ffffffffffffffffffffffffffffffff")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no registered project has id")
}

func TestResolveProjectSelectorUnregisteredPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	unregistered := initProjectRegistryRepo(t, filepath.Join(base, "loose-repo"))
	_, err := ResolveProjectSelector(unregistered)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a registered project")
	require.Contains(t, err.Error(), "af projects register", "the error must name the real registration command")
}

func TestResolveProjectSelectorNonGitPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	plain := filepath.Join(base, "plain-dir")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	_, err := ResolveProjectSelector(plain)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not inside a git repository")
}

func TestProjectForRootMatchesRegisteredRoot(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	got, found, err := projectForRoot(repoRoot)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, project.ID, got.ID)
}

func TestProjectForRootUnregisteredIsNotFound(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	repoRoot := initProjectRegistryRepo(t, filepath.Join(base, "repo"))
	_, found, err := projectForRoot(repoRoot)
	require.NoError(t, err)
	require.False(t, found, "an unregistered repo has no personal layer")
}

func TestProjectForRepoBoundsUnrelatedRegistryIdentityProbe(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	runProjectRegistryGit(t, base, "clone", "--bare", seed, bare)
	runProjectRegistryGit(t, bare, "worktree", "add", first)
	runProjectRegistryGit(t, bare, "worktree", "add", "-b", "second", second)
	registered, err := RegisterProject(first)
	require.NoError(t, err)
	target, err := RepoFromPath(second)
	require.NoError(t, err)

	registryDir, err := projectRegistryDir()
	require.NoError(t, err)
	staleRoot := filepath.Join(base, "stalled")
	require.NoError(t, os.MkdirAll(staleRoot, 0o755))
	stale := projectRecord{
		SchemaVersion: projectRegistrySchemaVersion,
		ID:            "prj_00000000000000000000000000000000",
		CheckoutID:    "chk_00000000000000000000000000000000",
		Root:          staleRoot,
		CheckoutRoot:  staleRoot,
		RelativeRoot:  ".",
	}
	require.NoError(t, os.MkdirAll(filepath.Join(registryDir, stale.ID), 0o755))
	require.NoError(t, writeProjectRecord(registryDir, stale))

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "git")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\ncase \" $* \" in\n  *\"$AF_STALLED_ROOT\"*) /bin/sleep 3; exit 1 ;;\nesac\nexec \"$AF_REAL_GIT\" \"$@\"\n"), 0o755))
	t.Setenv("AF_STALLED_ROOT", staleRoot)
	t.Setenv("AF_REAL_GIT", realGit)
	t.Setenv("PATH", binDir)

	started := time.Now()
	got, found, err := projectForRepo(target)
	elapsed := time.Since(started)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, registered.ID, got.ID)
	assert.Less(t, elapsed, 2*time.Second,
		"one unrelated stalled registry root must not block config resolution for a healthy sibling")
}

func TestProjectForRepoRejectsRegisteredRootAdoptedByAncestor(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	outer := initProjectRegistryRepo(t, filepath.Join(base, "outer"))
	nested := initProjectRegistryRepo(t, filepath.Join(outer, "nested"))
	registered, err := RegisterProject(nested)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(nested, ".git")))
	target, err := RepoFromPath(outer)
	require.NoError(t, err)

	got, found, err := projectForRepo(target)
	require.NoError(t, err)
	assert.False(t, found,
		"a stale nested registration must not lend its personal config to the ancestor repository")
	assert.NotEqual(t, registered.ID, got.ID)
}
