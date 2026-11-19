package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// stagingSweepTestRoot creates a temp AF home with a docker-staging root and
// returns the root path plus a helper that mints a staged-payload dir of the
// shape dockerStagingDir produces (<slug>-<8 lowercase hex>) with the two
// files stageContainerPayload writes. The home's tunnel-generation registry
// is cleaned up with the test — a test that establishes its own live
// generation does not leak its held lock into the next test. Tests add the
// identity markers explicitly via stagingMarkIdentity, since the markers are
// what lets the sweep reclaim.
func stagingSweepTestRoot(t *testing.T) (root string, mk func(name string) string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	t.Cleanup(func() {
		canonical, cerr := currentTunnelHome()
		if cerr != nil {
			return
		}
		if ent, ok := tunnelGenDirs.Load(canonical); ok {
			e := ent.(*tunnelGenDirEntry)
			if e.lock != nil {
				_ = e.lock.Close()
			}
			if e.root != nil && e.root.dir != nil {
				_ = e.root.dir.Close()
			}
			tunnelGenDirs.Delete(canonical)
		}
	})
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

// stagingDeadGeneration is the generation-id stagingMarkIdentity writes by
// default: a well-formed name that exists under no test home's tunnels/ —
// a generation that is provably dead (its dir is gone), which is the only
// kind a dir must record before the sweep will reclaim it.
const stagingDeadGeneration = "d999999-dead00"

// stagingMarkIdentity writes the dir's recorded identity markers as
// recordStagingIdentity does at provision, with a dead generation recorded —
// the default tests want, since only a dead generation's dirs are
// sweepable. Tests for live-generation protection write their own marker
// via stagingMarkIdentityGen.
func stagingMarkIdentity(t *testing.T, dir, engineID, homeID string) {
	t.Helper()
	stagingMarkIdentityGen(t, dir, stagingDeadGeneration, engineID, homeID)
}

// stagingMarkIdentityGen writes all three identity markers: the generation
// that minted the dir, the resolved engine ID, and the af.home label
// spelling the container will carry. The sweep's homeID argument is the
// spelling it filters the listing by, so reclaim tests mark the same
// "/test/home" they pass in.
func stagingMarkIdentityGen(t *testing.T, dir, gen, engineID, homeID string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, dockerStagingGenerationFileName), []byte(gen+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, dockerStagingEngineFileName), []byte(engineID+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, dockerStagingHomeFileName), []byte(homeID+"\n"), 0o600))
}

// stagingLiveGeneration stands up a live generation under this test's AF
// home — a tunnels/d<pid>-<rand> dir whose .lock this process holds open —
// and returns its basename. A staging dir marked with it is read by the
// sweep as minted by a live af process: never a candidate, whatever the
// keep sets say.
func stagingLiveGeneration(t *testing.T, name string) string {
	t.Helper()
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	gen := filepath.Join(home, tunnelSocketRootName, name)
	require.NoError(t, os.MkdirAll(gen, 0o700))
	lock, err := os.OpenFile(filepath.Join(gen, tunnelGenLockName), os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	t.Cleanup(func() { _ = lock.Close() })
	return name
}

// stagingSweepDockerEnv arms the docker seams the sweep consults: a present
// CLI plus a fake exec that resolves the endpoint selector, reports engineIDs
// in order from `info` (per call, so a changed id can force an engine
// mismatch), and prints psListing/psErr for `ps` — the <id>\t<slug> rows
// listAfContainers parses. An empty psListing means no af containers exist.
// DOCKER_HOST/DOCKER_CONTEXT are scrubbed so the selector the sweep pins is
// deterministic: the resolved current context's name, which the fake answers
// as "default" and materializes into DOCKER_CONTEXT in the passed environ.
func stagingSweepDockerEnv(t *testing.T, engineIDs []string, psListing string, psErr error) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	infoCall := 0
	cli := &fakeDockerContextCLI{}
	t.Cleanup(SetDockerExecForTest(func(ctx context.Context, environ []string, args ...string) ([]byte, error) {
		require.NotEmpty(t, args)
		switch args[0] {
		case "context":
			if len(args) == 4 && args[1] == "inspect" && args[2] == "--format" && args[3] == dockerContextNameFormat {
				return []byte(cli.current(environ) + "\n"), nil
			}
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
// for the sweep's purposes and keeps the dir, whichever marker it is.
func TestStagingSweepKeepsMalformedMarkerDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	dead := stagingDeadGeneration + "\n"
	// Each row writes generation-id, engine-id and home-id; nil leaves the
	// file unwritten. Rows carry valid siblings so the malformed one is what
	// keeps the dir.
	for _, tc := range []struct {
		name              string
		gen, engine, home *string
	}{
		{"torn-engine-55ee66ff", &dead, ptr("engine-1 extra-words\n"), ptr("/test/home\n")},
		{"empty-engine-77aa88bb", &dead, ptr(""), ptr("/test/home\n")},
		{"oversize-engine-99bbccdd", &dead, ptr(strings.Repeat("e", 300) + "\n"), ptr("/test/home\n")},
		{"no-home-11223344", &dead, ptr("engine-1\n"), nil},
		{"empty-home-55667788", &dead, ptr("engine-1\n"), ptr("")},
		{"multiline-home-99aabbcc", &dead, ptr("engine-1\n"), ptr("/test/home\n/evil\n")},
		{"no-gen-22446688", nil, ptr("engine-1\n"), ptr("/test/home\n")},
		{"empty-gen-33557799", ptr(""), ptr("engine-1\n"), ptr("/test/home\n")},
		{"torn-gen-446688aa", ptr("not a gen name\n"), ptr("engine-1\n"), ptr("/test/home\n")},
		{"multiline-gen-557799bb", ptr("d1-abcdef\nd2-abcdef\n"), ptr("engine-1\n"), ptr("/test/home\n")},
		{"oversize-gen-6688aacc", ptr(strings.Repeat("d1-abcdef\n", 500)), ptr("engine-1\n"), ptr("/test/home\n")},
	} {
		d := mk(tc.name)
		if tc.gen != nil {
			require.NoError(t, os.WriteFile(filepath.Join(d, dockerStagingGenerationFileName), []byte(*tc.gen), 0o600))
		}
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
		"no-gen-22446688", "empty-gen-33557799", "torn-gen-446688aa",
		"multiline-gen-557799bb", "oversize-gen-6688aacc",
	} {
		_, err := os.Stat(filepath.Join(home, dockerStagingDirName, name))
		assert.NoError(t, err, "%s: a malformed marker is kept", name)
	}
}

// TestStagingSweepKeepsNonRegularMarkerDir: a marker entry that is not a
// regular file — a directory planted where the file belongs — is never
// opened and keeps the dir (the Lstat gate precedes every read, #7616 R3).
func TestStagingSweepKeepsNonRegularMarkerDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	// generation-id as a directory: unprovable, kept before the engine and
	// home markers are even consulted.
	d1 := mk("gen-is-dir-a1b2c3d4")
	require.NoError(t, os.Mkdir(filepath.Join(d1, dockerStagingGenerationFileName), 0o700))
	// engine-id as a directory: the generation proves dead, the engine stays
	// unprovable — still kept.
	d2 := mk("eng-is-dir-e5f6a7b8")
	stagingMarkIdentityGen(t, d2, stagingDeadGeneration, "engine-1", "/test/home")
	require.NoError(t, os.Remove(filepath.Join(d2, dockerStagingEngineFileName)))
	require.NoError(t, os.Mkdir(filepath.Join(d2, dockerStagingEngineFileName), 0o700))

	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.Equal(t, 0, got.Reaped)
	for _, d := range []string{d1, d2} {
		_, err := os.Stat(d)
		assert.NoError(t, err, "%s: a non-regular marker is never trusted — keep", d)
	}
}

// TestStagingSweepKeepsLiveGenerationDir is the #7616 R1 property: a staging
// dir minted mid-pass under an UNPROTECTED slug — a restore's reprovision, a
// lazily registered row, a create whose rename moved it out of the snapshot —
// is never a candidate while the generation that minted it is alive. The live
// dir records a generation whose .lock a live af process holds (here: this
// process, standing in for the daemon or a predecessor mid-upgrade); the dead
// twin records a generation whose dir is gone. Identical engine and home
// markers, identical empty keep sets — only the generation differs, and only
// the dead one is reclaimed.
func TestStagingSweepKeepsLiveGenerationDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	liveGen := stagingLiveGeneration(t, "d4242-a1b2c3")
	live := mk("restored-sess-0011aabb")
	stagingMarkIdentityGen(t, live, liveGen, "engine-1", "/test/home")
	dead := mk("orphaned-sess-2233ccdd")
	stagingMarkIdentityGen(t, dead, stagingDeadGeneration, "engine-1", "/test/home")

	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	_, err := os.Stat(live)
	assert.NoError(t, err, "a dir a live generation minted is never a candidate — its container may not exist yet")
	_, err = os.Stat(dead)
	assert.True(t, os.IsNotExist(err), "the same dir from a provably dead generation is reclaimed")
	assert.Equal(t, 1, got.Reaped)
}

// TestStagingSweepKeepsThisGenerationsMintedDir exercises the exact mint
// path: the generation recordStagingIdentity records is this process's own
// live generation (the tunnels/ machinery holds its lock), so a dir this
// daemon JUST staged — mid-pass, under a slug no keep set names — is kept.
// The slug-snapshot keep set cannot cover provisions still in flight; the
// generation lock is the proof that does.
func TestStagingSweepKeepsThisGenerationsMintedDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	genDir, err := tunnelGenerationDir()
	require.NoError(t, err)
	minted := mk("midprovision-a1b2c3d4")
	stagingMarkIdentityGen(t, minted, filepath.Base(genDir), "engine-1", "/test/home")

	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.Equal(t, 0, got.Reaped)
	_, err = os.Stat(minted)
	assert.NoError(t, err, "a dir this daemon generation minted is never a candidate")
}

// TestStagingSweepKeepsUnprovableGenerationDir: a generation marker that
// names a real-looking dir whose liveness cannot be proven — no .lock to
// probe — is unproven rather than dead, and keeps. Only a lock HELD-or-FREE
// answer (or a generation dir that is gone entirely, which the lock-gated
// generation sweep alone removes) discharges the proof.
func TestStagingSweepKeepsUnprovableGenerationDir(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	home, err := config.GetConfigDir()
	require.NoError(t, err)
	// A generation dir with no .lock: its owner's liveness is unprovable.
	lockless := "d5555-0f0f0f"
	require.NoError(t, os.MkdirAll(filepath.Join(home, tunnelSocketRootName, lockless), 0o700))
	unprovable := mk("unprovable-bbccddee")
	stagingMarkIdentityGen(t, unprovable, lockless, "engine-1", "/test/home")

	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.Equal(t, 0, got.Reaped)
	_, err = os.Stat(unprovable)
	assert.NoError(t, err, "an unprovable generation keeps the dir — dead must be proven, not assumed")
}

// TestStagingSweepDeadGenerationDirWithFreeLock: a generation dir that still
// exists but whose .lock is free — its owner exited and the generation sweep
// has not removed it yet — is a proven-dead generation, so the dir it minted
// proceeds to the other proofs and reclaims.
func TestStagingSweepDeadGenerationDirWithFreeLock(t *testing.T) {
	_, mk := stagingSweepTestRoot(t)
	stagingSweepDockerEnv(t, []string{"engine-1"}, "", nil)

	home, err := config.GetConfigDir()
	require.NoError(t, err)
	dead := "d6666-f6f6f6"
	require.NoError(t, os.MkdirAll(filepath.Join(home, tunnelSocketRootName, dead), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, tunnelSocketRootName, dead, tunnelGenLockName), nil, 0o600))
	orphan := mk("dead-gen-ccddee00")
	stagingMarkIdentityGen(t, orphan, dead, "engine-1", "/test/home")

	got := SweepOrphanedStagingDirs("/test/home", nil, nil)

	assert.Equal(t, 1, got.Reaped)
	_, err = os.Stat(orphan)
	assert.True(t, os.IsNotExist(err), "a generation whose lock provably freed is dead — its dirs reclaim")
}

// TestStagingSweepPinsOneSelector is the #7616 R2 property: once the
// endpoint resolves, every docker call of the pass — the engine-ID read, the
// container listing, and the post-pass ID re-read — runs under ONE selector,
// so a `docker context use` switch mid-pass cannot move the listing to an
// engine the identity reads never saw. The pin is materialized into the
// environ (DOCKER_HOST/DOCKER_CONTEXT — the variables the CLI itself reads),
// so argv stays bare while the endpoint cannot drift.
func TestStagingSweepPinsOneSelector(t *testing.T) {
	// Variant A — DOCKER_HOST set: it already pins the CLI on every call;
	// the resolution adds nothing (and needs no name inspect at all).
	t.Run("docker host", func(t *testing.T) {
		_, mk := stagingSweepTestRoot(t)
		host := fakeUnixDockerSocket(t)
		t.Setenv("DOCKER_HOST", host)
		t.Setenv("DOCKER_CONTEXT", "")
		var calls [][]string
		var envs [][]string
		t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, environ []string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			envs = append(envs, append([]string(nil), environ...))
			switch args[0] {
			case "info":
				return []byte("engine-1\n"), nil
			case "ps":
				return []byte(""), nil
			}
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}))
		orphan := mk("gone-aabbccdd")
		stagingMarkIdentity(t, orphan, "engine-1", "/test/home")

		got := SweepOrphanedStagingDirs("/test/home", nil, nil)

		assert.Equal(t, 1, got.Reaped)
		require.Len(t, calls, 3, "info, ps, and the re-read — three calls on the pinned endpoint")
		for i, e := range envs {
			assert.Equal(t, host, rawEnvironmentValue(e, "DOCKER_HOST"), "call %d must carry DOCKER_HOST's pin", i)
		}
	})

	// Variant B — neither selector set: the two resolution inspects run
	// unpinned (they ARE the resolution); every call after them carries the
	// resolved context's own name materialized as DOCKER_CONTEXT.
	t.Run("resolved context", func(t *testing.T) {
		_, mk := stagingSweepTestRoot(t)
		t.Setenv("DOCKER_HOST", "")
		t.Setenv("DOCKER_CONTEXT", "")
		var calls [][]string
		var envs [][]string
		cli := &fakeDockerContextCLI{}
		t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
		t.Cleanup(SetDockerExecForTest(func(ctx context.Context, environ []string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			envs = append(envs, append([]string(nil), environ...))
			switch args[0] {
			case "context":
				if len(args) == 4 && args[1] == "inspect" && args[2] == "--format" && args[3] == dockerContextNameFormat {
					return []byte(cli.current(environ) + "\n"), nil
				}
				return cli.exec(ctx, environ, args...)
			case "info":
				return []byte("engine-1\n"), nil
			case "ps":
				return []byte(""), nil
			}
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}))
		orphan := mk("gone-11223344")
		stagingMarkIdentity(t, orphan, "engine-1", "/test/home")

		got := SweepOrphanedStagingDirs("/test/home", nil, nil)

		assert.Equal(t, 1, got.Reaped)
		require.Len(t, calls, 5, "endpoint inspect, name inspect, info, ps, re-read")
		assert.Equal(t, "context", calls[0][0], "the endpoint resolution runs unpinned")
		assert.Equal(t, "", rawEnvironmentValue(envs[0], "DOCKER_CONTEXT"), "the endpoint inspect is itself the resolution")
		assert.Equal(t, "context", calls[1][0], "the name resolution runs unpinned")
		assert.Equal(t, "", rawEnvironmentValue(envs[1], "DOCKER_CONTEXT"), "the name inspect is itself the resolution")
		for i, e := range envs[2:] {
			assert.Equal(t, "default", rawEnvironmentValue(e, "DOCKER_CONTEXT"), "call %d must carry the resolved context pin", i+2)
		}
	})
}

// TestResolveDockerContextPin: the selector already in the env short-circuits
// without a call; a bare env resolves the current context's own name once;
// an inspect failure or a malformed answer errors the resolution — callers
// fail closed (Unlisted pass / refused provision), never unpinned.
func TestResolveDockerContextPin(t *testing.T) {
	t.Run("env selector short-circuits", func(t *testing.T) {
		calls := 0
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
			calls++
			return nil, fmt.Errorf("no docker call may run when the env selects")
		}))
		for _, env := range [][]string{
			{"DOCKER_HOST=unix:///var/run/docker.sock"},
			{"DOCKER_HOST= "},
			{"DOCKER_CONTEXT=work"},
		} {
			pin, err := resolveDockerContextPin(env)
			require.NoError(t, err)
			assert.Empty(t, pin)
		}
		assert.Zero(t, calls)
	})

	t.Run("bare env resolves the name once", func(t *testing.T) {
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			require.Equal(t, []string{"context", "inspect", "--format", dockerContextNameFormat}, args)
			return []byte("workctx\n"), nil
		}))
		pin, err := resolveDockerContextPin(nil)
		require.NoError(t, err)
		assert.Equal(t, "workctx", pin)
	})

	t.Run("failures and malformed names error", func(t *testing.T) {
		for _, answer := range [][]byte{[]byte(""), []byte("a\nb\n"), []byte("with space\n")} {
			t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
				return answer, nil
			}))
			_, err := resolveDockerContextPin(nil)
			assert.Error(t, err, "answer %q must not resolve", answer)
		}
		t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
			return nil, fmt.Errorf("docker daemon unreachable")
		}))
		_, err := resolveDockerContextPin(nil)
		assert.Error(t, err)
	})
}

// TestProvisionPinsSelectorBeforeEngineRead is the Codex-on-#7616 property:
// the provision resolves the docker CLI's selector ONCE, materializes it as
// DOCKER_CONTEXT, and only then reads the engine identity and runs the
// container — so a `docker context use` switch mid-provision cannot put the
// container on an engine the staging dir's engine-id marker never recorded.
// The pass's own resolution calls are the only unpinned ones.
func TestProvisionPinsSelectorBeforeEngineRead(t *testing.T) {
	stagingTestEnv(t)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	var calls [][]string
	var envs [][]string
	cli := &fakeDockerContextCLI{}
	t.Cleanup(SetDockerExecForTest(func(ctx context.Context, environ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		envs = append(envs, append([]string(nil), environ...))
		switch {
		case len(args) == 4 && args[0] == "context" && args[1] == "inspect" && args[2] == "--format" && args[3] == dockerContextNameFormat:
			return []byte(cli.current(environ) + "\n"), nil
		case len(args) > 0 && args[0] == "context":
			return cli.exec(ctx, environ, args...)
		case len(args) > 0 && args[0] == "info":
			return []byte("engine-1\n"), nil
		}
		return nil, fmt.Errorf("stopping the provision here: %v", args)
	}))

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "pin sess"}}
	_, err := p.provisionSteps()
	require.Error(t, err, "the stubbed later step must stop the provision")

	require.GreaterOrEqual(t, len(calls), 2, "the pin resolution and at least one pinned call must run")
	assert.Equal(t, []string{"context", "inspect", "--format", dockerContextNameFormat}, calls[0],
		"the context-name resolution must be the FIRST docker call — the pin precedes every call it pins")
	assert.Equal(t, "", rawEnvironmentValue(envs[0], "DOCKER_CONTEXT"), "the resolution call itself runs unpinned")
	for i := 1; i < len(calls); i++ {
		assert.Equal(t, "default", rawEnvironmentValue(envs[i], "DOCKER_CONTEXT"),
			"call %d (%v) must run under the materialized pin", i, calls[i])
	}
}

// TestProvisionerDockerEnvironmentSkipsPinWhenSelectorSet: the pin is empty —
// dockerEnvironment materializes nothing — when the env already selects
// (DOCKER_HOST or DOCKER_CONTEXT outrank a context switch on their own), and
// on paths that never provisioned (a restored reaper reads the operator's
// live selector by design).
func TestProvisionerDockerEnvironmentSkipsPinWhenSelectorSet(t *testing.T) {
	p := &dockerProvisioner{spec: ProvisionSpec{Title: "unpinned"}}
	assert.NotContains(t, p.dockerEnvironment(), "DOCKER_CONTEXT=ctx",
		"an unset pin must not materialize")
	p.dockerContextPin = "ctx"
	assert.Equal(t, "ctx", rawEnvironmentValue(p.dockerEnvironment(), "DOCKER_CONTEXT"),
		"a resolved pin materializes into every provision-time call's env")
}

// TestDockerStagingDirRecordsEngineMarker: provisioning records beside the
// staged payload the resolved engine identity — the same `docker info` ID
// the cleanup record carries — the af.home label spelling the container will
// carry, and this daemon's generation dir name, so a later sweep can prove
// it is acting on the engine, under the label spelling, and past the
// generation that created the dir. A provision that staged before the engine
// or home resolved writes nothing for that half, and the dir stays
// unrecorded (kept); the generation marker is always recorded because the
// mint itself establishes the generation.
func TestDockerStagingDirRecordsEngineMarker(t *testing.T) {
	home := stagingTestEnv(t)
	stubTunnelHome(t, home)
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
	rawGen, err := os.ReadFile(filepath.Join(dir, dockerStagingGenerationFileName))
	require.NoError(t, err, "the generation marker must be written beside the staged payload")
	gen := strings.TrimSuffix(string(rawGen), "\n")
	assert.True(t, tunnelGenDirRE.MatchString(gen), "the marker names a generation dir, got %q", gen)
	genDir, err := tunnelGenerationDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(genDir), gen, "the marker names THIS process's live generation")

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
