package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// stagingSweepTestRoot creates a temp AF home with a docker-staging root and
// returns the root path plus a helper that mints a staged-payload dir of the
// shape dockerStagingDir produces (<slug>-<8 lowercase hex>) with the two
// files stageContainerPayload writes.
func stagingSweepTestRoot(t *testing.T) (root string, mk func(name string) string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	root = filepath.Join(home, dockerStagingDirName)
	require.NoError(t, os.MkdirAll(root, 0o700))
	return root, func(name string) string {
		d := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(d, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(d, "af"), []byte("binary"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(d, "af-policy.json"), []byte("{}"), 0o644))
		return d
	}
}

// stagingSweepDockerEnv arms the docker seams the sweep consults: a present
// CLI plus a fake exec that resolves the endpoint selector, reports engineIDs
// in order from `info` (per call, so a changed id can force an engine
// mismatch), and prints psListing/psErr for `ps` — the <id>\t<slug> rows
// listAfContainers parses. An empty psListing means no af containers exist.
func stagingSweepDockerEnv(t *testing.T, engineIDs []string, psListing string, psErr error) {
	t.Helper()
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	infoCall := 0
	cli := &fakeDockerContextCLI{}
	t.Cleanup(SetDockerExecForTest(func(ctx context.Context, environ []string, args ...string) ([]byte, error) {
		require.NotEmpty(t, args)
		switch args[0] {
		case "context":
			return cli.exec(ctx, environ, args...)
		case "info":
			id := engineIDs[min(infoCall, len(engineIDs)-1)]
			infoCall++
			return []byte(id + "\n"), nil
		case "ps":
			return []byte(psListing), psErr
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	}))
}

// TestStagingSweepReclaimsOrphanedDir is the #7590 property: a staging dir no
// live session record names — and no same-slug container exists to bind it —
// is reclaimed, contents and all. It is the exit a daemon killed between
// staging and the first persisted record leaves behind.
func TestStagingSweepReclaimsOrphanedDir(t *testing.T) {
	root, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	orphan := mk("dead-session-1a2b3c4d")
	SweepOrphanedStagingDirs("/test/home", nil, nil)

	_, err := os.Stat(orphan)
	assert.True(t, os.IsNotExist(err), "%s has no record and no container — it must be reclaimed", orphan)
	entries, readErr := os.ReadDir(root)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "the root should be empty after the only candidate is reaped")
}

// TestStagingSweepKeepsRecordClaimedDir pins the keep side of the property: a
// dir whose base name a session record still owns is never touched, whatever
// the slug or container sets say.
func TestStagingSweepKeepsRecordClaimedDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	claimed := mk("live-sess-aabbccdd")
	SweepOrphanedStagingDirs("/test/home", map[string]bool{"live-sess-aabbccdd": true}, nil)

	_, err := os.Stat(claimed)
	assert.NoError(t, err, "a record-claimed staging dir must stay")
}

// TestStagingSweepKeepsProtectedSlugDir: the mid-create window — a session
// that staged but has not persisted its record yet — protects every
// <slug>-<hex> dir under its slug, the same err-toward-sparing the container
// sweep applies.
func TestStagingSweepKeepsProtectedSlugDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	midCreate := mk("mid-create-11223344")
	SweepOrphanedStagingDirs("/test/home", nil, map[string]bool{"mid-create": true})

	_, err := os.Stat(midCreate)
	assert.NoError(t, err, "a staging dir under a live/pending slug must stay")
}

// TestStagingSweepKeepsContainerBoundSlug pins the container proof: while any
// af.session=<slug> container exists on this engine — running or not — the
// dir that slug could still bind is never removed. A stopped container holds
// the bind across a restart; existence, not mount inspection, is the check.
func TestStagingSweepKeepsContainerBoundSlug(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "cid-1\tbound-sess\n", nil)

	bound := mk("bound-sess-55667788")
	unrelated := mk("other-sess-99aabbcc")
	SweepOrphanedStagingDirs("/test/home", nil, nil)

	_, err := os.Stat(bound)
	assert.NoError(t, err, "a same-slug container exists — the dir might still be bound")
	_, err = os.Stat(unrelated)
	assert.True(t, os.IsNotExist(err), "a different slug with no container is still reclaimed")
}

// TestStagingSweepUnlistedWhenListingFails: a failed container listing proves
// nothing — the dirs stay for a later sweep, and the result says Unlisted
// rather than reading as "nothing to reclaim" (#7447's rule applied here).
func TestStagingSweepUnlistedWhenListingFails(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", fmt.Errorf("Cannot connect to the Docker daemon"))

	candidate := mk("maybe-bound-deadbeef")
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.True(t, got.Unlisted, "a failed listing is an unproven pass, not an empty one")
	assert.Equal(t, 0, got.Reaped)
	_, err := os.Stat(candidate)
	assert.NoError(t, err, "nothing may be removed while containers are unproven")
}

// TestStagingSweepUnlistedWithoutDockerCLI: no docker CLI now does not prove
// an earlier daemon created no container — Unlisted, nothing removed (Codex
// on #7448, applied to the dir sweep).
func TestStagingSweepUnlistedWithoutDockerCLI(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "", fmt.Errorf("not found") }))

	candidate := mk("maybe-bound-cafebabe")
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.True(t, got.Unlisted)
	assert.Equal(t, 0, got.Reaped)
	_, err := os.Stat(candidate)
	assert.NoError(t, err)
}

// TestStagingSweepUnlistedOnMovedEngine: an engine that changed between the
// identity read and the listing leaves the listing unproven — an empty ps
// from the wrong engine is not "no containers" (Codex on #7458).
func TestStagingSweepUnlistedOnMovedEngine(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-a", "engine-b"}, "", nil)

	candidate := mk("maybe-bound-0badf00d")
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.True(t, got.Unlisted, "a listing that may have come from another engine proves nothing")
	assert.Empty(t, got.EngineID)
	_, err := os.Stat(candidate)
	assert.NoError(t, err)
}

// TestStagingSweepLeavesNonCandidates: names af never mints — a stray
// directory, a loose file, a hex-suffixed name whose slug half is not a
// Slugify value — are left alone whether or not a keep set covers them.
func TestStagingSweepLeavesNonCandidates(t *testing.T) {
	root, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	stray := filepath.Join(root, "operator-notes")
	require.NoError(t, os.MkdirAll(stray, 0o700))
	badSlug := filepath.Join(root, "Not_A_Slug-1234abcd")
	require.NoError(t, os.MkdirAll(badSlug, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "loose-file"), []byte("x"), 0o644))
	orphan := mk("gone-87654321")

	SweepOrphanedStagingDirs("/test/home", nil, nil)

	for _, d := range []string{stray, badSlug, filepath.Join(root, "loose-file")} {
		_, err := os.Stat(d)
		assert.NoError(t, err, "%s is not a staging-dir name — never removed", d)
	}
	_, err := os.Stat(orphan)
	assert.True(t, os.IsNotExist(err), "the conforming orphan is still reclaimed")
}

// TestStagingSweepRefusesSymlinkedRoot: a docker-staging root that is not a
// real directory — an operator's data-disk link or a shared root — is
// refused wholesale: removing "orphans" under it would empty the shared
// TARGET. Same rule the setup-payload sweep applies.
func TestStagingSweepRefusesSymlinkedRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	target := t.TempDir()
	otherHomeDir := filepath.Join(target, "other-home-1122aabb")
	require.NoError(t, os.MkdirAll(otherHomeDir, 0o700))
	require.NoError(t, os.Symlink(target, filepath.Join(home, dockerStagingDirName)))
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	SweepOrphanedStagingDirs("/test/home", nil, nil)

	_, err = os.Stat(otherHomeDir)
	assert.NoError(t, err, "a linked docker-staging root must be refused, never emptied through the link")
}

// TestStagingSweepNoRootIsSilent: no docker-staging root — a home that never
// staged — is a no-op, not a warning, and never touches docker.
func TestStagingSweepNoRootIsSilent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	called := false
	t.Cleanup(SetDockerExecForTest(func(context.Context, []string, ...string) ([]byte, error) {
		called = true
		return nil, fmt.Errorf("docker must not be called")
	}))
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)
	assert.Equal(t, StagingSweepResult{}, got)
	assert.False(t, called)
}

// TestStagingSweepEmptyHomeDisabled: without a homeID the container proof
// cannot be scoped, so the pass never runs — the same disable rule the
// container sweep carries.
func TestStagingSweepEmptyHomeDisabled(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	candidate := mk("gone-0f0f0f0f")
	got := SweepOrphanedStagingDirs("", nil, nil)
	assert.Equal(t, StagingSweepResult{}, got)
	_, err := os.Stat(candidate)
	assert.NoError(t, err)
}

// TestStagingSweepNoCandidatesSkipsDocker: with nothing to reclaim the pass
// must not pay for a docker listing at all.
func TestStagingSweepNoCandidatesSkipsDocker(t *testing.T) {
	stagingSweepTestRoot(t)
	called := false
	t.Cleanup(SetDockerExecForTest(func(context.Context, []string, ...string) ([]byte, error) {
		called = true
		return nil, fmt.Errorf("docker must not be called")
	}))
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)
	assert.Equal(t, StagingSweepResult{}, got)
	assert.False(t, called)
}

// TestParseStagingDirName bounds the candidate shape to exactly what
// dockerStagingDir + Slugify can mint.
func TestParseStagingDirName(t *testing.T) {
	for _, tc := range []struct {
		name string
		slug string
		ok   bool
	}{
		{"session-a1b2c3d4", "session", true},
		{"my-sess-1-0badf00d", "my-sess-1", true},
		{"a-12345678", "a", true},
		// Not the minted shape.
		{"session-a1b2c3d", "", false},     // 7 hex digits
		{"session-a1b2c3d4e", "", false},   // 9 hex digits
		{"session-A1B2C3D4", "", false},    // uppercase hex
		{"session-a1b2c3zz", "", false},    // non-hex
		{"-12345678", "", false},           // empty slug
		{"-sess-12345678", "", false},      // leading-dash slug
		{"sess--12345678", "", false},      // trailing-dash slug
		{"Not_A_Slug-1234abcd", "", false}, // slug chars Slugify never emits
		{"operator-notes", "", false},      // no suffix at all
		{"", "", false},                    // empty
		{".", "", false},                   // path noise
		{"..", "", false},                  // traversal
		{"a/b-12345678", "", false},        // not a single element
	} {
		slug, ok := parseStagingDirName(tc.name)
		assert.Equal(t, tc.ok, ok, "parseStagingDirName(%q)", tc.name)
		assert.Equal(t, tc.slug, slug, "parseStagingDirName(%q) slug", tc.name)
	}
}

// TestDockerStagingDirClaim: the keep set is built from each instance's
// cleanup record — a docker backend carrying a staging claim returns its base
// name; every other shape returns "" (no teardown reaches the dir, so the
// sweep must treat the name as unclaimed).
func TestDockerStagingDirClaim(t *testing.T) {
	docker := &Instance{backend: &dockerBackend{cleanup: &DockerRuntimeCleanupData{
		ContainerID: "cid", EngineID: "eng", StagingDir: "sess-1a2b3c4d",
	}}}
	assert.Equal(t, "sess-1a2b3c4d", docker.DockerStagingDirClaim())

	assert.Equal(t, "", (&Instance{}).DockerStagingDirClaim(), "no backend: nothing claimed")
	assert.Equal(t, "", (&Instance{backend: &LocalBackend{}}).DockerStagingDirClaim(),
		"a backend with no cleanup provider claims nothing")
	assert.Equal(t, "", (&Instance{backend: &dockerBackend{cleanup: &DockerRuntimeCleanupData{
		ContainerID: "cid", EngineID: "eng",
	}}}).DockerStagingDirClaim(), "a docker handle without a staged dir claims nothing")
}

// TestDockerStagingDirClaimFromRestoredRow: the claim survives the inert
// restore a tombstoned row goes through — the daemon builds its keep set from
// materialized instances, so a record persisted before a daemon restart must
// still name its staging dir afterwards.
func TestDockerStagingDirClaimFromRestoredRow(t *testing.T) {
	inst, err := FromInstanceData(InstanceData{
		ID:          "id-1",
		Title:       "sess",
		BackendType: "docker",
		Liveness:    LiveRunning,
		RuntimeCleanup: &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
			ContainerID: "cid", EngineID: "eng", StagingDir: "sess-1a2b3c4d",
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-1a2b3c4d", inst.DockerStagingDirClaim())
}
