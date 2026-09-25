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

// TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerOwnerUnresolvable
// pins the unresolvable-owner half of the claimed-marker case. The
// shared-marker branch is reached by matching the marker's identity to a
// registered owner and confirming the owner's root shares this checkout's
// git common directory; projectRootUsesGitCommonDir suppresses resolution
// errors, so when the owner's root has been removed, renamed, or is
// temporarily unresolvable while other linked worktrees still share the
// same bare common directory, the check returns false and the prior code
// fell through to the "remove the copied checkout marker" remedy — even
// though the marker at this binding's path is still the owner's own
// shared registry marker. Following that advice would break identity
// resolution for every remaining worktree and leave the owner's record
// stale. af must refuse without naming the marker private or recommending
// its deletion, naming the owner whose root could not be resolved.
func TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerOwnerUnresolvable(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	// Register bar at a linked worktree of the bare: a worktree of a bare
	// repo resolves binding.root to the worktree path and
	// binding.gitCommonDir to the bare, so the marker the bare shares is
	// bar's registry marker.
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
	// same bare. /foo's new binding.checkoutMarkerPath is the same shared
	// marker file bar's registration wrote, so it carries bar's checkout
	// ID (not foo's).
	require.NoError(t, os.RemoveAll(fooRoot))
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", fooRoot)
	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	markerID, _, err := readCheckoutID(binding.checkoutMarkerPath)
	require.NoError(t, err)
	require.Equal(t, bar.CheckoutID, markerID,
		"the marker at /foo's binding path is bar's own record marker")

	// Remove bar's registered root so the marker owner is unresolvable:
	// projectRootUsesGitCommonDir(bar.Root, ...) would suppress the
	// resolution error and return false, falling through to the
	// remove-the-copied-marker remedy without this fix.
	require.NoError(t, os.RemoveAll(barRoot))
	_, ownerUnresolvableErr := resolveProjectBinding(bar.Root)
	require.Error(t, ownerUnresolvableErr,
		"bar's root should no longer resolve after removal")

	// The CLI write path must refuse instead of recommending deletion of
	// the still-shared marker, naming bar's id and the unresolvable root.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has marker "+bar.CheckoutID+" instead of "+foo.CheckoutID)
	assert.Contains(t, err.Error(), "could not be resolved")
	assert.Contains(t, err.Error(), bar.ID)
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

// TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerOwnerReplaced
// pins the resolvable-replacement half of the claimed-marker case. Like the
// unresolvable-owner case, the marker at /foo's binding path is still bar's
// own shared marker through the bare — but here bar's recorded root has been
// replaced by another valid repository rather than removed.
// resolveProjectBinding(bar.Root) succeeds (no error) and returns a different
// git common directory, so the unresolvable-owner guard does not fire; without
// this fix the common-directory mismatch fell through to the "remove the
// copied checkout marker" remedy, recommending the user delete a marker that
// may still be bar's own shared registry marker. The recorded root is only
// last-known, so a common-directory mismatch justifies deletion only after
// the owner root has proven its recorded marker; af must refuse without
// naming the marker private or recommending its deletion, naming the owner
// whose recorded root no longer carries its marker.
func TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerOwnerReplaced(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	// Register bar at a linked worktree of the bare: a worktree of a bare
	// repo resolves binding.root to the worktree path and
	// binding.gitCommonDir to the bare, so the marker the bare shares is
	// bar's registry marker.
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
	// same bare. /foo's new binding.checkoutMarkerPath is the same shared
	// marker file bar's registration wrote, so it carries bar's checkout
	// ID (not foo's).
	require.NoError(t, os.RemoveAll(fooRoot))
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", fooRoot)
	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	markerID, _, err := readCheckoutID(binding.checkoutMarkerPath)
	require.NoError(t, err)
	require.Equal(t, bar.CheckoutID, markerID,
		"the marker at /foo's binding path is bar's own record marker")

	// Replace bar's registered root with a fresh independent repository so
	// the owner root resolves successfully to a different git common
	// directory (the "replaced marker owner" case): the unresolvable-owner
	// guard does not fire, and without this fix the common-directory
	// mismatch fell through to the remove-the-copied-marker remedy. The
	// fresh repo has no agent-factory marker, so its marker slot no longer
	// carries bar's recorded checkout id — the recorded root is no longer
	// authoritative and the marker at /foo may still be bar's own shared
	// marker through this checkout's bare.
	require.NoError(t, os.RemoveAll(barRoot))
	initProjectRegistryRepo(t, barRoot)
	ownerBinding, err := resolveProjectBinding(bar.Root)
	require.NoError(t, err, "bar's replaced root must resolve (the resolvable-replacement case)")
	require.False(t, sameProjectPath(ownerBinding.gitCommonDir, binding.gitCommonDir),
		"bar's replaced root must resolve to a different git common directory than /foo's bare")
	ownerMarkerID, ownerMarkerExists, err := readCheckoutID(ownerBinding.checkoutMarkerPath)
	require.NoError(t, err)
	require.False(t, ownerMarkerExists,
		"bar's replaced root carries no agent-factory marker")
	require.Empty(t, ownerMarkerID)

	// The CLI write path must refuse instead of recommending deletion of the
	// still-shared marker, naming bar's id and the replaced root.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has marker "+bar.CheckoutID+" instead of "+foo.CheckoutID)
	assert.Contains(t, err.Error(), "no longer carries project "+bar.ID+"'s checkout marker")
	assert.Contains(t, err.Error(), bar.ID)
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

// TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerOwnerReplacedWithRetainedMarker
// pins the retained-marker half of the resolvable-replacement case. Like the
// resolvable-replacement case, the marker at /foo's binding path is bar's own
// shared marker through the bare, and bar's recorded root has been replaced by
// a fresh repository that resolves to a different git common directory — but
// here the replacement RETAINED bar's checkout marker, so the new owner-marker
// check passes (ownerBinding carries owner.CheckoutID). Matching
// owner.CheckoutID cannot distinguish which of the two duplicated markers is
// the copy: the marker at /foo is in the bare's shared directory (this checkout
// is a linked worktree), so it may be bar's own shared marker rather than a
// private cp -R copy. af must refuse without recommending deletion, instead
// of falling through to the "remove the copied checkout marker" remedy.
func TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerOwnerReplacedWithRetainedMarker(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	// Register bar at a linked worktree of the bare: a worktree of a bare
	// repo resolves binding.root to the worktree path and
	// binding.gitCommonDir to the bare, so the marker the bare shares is
	// bar's registry marker.
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
	// same bare. /foo's new binding.root stays /foo (a linked worktree),
	// binding.gitCommonDir is the bare, and binding.checkoutMarkerPath is
	// the same shared marker file bar's registration wrote, so it carries
	// bar's checkout ID (not foo's).
	require.NoError(t, os.RemoveAll(fooRoot))
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", fooRoot)
	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.True(t, sharedWorktreeCommonDir(binding.root, binding.gitCommonDir),
		"/foo is a linked worktree whose git common directory is the shared bare")
	markerID, _, err := readCheckoutID(binding.checkoutMarkerPath)
	require.NoError(t, err)
	require.Equal(t, bar.CheckoutID, markerID,
		"the marker at /foo's binding path is bar's own shared record marker")

	// Replace bar's registered root with a fresh main checkout that RETAINS
	// bar's checkout marker: resolveProjectBinding(bar.Root) succeeds and
	// returns a different git common directory than the bare (the
	// resolvable-replacement case), and ownerBinding.checkoutMarkerPath
	// carries bar.CheckoutID — so the new owner-marker check passes. Without
	// the linked-worktree guard, matching bar.CheckoutID would fall through
	// to the "remove the copied checkout marker" remedy even though /foo's
	// marker is in the bare's shared directory and may be bar's own.
	require.NoError(t, os.RemoveAll(barRoot))
	initProjectRegistryRepo(t, barRoot)
	ownerBinding, err := resolveProjectBinding(bar.Root)
	require.NoError(t, err, "bar's replaced root must resolve (the retained-marker case)")
	require.False(t, sameProjectPath(ownerBinding.gitCommonDir, binding.gitCommonDir),
		"bar's replaced root must resolve to a different git common directory than /foo's bare")
	// Write bar's checkout id into the replaced root's marker slot so the
	// owner-marker check matches (retained-marker shape).
	require.NoError(t, os.MkdirAll(filepath.Dir(ownerBinding.checkoutMarkerPath), 0o755))
	require.NoError(t, os.WriteFile(ownerBinding.checkoutMarkerPath, []byte(bar.CheckoutID), 0o644))
	retainedID, retainedExists, err := readCheckoutID(ownerBinding.checkoutMarkerPath)
	require.NoError(t, err)
	require.True(t, retainedExists, "bar's replaced root retains a matching marker")
	require.Equal(t, bar.CheckoutID, retainedID)

	// The CLI write path must refuse instead of recommending deletion of the
	// still-shared marker: matching bar.CheckoutID cannot tell which of the
	// two duplicated markers is the copy, and /foo's lives in the bare's
	// shared directory.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has marker "+bar.CheckoutID+" instead of "+foo.CheckoutID)
	assert.Contains(t, err.Error(), "linked worktree")
	assert.Contains(t, err.Error(), bar.ID)
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

// TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerMainWithWorktrees
// pins the main-with-worktrees half of the retained-marker case. Like the
// linked-worktree case above, the marker at /foo's binding path belongs to
// bar and bar's recorded root still carries a matching marker; but here
// /foo is a MAIN checkout whose git common directory is its own
// <root>/.git, so sharedWorktreeCommonDir returns false. The marker is still
// not provably private: /foo has spawned linked worktrees of its own, and
// git stores each one under <commonDir>/worktrees, so the .git directory (and
// the marker in it) is shared with every one of them. af must refuse without
// recommending deletion, instead of falling through to the "remove the
// copied checkout marker" remedy.
func TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerMainWithWorktrees(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	// Register bar at a linked worktree of the bare so bar's own marker
	// lives in the shared bare directory.
	barRoot := filepath.Join(base, "bar")
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", barRoot)
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	// Register foo at an independent main checkout whose <root>/.git is
	// its own. mainCheckoutHasLinkedWorktrees must be the only thing that
	// catches it, so this root starts out privately owned.
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	runProjectRegistryGit(t, fooRoot, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, fooRoot, "config", "user.name", "Test")
	runProjectRegistryGit(t, fooRoot, "commit", "--quiet", "--allow-empty", "-m", "initial")
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	// Spawn a linked worktree from /foo so /foo/.git/worktrees/<name> is
	// non-empty: sharedWorktreeCommonDir returns false here (the common dir
	// lives inside /foo), but the marker at /foo/.git is still shared with
	// every worktree git created from /foo.
	fooSibling := filepath.Join(base, "foo-sibling")
	runProjectRegistryGit(t, fooRoot, "worktree", "add", "--quiet", "--detach", fooSibling)
	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.False(t, sharedWorktreeCommonDir(binding.root, binding.gitCommonDir),
		"/foo is a main checkout whose git common directory lives inside the root")
	require.True(t, mainCheckoutHasLinkedWorktrees(binding.gitCommonDir),
		"/foo's .git must contain a linked worktree entry from `git worktree add`")
	ownerBinding, err := resolveProjectBinding(bar.Root)
	require.NoError(t, err)
	require.False(t, sameProjectPath(ownerBinding.gitCommonDir, binding.gitCommonDir),
		"/bar resolves to a different git common directory than /foo")

	// Replace foo's marker with bar's checkout id, mimicking the claimed
	// marker case where this binding's marker belongs to another project.
	require.NoError(t, os.MkdirAll(filepath.Dir(binding.checkoutMarkerPath), 0o755))
	require.NoError(t, os.WriteFile(binding.checkoutMarkerPath, []byte(bar.CheckoutID), 0o644))

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

	// The CLI write path must refuse without recommending removal: /foo is
	// not a linked worktree, but it has spawned linked worktrees of its own,
	// so the marker at /foo/.git is shared with them and may be bar's own
	// shared marker rather than a private copy af can ask the user to delete.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has marker "+bar.CheckoutID+" instead of "+foo.CheckoutID)
	assert.Contains(t, err.Error(), "main working tree that has spawned linked worktrees")
	assert.Contains(t, err.Error(), bar.ID)
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

// TestResolveProjectSelectorRejectsMissingMarkerSharedCommonDir pins the
// absent-marker half of the shared-common-directory case. When the marker on
// the shared directory is deleted, ResolveProjectSelector's !markerExists
// branch used to recommend `af projects rebind <p> <binding.root>`; but
// RebindProject calls ensureCheckoutID, which writes a new marker at the
// shared path and rejects only records whose CheckoutID matches the new one
// (project_registry.go:293-303), so the suggested command succeeds only by
// reattributing every worktree sharing that directory to p while leaving the
// original owner's record stale. With the fix the write path refuses the
// rebind advice and names the other registered project sharing the directory.
func TestResolveProjectSelectorRejectsMissingMarkerSharedCommonDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	// Register bar at a linked worktree of the bare: a worktree of a bare
	// repo shares the bare git common directory, and the bare's checkout
	// marker is bar's own registry marker.
	barRoot := filepath.Join(base, "bar")
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", barRoot)
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	// Replace foo's registered root with another linked worktree of the
	// same bare so /foo's binding.root stays /foo (a worktree root of a
	// bare) while binding.gitCommonDir switches to the bare bar shares.
	require.NoError(t, os.RemoveAll(fooRoot))
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", fooRoot)
	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.True(t, projectRootUsesGitCommonDir(bar.Root, binding.gitCommonDir),
		"/bar must share the bare git common directory /foo now resolves to")

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

	// Strip the shared marker on the bare: foo's binding now reads an
	// absent marker, falling into the !markerExists branch.
	require.NoError(t, os.Remove(binding.checkoutMarkerPath))

	// The daemon's marker-first resolver finds nothing for /foo.
	_, found, err := projectForRoot(fooRoot)
	require.NoError(t, err)
	require.False(t, found, "with the shared marker removed, the daemon resolver has no identity at /foo")

	// The CLI write path must refuse rebind advice rather than recommend a
	// rebind that steals bar's shared checkout marker.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has no checkout marker")
	assert.Contains(t, err.Error(), "shares its git directory with project "+bar.ID)
	assert.NotContains(t, err.Error(), "af projects rebind",
		"the shared absent marker must not recommend the rebind that steals bar's checkout")

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

// TestResolveProjectSelectorRejectsMissingMarkerSharedCommonDirOwnerUnresolvable
// pins the fail-closed half of the absent-marker scan. Like the resolvable
// shared-owner case (TestResolveProjectSelectorRejectsMissingMarkerSharedCommonDir),
// /foo's registered root has been replaced by another linked worktree of the
// same bare bar shares, and the shared marker on the bare is removed — but
// here bar's registered root has also been removed, so resolveProjectBinding
// fails. The prior code used projectRootUsesGitCommonDir, which suppresses
// resolution errors, so this read as a definite non-match and the
// absent-marker scan fell through to the rebind advice — recommending a
// rebind that would steal bar's still-shared marker and reattribute every
// surviving worktree to foo while leaving bar's registration stale. With the
// fix the write path refuses the rebind advice and names the unresolvable
// owner.
func TestResolveProjectSelectorRejectsMissingMarkerSharedCommonDirOwnerUnresolvable(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	// Register bar at a linked worktree of the bare: a worktree of a bare
	// repo shares the bare git common directory, and the bare's checkout
	// marker is bar's own registry marker.
	barRoot := filepath.Join(base, "bar")
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", barRoot)
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	// Replace foo's registered root with another linked worktree of the
	// same bare so /foo's binding.root stays /foo and binding.gitCommonDir
	// becomes the bare bar shares.
	require.NoError(t, os.RemoveAll(fooRoot))
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", fooRoot)
	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.True(t, projectRootUsesGitCommonDir(bar.Root, binding.gitCommonDir),
		"left intact for now, /bar must share the bare git common directory /foo now resolves to")

	// Remove the shared marker on the bare so foo's binding reads an
	// absent marker (the !markerExists branch).
	require.NoError(t, os.Remove(binding.checkoutMarkerPath))

	// Remove bar's registered root so the shared owner is unresolvable:
	// projectRootUsesGitCommonDir(bar.Root, ...) would suppress the
	// resolution error and read as a definitive non-match, which is the
	// hole this test pins.
	require.NoError(t, os.RemoveAll(barRoot))
	_, ownerUnresolvableErr := resolveProjectBinding(bar.Root)
	require.Error(t, ownerUnresolvableErr,
		"bar's root should no longer resolve after removal")

	// The CLI write path must refuse rather than recommend a rebind that
	// would steal the shared marker, naming the unresolvable owner.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has no checkout marker")
	assert.Contains(t, err.Error(), "could not be resolved")
	assert.Contains(t, err.Error(), bar.ID)
	assert.NotContains(t, err.Error(), "af projects rebind",
		"the unresolvable shared owner must not allow the rebind advice that steals bar's checkout")
}

// TestResolveProjectSelectorMissingMarkerSkipsUnresolvableUnrelatedProjectForPrivateCheckout
// pins the private-checkout half of the absent-marker scan. When the marker at
// the current checkout is missing and an unrelated registered root no longer
// resolves, the prior code unconditionally returned an error naming that
// unresolvable root and so blocked the rebind recovery — even when this
// checkout's <root>/.git is private and therefore cannot share its marker
// location with any registration. With the fix the unresolvable unrelated
// root is only fail-closed when this binding could actually use a shared
// common directory (a linked worktree, or a main checkout that has spawned
// linked worktrees of its own); for a private main checkout the scan skips the
// unresolvable root and the rebind advice stays reachable.
func TestResolveProjectSelectorMissingMarkerSkipsUnresolvableUnrelatedProjectForPrivateCheckout(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	barRoot := initProjectRegistryRepo(t, filepath.Join(base, "bar"))
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.False(t, sharedWorktreeCommonDir(binding.root, binding.gitCommonDir),
		"/foo is a plain main checkout whose <root>/.git lives inside the root")
	require.False(t, mainCheckoutHasLinkedWorktrees(binding.gitCommonDir),
		"/foo has not spawned linked worktrees, so its marker is private")

	// Remove foo's marker so the CLI write path takes the !markerExists
	// branch and scans the registry for another root that might share this
	// checkout's git directory.
	require.NoError(t, os.Remove(binding.checkoutMarkerPath))

	// Remove bar's registered root so its binding fails to resolve. With a
	// private .git at /foo, bar cannot share this checkout's marker, so an
	// unrelated stale registration must not block the rebind recovery for
	// foo.
	require.NoError(t, os.RemoveAll(barRoot))
	_, ownerUnresolvableErr := resolveProjectBinding(barRoot)
	require.Error(t, ownerUnresolvableErr,
		"bar's root should no longer resolve after removal")

	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has no checkout marker")
	assert.Contains(t, err.Error(), "af projects rebind",
		"a private checkout with one unrelated stale registration must still reach the rebind recovery")
	assert.NotContains(t, err.Error(), "could not be resolved",
		"the unrelated unresolvable root must not block the rebind recovery for a private checkout")
	assert.NotContains(t, err.Error(), bar.ID,
		"the unrelated unresolvable root must not be named when the marker is private")

	// The write path goes through ResolveProjectSelector, so it must surface
	// the same rebind recovery and succeed in writing foo's personal file
	// would-be target only after the rebind advice notes a different action;
	// here the write is still refused (no rebind has run), so it must not
	// touch bar's personal file.
	fooPersonalPath, err := ProjectConfigTomlPath(foo.ID)
	require.NoError(t, err)
	_, err = SetProjectConfigValue(fooRoot, "default_program", "codex")
	require.Error(t, err, "the CLI write path still refuses until the user rebinds")
	_, fooStatErr := os.Stat(fooPersonalPath)
	assert.ErrorIs(t, fooStatErr, os.ErrNotExist,
		"the refused write must not create foo's personal file")
}

// TestResolveProjectSelectorMissingMarkerSkipsAncestorFallbackNestedRegistration
// pins the exact-root guard in the absent-marker scan. When another
// registered project used to be a nested repository under this checkout's root
// and its nested .git directory was removed (leaving the directory present),
// resolveProjectBinding resolves the enclosing repository through git's
// ancestor fallback and returns this checkout's own git common directory. The
// prior code then falsely named the stale nested registration as a shared
// owner and blocked the rebind recovery, even though no linked worktree
// exists. With the fix the scan requires otherBinding.root to still name
// other.Root before treating its common directory as ownership evidence — the
// same exact-root guard ResolveRegisteredProjectRepo uses against nesting —
// so the rebind advice stays reachable.
func TestResolveProjectSelectorMissingMarkerSkipsAncestorFallbackNestedRegistration(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	// Register a nested repo under /foo's root so the nested registration's
	// recorded root resolves through git's ancestor fallback once its .git
	// is removed.
	nestedRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo", "nested"))
	nested, err := RegisterProject(nestedRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, nested.ID)

	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.False(t, sharedWorktreeCommonDir(binding.root, binding.gitCommonDir),
		"/foo is a plain main checkout whose <root>/.git lives inside the root")
	require.False(t, mainCheckoutHasLinkedWorktrees(binding.gitCommonDir),
		"/foo has not spawned linked worktrees, so its marker is private")

	// Strip the nested .git directory only — its root directory stays
	// present, so git -C /foo/nested now resolves the enclosing /foo
	// repository through ancestor fallback.
	require.NoError(t, os.RemoveAll(filepath.Join(nestedRoot, ".git")))
	nestedBinding, err := resolveProjectBinding(nestedRoot)
	require.NoError(t, err, "the enclosing /foo repo keeps nested resolvable as a path")
	require.False(t, sameProjectPath(nestedBinding.root, nestedRoot),
		"the nested .git is gone, so resolveProjectBinding must return the enclosing /foo root, not nested's recorded root")
	require.True(t, sameProjectPath(nestedBinding.root, binding.root),
		"the enclosing repo resolveProjectBinding now returns is /foo, whose common directory /foo shares")

	// Remove foo's marker so the CLI write path takes the !markerExists
	// branch and scans the registry, hitting the nested registration.
	require.NoError(t, os.Remove(binding.checkoutMarkerPath))

	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has no checkout marker")
	assert.Contains(t, err.Error(), "af projects rebind",
		"the stale nested registration resolved through ancestor fallback must not block the rebind recovery")
	assert.NotContains(t, err.Error(), "shares its git directory with project "+nested.ID,
		"a nested registration resolved through ancestor fallback is not ownership evidence for the shared directory")
}

// TestSharedWorktreeCommonDirRejectsSeparateGitDir pins the metadata-based
// fix to the linked-worktree predicate. Directory containment alone reads a
// `git init --separate-git-dir` checkout (and a submodule alike) as a linked
// worktree because its git common directory sits outside the worktree root;
// that misclass would refuse the safe deletion/rebind recovery in the
// retained-marker case. sharedWorktreeCommonDir now reads git's worktree
// metadata instead: a separate-git-dir repo's `<root>/.git` is a regular
// file pointing at the common dir itself (not into its `worktrees` subdir),
// so the predicate returns false for it — same as for a regular main
// checkout whose `.git` is a directory — and the deletion remedy stays
// reachable. A real linked worktree of a bare must still read true.
func TestSharedWorktreeCommonDirRejectsSeparateGitDir(t *testing.T) {
	base := t.TempDir()
	// A separate-git-dir checkout: <root>/.git is a gitdir file pointing
	// at the common dir directly (not into its worktrees subdir), so the
	// predicate must read false despite the common dir living outside the
	// root.
	root := filepath.Join(base, "root")
	separateDir := filepath.Join(base, "separate.git")
	require.NoError(t, os.MkdirAll(root, 0o755))
	runProjectRegistryGit(t, base, "init", "--quiet", "--separate-git-dir", separateDir, root)
	separateBinding, err := resolveProjectBinding(root)
	require.NoError(t, err, "a separate-git-dir checkout must resolve like any other")
	require.False(t, sameProjectPath(separateBinding.root, separateBinding.gitCommonDir),
		"--separate-git-dir must keep the common dir outside the root for this test to mean anything")
	require.FileExists(t, filepath.Join(separateBinding.root, ".git"),
		"a separate-git-dir checkout keeps a .git file (not a directory) at the root")
	require.False(t, sharedWorktreeCommonDir(separateBinding.root, separateBinding.gitCommonDir),
		"a --separate-git-dir checkout is not a linked worktree even though its common dir lives outside the root")

	// Sanity: a real linked worktree of a bare must still read true with
	// the metadata-based predicate, since its <root>/.git points into
	// <commonDir>/worktrees/<name>.
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	linked := filepath.Join(base, "linked")
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", linked)
	linkedBinding, err := resolveProjectBinding(linked)
	require.NoError(t, err)
	require.True(t, sharedWorktreeCommonDir(linkedBinding.root, linkedBinding.gitCommonDir),
		"a real linked worktree of a bare must still read true with the metadata-based predicate")

	// Sanity: a plain main checkout whose .git is a directory must still
	// read false.
	mainRoot := initProjectRegistryRepo(t, filepath.Join(base, "main"))
	mainBinding, err := resolveProjectBinding(mainRoot)
	require.NoError(t, err)
	require.False(t, sharedWorktreeCommonDir(mainBinding.root, mainBinding.gitCommonDir),
		"a main checkout whose .git is a directory is not a linked worktree")
}

// TestMainCheckoutHasLinkedWorktreesFailsClosedOnUnreadable pins the
// fail-closed fix to the metadata read. When <commonDir>/worktrees exists
// but cannot be read (permissions, transient I/O), the prior code treated
// the unknown result as "no linked worktrees" and fell through to the
// copied-marker remedy, recommending the user delete a marker that could
// still be shared with active worktrees. The fix returns true on
// indeterminate errors so the caller refuses the deletion advice instead.
// Only a determinate not-exist still reads false — that case is PROOF the
// main has no linked worktrees and the deletion remedy stays reachable.
func TestMainCheckoutHasLinkedWorktreesFailsClosedOnUnreadable(t *testing.T) {
	// A common dir without a worktrees subdir determinately has none.
	emptyCommonDir := t.TempDir()
	require.False(t, mainCheckoutHasLinkedWorktrees(emptyCommonDir),
		"no <commonDir>/worktrees directory proves there are no linked worktrees")

	// A common dir whose worktrees subdir has a directory entry proves it has.
	sharedCommonDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(sharedCommonDir, "worktrees", "linked-1"), 0o755))
	require.True(t, mainCheckoutHasLinkedWorktrees(sharedCommonDir),
		"a directory entry under <commonDir>/worktrees proves linked worktrees exist")

	// A common dir whose worktrees subdir cannot be read for any reason
	// other than not-exist is indeterminate: fail closed and return true.
	if os.Geteuid() == 0 {
		t.Skip("permission-based fail-closed test is unreliable when the test runs as root")
	}
	unreadableCommonDir := t.TempDir()
	unreadableWorktreesDir := filepath.Join(unreadableCommonDir, "worktrees")
	require.NoError(t, os.MkdirAll(unreadableWorktreesDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadableWorktreesDir, 0o755) })
	require.True(t, mainCheckoutHasLinkedWorktrees(unreadableCommonDir),
		"an unreadable <commonDir>/worktrees must fail closed as 'worktrees may exist'")
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
