package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
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

func TestCheckoutMarkerCompletedFailureWinsOverExpiredContext(t *testing.T) {
	completed := exec.Command("sh", "-c", "exit 7").Run()
	require.Error(t, completed)
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkoutMarkerProbeFailure(parent, t.TempDir(), completed)
	require.Error(t, err, "a completed nonzero Git exit did not establish checkout-marker absence")
	require.NotErrorIs(t, err, context.Canceled,
		"the completed Git result must still win when the caller deadline lands before classification")
	require.Contains(t, err.Error(), "exit status 7")
}

func TestCheckoutMarkerTimeoutReusesParkedRead(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	markerName, err := checkoutMarkerName()
	require.NoError(t, err)
	marker := filepath.Join(repoRoot, ".git", checkoutMarkerDirName, markerName)
	markerData, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.NoError(t, os.Remove(marker))
	require.NoError(t, syscall.Mkfifo(marker, 0o600))

	var releaseOnce sync.Once
	releaseDone := make(chan error, 1)
	released := false
	release := func() {
		releaseOnce.Do(func() {
			go func() { releaseDone <- os.WriteFile(marker, markerData, 0o600) }()
		})
	}
	t.Cleanup(func() {
		if !released {
			release()
			<-releaseDone
		}
		_ = os.Remove(marker)
		_ = os.WriteFile(marker, markerData, 0o600)
	})

	_, _, firstErr := checkoutIDForWorkspaceContext(context.Background(), repoRoot)
	require.Error(t, firstErr)
	first := CheckoutMarkerProbeCompletion(firstErr)
	require.NotNil(t, first, "the timed-out os.ReadFile must expose its actual lifetime")
	_, _, secondErr := checkoutIDForWorkspaceContext(context.Background(), repoRoot)
	require.Error(t, secondErr)
	second := CheckoutMarkerProbeCompletion(secondErr)
	require.NotNil(t, second)
	require.Equal(t, first, second, "a second probe must join the parked marker read")

	release()
	releaseErr := <-releaseDone
	released = true
	require.NoError(t, releaseErr)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("checkout-marker read did not complete after its mount became responsive")
	}
	require.NoError(t, os.Remove(marker))
	require.NoError(t, os.WriteFile(marker, markerData, 0o600))

	got, found, err := checkoutIDForWorkspaceContext(context.Background(), repoRoot)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, project.CheckoutID, got, "a completed flight must release the path for a fresh healthy read")
}

func TestRootAgentInspectionPropagatesCompletedCheckoutProbeFailure(t *testing.T) {
	_, repoRoot, _ := registeredTestProject(t)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$3\" = rev-parse ] && [ \"$4\" = --git-common-dir ] && [ \"$#\" -eq 4 ]; then exit 7; fi\nexec %q \"$@\"\n", realGit)
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = ResolveRootAgentForInspectionWithConfigContext(ctx, cfg, repoRoot, false)
	require.Error(t, err, "a failed checkout probe must not read as an absent personal layer")
	require.Contains(t, err.Error(), "exit status 7")
	require.NotErrorIs(t, err, context.DeadlineExceeded)
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

func TestResolveProjectSelectorByLinkedWorktreeIdentity(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	firstRoot := filepath.Join(base, "first-worktree")
	secondRoot := filepath.Join(base, "second-worktree")
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", firstRoot)
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", secondRoot)

	first, err := RegisterProject(firstRoot)
	require.NoError(t, err)
	second, err := RegisterProject(secondRoot)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)

	resolved, err := ResolveProjectSelector(secondRoot)
	require.NoError(t, err)
	require.Equal(t, first.ID, resolved.ID)

	personal, err := SetProjectConfigValue(secondRoot, "root_agent", `{"enabled":true}`)
	require.NoError(t, err)
	require.Equal(t, first.ID, filepath.Base(filepath.Dir(personal.Path)))
	config, err := LoadProjectConfig(first.ID)
	require.NoError(t, err)
	require.NotNil(t, config)
	require.True(t, config.RootAgentLayer().Value.Enabled)

	unrelated := initProjectRegistryRepo(t, filepath.Join(base, "unrelated"))
	_, err = ResolveProjectSelector(unrelated)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a registered project")
}

func TestResolveProjectSelectorRejectsCopiedCheckoutMarker(t *testing.T) {
	_, original, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "default_program", "codex")
	require.NoError(t, err)
	personalPath, err := ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	before, err := os.ReadFile(personalPath)
	require.NoError(t, err)

	copyRoot := filepath.Join(t.TempDir(), "copy")
	require.NoError(t, exec.Command("cp", "-R", original, copyRoot).Run())

	_, err = ResolveProjectSelector(copyRoot)
	require.Error(t, err)
	require.Contains(t, err.Error(), "checkout marker "+project.CheckoutID+" appears at both")
	require.Contains(t, err.Error(), "move or remove one copy; af will not choose between them")

	_, err = SetProjectConfigValue(copyRoot, "root_agent", `{"enabled":true}`)
	require.Error(t, err)
	after, readErr := os.ReadFile(personalPath)
	require.NoError(t, readErr)
	require.Equal(t, before, after)

	exact, err := ResolveProjectSelector(original)
	require.NoError(t, err)
	require.Equal(t, project.ID, exact.ID)
}

// TestResolveProjectSelectorRejectsReplacedMarkerAtRegisteredRoot pins the fix
// for the CLI/daemon resolution split: #3361 made the daemon's projectForRoot
// marker-first while ResolveProjectSelector stayed path-first. When a
// registered path's checkout marker is replaced by another project's marker,
// the CLI write path must refuse with a rebind instruction rather than
// silently write a personal override the daemon never applies there.
func TestResolveProjectSelectorRejectsReplacedMarkerAtRegisteredRoot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	barRoot := initProjectRegistryRepo(t, filepath.Join(base, "bar"))
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	// Give bar a real personal override so the daemon's marker-first resolver
	// would otherwise load it for a session at /foo.
	_, err = SetProjectConfigValue(bar.ID, "default_program", "codex")
	require.NoError(t, err)
	barPersonalPath, err := ProjectConfigTomlPath(bar.ID)
	require.NoError(t, err)
	barBefore, err := os.ReadFile(barPersonalPath)
	require.NoError(t, err)
	fooPersonalPath, err := ProjectConfigTomlPath(foo.ID)
	require.NoError(t, err)

	// Replace foo's marker with bar's at foo's registered root.
	markerName, err := checkoutMarkerName()
	require.NoError(t, err)
	barMarker, err := os.ReadFile(filepath.Join(barRoot, ".git", checkoutMarkerDirName, markerName))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(fooRoot, ".git", checkoutMarkerDirName, markerName), barMarker, 0o644))

	// The daemon's marker-first resolver now names bar for /foo — the
	// disagreement the bug report documents.
	viaMarker, found, err := projectForRoot(fooRoot)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, bar.ID, viaMarker.ID, "daemon resolver: marker-first returns bar")

	// The CLI write path must now refuse instead of returning foo by path
	// alone, reconciling with the registry's RegisterProject contract.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has marker "+bar.CheckoutID+" instead of "+foo.CheckoutID)
	// The marker belongs to bar, so a naive `af projects rebind foo foo-root`
	// would be rejected by RebindProject (the marker is claimed by another
	// record); the error must tell the user to remove the copied marker first.
	assert.Contains(t, err.Error(), "remove the copied checkout marker at ")
	// The recovery command must quote the path so a path with whitespace or
	// shell metacharacters pastes as one argument.
	assert.Contains(t, err.Error(), "run `af projects rebind "+foo.ID+" "+ShellQuotePath(fooRoot)+"`")

	// SetProjectConfigValue goes through ResolveProjectSelector, so it must
	// surface the same refusal and write neither project's personal file.
	_, err = SetProjectConfigValue(fooRoot, "default_program", "codex")
	require.Error(t, err)
	_, fooStatErr := os.Stat(fooPersonalPath)
	assert.ErrorIs(t, fooStatErr, os.ErrNotExist,
		"the refused write must not create foo's personal file")
	barAfter, err := os.ReadFile(barPersonalPath)
	require.NoError(t, err)
	assert.Equal(t, barBefore, barAfter, "the refused write must not touch bar's personal file")

	// UnsetProjectConfigValue is wired through ResolveProjectSelector too, so
	// it must refuse for the same root.
	_, err = UnsetProjectConfigValue(fooRoot, "default_program")
	require.Error(t, err)
	barAfter2, err := os.ReadFile(barPersonalPath)
	require.NoError(t, err)
	assert.Equal(t, barBefore, barAfter2, "the refused unset must not touch bar's personal file")
}

// TestResolveProjectSelectorRejectsAbsentMarkerAtRegisteredRoot pins the
// marker-absent half of the fix: a re-clone at the registered path leaves no
// checkout marker, and the CLI write path must refuse with a rebind
// instruction rather than write an override the daemon's marker-first
// resolver (which returns not-found) never reads there.
func TestResolveProjectSelectorRejectsAbsentMarkerAtRegisteredRoot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	// Set up an existing personal file via the id selector (which bypasses
	// marker validation by design) so the refused path-selector write can be
	// checked for "no mutation".
	_, err = SetProjectConfigValue(foo.ID, "default_program", "codex")
	require.NoError(t, err)
	fooPersonalPath, err := ProjectConfigTomlPath(foo.ID)
	require.NoError(t, err)
	fooBefore, err := os.ReadFile(fooPersonalPath)
	require.NoError(t, err)

	// Delete the marker — a fresh `git clone` at /foo would leave no
	// af-authored marker.
	markerName, err := checkoutMarkerName()
	require.NoError(t, err)
	markerPath := filepath.Join(fooRoot, ".git", checkoutMarkerDirName, markerName)
	require.NoError(t, os.Remove(markerPath))

	// The daemon's marker-first resolver returns not-found.
	_, found, err := projectForRoot(fooRoot)
	require.NoError(t, err)
	assert.False(t, found, "daemon resolver: marker absent means /foo is no longer recognized")

	// The CLI write path must refuse instead of returning foo by path alone.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has no checkout marker")
	assert.Contains(t, err.Error(), "run `af projects rebind "+foo.ID+" "+ShellQuotePath(fooRoot)+"`")

	// SetProjectConfigValue and UnsetProjectConfigValue go through
	// ResolveProjectSelector, so both must refuse and leave the file unchanged.
	_, err = SetProjectConfigValue(fooRoot, "branch_prefix", "feat/")
	require.Error(t, err)
	fooAfterSet, err := os.ReadFile(fooPersonalPath)
	require.NoError(t, err)
	assert.Equal(t, fooBefore, fooAfterSet, "the refused set must leave foo's personal file unchanged")

	_, err = UnsetProjectConfigValue(fooRoot, "default_program")
	require.Error(t, err)
	fooAfterUnset, err := os.ReadFile(fooPersonalPath)
	require.NoError(t, err)
	assert.Equal(t, fooBefore, fooAfterUnset, "the refused unset must leave foo's personal file unchanged")
}

// TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarker pins the
// linked-worktree half of the claimed-marker case. The claimed-marker branch
// only reaches the OWNER's own shared marker (not a private cp -R copy on a
// private .git) when the check wins on a linked-worktree-of-a-BARE-repo
// shape: there, binding.root resolves to the worktree root, while
// binding.gitCommonDir resolves to the bare common dir, so the marker file
// at binding.checkoutMarkerPath is the same one project bar's own
// registration wrote — removing it here would delete it for every worktree
// sharing that bare dir, and a rebind here would leave bar's record
// referencing a checkout ID the marker no longer carries. af must refuse
// with a disruption message that does NOT recommend removing the marker,
// rather than call it a copied one.
func TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarker(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	// Register bar at a linked worktree of the bare: a worktree of a bare
	// repo resolves binding.root to the worktree path and binding.gitCommonDir
	// to the bare, so the marker the bare shares is bar's registry marker.
	barRoot := filepath.Join(base, "bar")
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", barRoot)
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	// Give bar a real personal override so the refused write can be checked
	// for "no mutation".
	_, err = SetProjectConfigValue(bar.ID, "default_program", "codex")
	require.NoError(t, err)
	barPersonalPath, err := ProjectConfigTomlPath(bar.ID)
	require.NoError(t, err)
	barBefore, err := os.ReadFile(barPersonalPath)
	require.NoError(t, err)
	fooPersonalPath, err := ProjectConfigTomlPath(foo.ID)
	require.NoError(t, err)

	// Replace foo's registered root with another linked worktree of the
	// same bare. /foo's new binding.root stays /foo (worktree root of a
	// bare), while binding.gitCommonDir is the bare; binding.checkoutMarkerPath
	// is the same marker file bar's registration wrote, so the marker it
	// carries is bar's checkout ID (not foo's).
	require.NoError(t, os.RemoveAll(fooRoot))
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", fooRoot)
	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.True(t, projectRootUsesGitCommonDir(bar.Root, binding.gitCommonDir),
		"binding for /foo should share the bare git common directory bar uses")
	markerID, _, err := readCheckoutID(binding.checkoutMarkerPath)
	require.NoError(t, err)
	require.Equal(t, bar.CheckoutID, markerID,
		"the marker at /foo's binding path is bar's own record marker")

	// The daemon's marker-first resolver names bar for /foo — the
	// disagreement the bug report documents: the CLI write path must not
	// also write foo's personal override for the session af treats as bar.
	viaMarker, found, err := projectForRoot(fooRoot)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, bar.ID, viaMarker.ID, "daemon resolver: marker-first returns bar")

	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has marker "+bar.CheckoutID+" instead of "+foo.CheckoutID)
	assert.Contains(t, err.Error(), "linked worktree")
	// The shared-marker case must NOT recommend removing the marker file:
	// doing so would delete bar's own marker for every one of the worktrees
	// that share the bare's git directory.
	assert.NotContains(t, err.Error(), "remove the copied checkout marker")

	// The write path goes through ResolveProjectSelector, so it must
	// surface the same refusal and touch neither project's personal file.
	_, err = SetProjectConfigValue(fooRoot, "default_program", "codex")
	require.Error(t, err)
	_, fooStatErr := os.Stat(fooPersonalPath)
	assert.ErrorIs(t, fooStatErr, os.ErrNotExist,
		"the refused write must not create foo's personal file")
	barAfter, err := os.ReadFile(barPersonalPath)
	require.NoError(t, err)
	assert.Equal(t, barBefore, barAfter, "the refused write must not touch bar's personal file")
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
	require.Contains(t, err.Error(), "af projects add", "the error must name the real registration command")
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

func TestProjectLookupMarkerProbeTimeoutIsUnknown(t *testing.T) {
	_, repoRoot, _ := registeredTestProject(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	script := "#!/bin/sh\nsleep 1\nexec \"" + realGit + "\" \"$@\"\n"
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, found, err := projectForWorkspaceContext(context.Background(), repoRoot)
	require.Error(t, err, "a timed-out checkout-marker probe is unknown, not unregistered")
	require.False(t, found)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestProjectConfigLockRefusesCallbackAfterTransientMarkerFailure(t *testing.T) {
	_, repoRoot, _ := registeredTestProject(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	countPath := filepath.Join(shimDir, "marker-count")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$3" = "rev-parse" ] && [ "$4" = "--git-common-dir" ]; then
  if [ ! -e %q ]; then
    : > %q
    exit 7
  fi
fi
exec %q "$@"
`, countPath, countPath, realGit)
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	called := false
	err = WithProjectConfigLockForRoot(repoRoot, func() error {
		called = true
		_, found, lookupErr := projectForRoot(repoRoot)
		require.NoError(t, lookupErr, "the later policy read reproduces after the transient probe failure")
		require.True(t, found)
		return nil
	})
	require.Error(t, err, "an unknown identity cannot authorize an unlocked mutating callback")
	require.False(t, called, "the callback must wait for a proven project-config lock")
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
	probeMarker := filepath.Join(t.TempDir(), "stale-root-probed")
	wrapper := filepath.Join(binDir, "git")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\ncase \" $* \" in\n  *\"$AF_STALLED_ROOT\"*) : > \"$AF_STALLED_PROBE_MARKER\"; /bin/sleep 3; exit 1 ;;\nesac\nexec \"$AF_REAL_GIT\" \"$@\"\n"), 0o755))
	t.Setenv("AF_STALLED_ROOT", staleRoot)
	t.Setenv("AF_STALLED_PROBE_MARKER", probeMarker)
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
	_, statErr := os.Stat(probeMarker)
	assert.ErrorIs(t, statErr, os.ErrNotExist,
		"config resolution must identify the target marker without probing unrelated registered roots")
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

func TestProjectForRepoRejectsReplacementAtRegisteredPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	root := initProjectRegistryRepo(t, filepath.Join(base, "repo"))
	registered, err := RegisterProject(root)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(root, ".git")))
	initProjectRegistryRepo(t, root)
	replacement, err := RepoFromPath(root)
	require.NoError(t, err)

	got, found, err := projectForRepo(replacement)
	require.NoError(t, err)
	assert.False(t, found,
		"path equality is not identity proof when the registered checkout marker is gone")
	assert.NotEqual(t, registered.ID, got.ID)
}
