package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// stagingSweepTestRoot creates a temp AF home with a docker-staging root and
// returns the root path plus a helper that mints a staged-payload dir of the
// shape dockerStagingDir produces (<slug>-<8 lowercase hex>) with the two
// files stageContainerPayload writes. Tests add the engine marker explicitly
// via stagingMarkIdentity, since the marker is what lets the sweep reclaim.
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

// stagingMarkIdentity writes the dir's recorded identity markers, as
// recordStagingIdentity does at provision: the resolved engine ID and the
// af.home label spelling the container will carry. The sweep's homeID
// argument is the spelling it filters the listing by, so reclaim tests mark
// the same "/test/home" they pass in.
func stagingMarkIdentity(t *testing.T, dir, engineID, homeID string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, dockerStagingEngineFileName), []byte(engineID+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, dockerStagingHomeFileName), []byte(homeID+"\n"), 0o600))
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
// live session record names — recorded on this engine, and no same-slug
// container exists to bind it — is reclaimed, contents and all. It is the
// exit a daemon killed between staging and the first persisted record leaves
// behind.
func TestStagingSweepReclaimsOrphanedDir(t *testing.T) {
	root, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	orphan := mk("dead-session-1a2b3c4d")
	stagingMarkIdentity(t, orphan, "engine-1", "/test/home")
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
	stagingMarkIdentity(t, bound, "engine-1", "/test/home")
	unrelated := mk("other-sess-99aabbcc")
	stagingMarkIdentity(t, unrelated, "engine-1", "/test/home")
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
	stagingMarkIdentity(t, candidate, "engine-1", "/test/home")
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
	stagingMarkIdentity(t, candidate, "engine-1", "/test/home")
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
	stagingMarkIdentity(t, candidate, "engine-a", "/test/home")
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.True(t, got.Unlisted, "a listing that may have come from another engine proves nothing")
	assert.Empty(t, got.EngineID)
	_, err := os.Stat(candidate)
	assert.NoError(t, err)
}

// TestStagingSweepKeepsUnrecordedDir is the Codex P1's deletion direction: a
// dir with no engine marker — a legacy dir from before the marker existed,
// or a provision that staged before the engine resolved — is kept, because
// the current engine's "not found" says nothing about another engine's
// containers.
func TestStagingSweepKeepsUnrecordedDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	legacy := mk("old-attempt-11aa22bb")
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.Equal(t, 0, got.Reaped)
	_, err := os.Stat(legacy)
	assert.NoError(t, err, "an unrecorded staging dir is kept, not swept")
}

// TestStagingSweepKeepsDifferentEngineDir: a dir whose marker names another
// engine is kept — the listing only proves the CURRENT engine's containers,
// and a container binding it could still exist where it was created.
func TestStagingSweepKeepsDifferentEngineDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	foreign := mk("other-engine-33cc44dd")
	stagingMarkIdentity(t, foreign, "engine-elsewhere", "/test/home")
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.Equal(t, 0, got.Reaped)
	_, err := os.Stat(foreign)
	assert.NoError(t, err, "a dir recorded on a different engine is kept")
}

// TestStagingSweepKeepsAliasedHomeDir is the Codex P2's alias case: a dir
// whose marker records a different af.home spelling — a symlinked
// AGENT_FACTORY_HOME reaching the same real directory — is kept. The label
// filter is lexical, so a container bound under the other spelling is
// invisible to this pass's listing.
func TestStagingSweepKeepsAliasedHomeDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	aliased := mk("old-spelling-2b2b3c4d")
	stagingMarkIdentity(t, aliased, "engine-1", "/data-link/af")
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.Equal(t, 0, got.Reaped)
	_, err := os.Stat(aliased)
	assert.NoError(t, err, "a dir recorded under another home spelling is kept")
}

// TestStagingSweepKeepsMalformedMarkerDir: a marker that does not hold the
// shape provisioning writes — torn, tampered, empty, missing — is unrecorded
// for the sweep's purposes and keeps the dir, whichever half it is.
func TestStagingSweepKeepsMalformedMarkerDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	// Each row writes engine-id and home-id; nil leaves the file unwritten.
	for _, tc := range []struct {
		name         string
		engine, home *string
	}{
		{"torn-engine-55ee66ff", ptr("engine-1 extra-words\n"), ptr("/test/home\n")},
		{"empty-engine-77aa88bb", ptr(""), ptr("/test/home\n")},
		{"oversize-engine-99bbccdd", ptr(strings.Repeat("e", 300) + "\n"), ptr("/test/home\n")},
		{"no-home-11223344", ptr("engine-1\n"), nil},
		{"empty-home-55667788", ptr("engine-1\n"), ptr("")},
		{"multiline-home-99aabbcc", ptr("engine-1\n"), ptr("/test/home\n/evil\n")},
	} {
		d := mk(tc.name)
		if tc.engine != nil {
			require.NoError(t, os.WriteFile(filepath.Join(d, dockerStagingEngineFileName), []byte(*tc.engine), 0o600))
		}
		if tc.home != nil {
			require.NoError(t, os.WriteFile(filepath.Join(d, dockerStagingHomeFileName), []byte(*tc.home), 0o600))
		}
	}
	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.Equal(t, 0, got.Reaped)
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	for _, name := range []string{
		"torn-engine-55ee66ff", "empty-engine-77aa88bb", "oversize-engine-99bbccdd",
		"no-home-11223344", "empty-home-55667788", "multiline-home-99aabbcc",
	} {
		_, err := os.Stat(filepath.Join(home, dockerStagingDirName, name))
		assert.NoError(t, err, "%s: a malformed marker is kept", name)
	}
}

// TestDockerStagingDirRecordsEngineMarker: provisioning records the resolved
// engine identity beside the staged payload — the same `docker info` ID the
// cleanup record carries — and the af.home label spelling the container will
// carry, so a later sweep can prove it is acting on the engine and under the
// label spelling that created the dir. A provision that staged before either
// resolved writes nothing for that half, and the dir stays unrecorded (kept).
func TestDockerStagingDirRecordsEngineMarker(t *testing.T) {
	stagingTestEnv(t)
	p := &dockerProvisioner{
		spec:     ProvisionSpec{Title: "eng sess"},
		engineID: "engine-resolved",
		homeID:   "/test/af-home",
	}
	dir, err := p.dockerStagingDir()
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(dir, dockerStagingEngineFileName))
	require.NoError(t, err, "the engine marker must be written beside the staged payload")
	assert.Equal(t, "engine-resolved\n", string(raw))
	rawHome, err := os.ReadFile(filepath.Join(dir, dockerStagingHomeFileName))
	require.NoError(t, err, "the home marker must be written beside the staged payload")
	assert.Equal(t, "/test/af-home\n", string(rawHome))

	p2 := &dockerProvisioner{spec: ProvisionSpec{Title: "early sess"}}
	dir2, err := p2.dockerStagingDir()
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir2, dockerStagingEngineFileName))
	assert.True(t, os.IsNotExist(err), "no engine resolved yet: nothing recorded")
	_, err = os.Stat(filepath.Join(dir2, dockerStagingHomeFileName))
	assert.True(t, os.IsNotExist(err), "no home label resolved: nothing recorded")
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
	stagingMarkIdentity(t, orphan, "engine-1", "/test/home")

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
