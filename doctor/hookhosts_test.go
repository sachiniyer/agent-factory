package doctor

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// Tests for the orphaned pinned host-key collector (#3560).
//
// Every one of them runs with Fix: true, including the ones asserting a
// directory is SPARED. A report-only run cannot distinguish "doctor decided not
// to remove this" from "doctor never got the chance", and the whole check exists
// to make that distinction. So the fixtures arm the removal and then prove it did
// not happen.
//
// Hermetic by construction, like the rest of this package: the af home is a temp
// dir, the session inventory is either an injected fake or a real Unix-socket
// server this test binds, and nothing here can see the developer's daemon.

const (
	hookHostOwnerTitle = "gone session"
	hookHostOwnerSlug  = "gone-session"
)

// hookHostFixture is one hermetic run: an af home with a pin store, and the
// Options that scan it.
type hookHostFixture struct {
	opts Options
	home string
	root string
}

// newHookHostFixture stages an af home whose pin store holds one directory for
// slug, and defaults the session inventory to "the daemon answered, with no
// sessions" so a test only has to declare the axis it is exercising.
func newHookHostFixture(t *testing.T, slug string) *hookHostFixture {
	t.Helper()
	opts := testOptions(t, true)
	f := &hookHostFixture{
		opts: opts,
		home: opts.ConfigDir,
		root: filepath.Join(opts.ConfigDir, session.HookHostsRoot),
	}
	f.opts.sessionInventory = func() ([]session.InstanceData, error) { return nil, nil }
	if slug != "" {
		f.writePin(t, slug)
	}
	return f
}

// writePin creates a directory in the shape hookProvisionKnownHosts writes: one
// known_hosts file, and nothing else.
func (f *hookHostFixture) writePin(t *testing.T, slug string) string {
	t.Helper()
	dir := filepath.Join(f.root, slug)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, session.HookHostsPinFileName),
		[]byte("[10.0.0.1]:22 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5\n"), 0o600))
	return dir
}

// pinDir is where slug's pin lives, whether or not it exists.
func (f *hookHostFixture) pinDir(slug string) string { return filepath.Join(f.root, slug) }

// daemonSees makes the injected inventory answer with these session titles, as
// the running daemon's global snapshot would.
func (f *hookHostFixture) daemonSees(titles ...string) {
	f.opts.sessionInventory = func() ([]session.InstanceData, error) {
		return instancesTitled(titles...), nil
	}
}

// storeRecords writes one project's session records through the production
// writer, so the fixture is a real instances.json in the real place.
func (f *hookHostFixture) storeRecords(t *testing.T, repoID string, records ...session.InstanceData) {
	t.Helper()
	raw, err := json.Marshal(records)
	require.NoError(t, err)
	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, config.SaveRepoInstances(repoID, raw))
}

// instancesDir is <home>/instances, the directory holding one subdirectory per
// project.
func (f *hookHostFixture) instancesDir() string { return filepath.Join(f.home, "instances") }

// run executes the full sweep and returns the report.
func (f *hookHostFixture) run(t *testing.T) *Report {
	t.Helper()
	report, err := Run(f.opts)
	require.NoError(t, err)
	return report
}

func instancesTitled(titles ...string) []session.InstanceData {
	out := make([]session.InstanceData, 0, len(titles))
	for _, title := range titles {
		out = append(out, session.InstanceData{Title: title, Liveness: session.LiveRunning})
	}
	return out
}

// hookHostFindings returns just this check's findings, in detection order.
func hookHostFindings(r *Report) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Check == hookHostsCheck {
			out = append(out, f)
		}
	}
	return out
}

// hookHostPassDetails returns the detail of every PASS row this check emitted.
func hookHostPassDetails(r *Report) []string {
	var out []string
	for _, c := range r.Checks {
		if c.Section == sectionRemote && c.Name == "hook host key" && c.Status == StatusPass {
			out = append(out, c.Detail)
		}
	}
	return out
}

// requireIntactPin asserts the directory AND the key inside it survived. The
// directory alone verifies nothing: an empty directory would still fail every
// later host-key verification, which is the outcome this check exists to avoid.
func requireIntactPin(t *testing.T, dir string) {
	t.Helper()
	require.DirExists(t, dir, "the pinned host-key directory was removed")
	require.FileExists(t, filepath.Join(dir, session.HookHostsPinFileName),
		"the pinned key itself was removed, so no later reap can verify the host")
}

// requireUnknown asserts the check reported exactly one UNKNOWN observation and
// armed no removal at all — the shape every inventory failure must produce.
func requireUnknown(t *testing.T, r *Report, want string) {
	t.Helper()
	findings := hookHostFindings(r)
	require.Len(t, findings, 1, "expected exactly one UNKNOWN observation")
	require.False(t, findings[0].Actionable, "an UNKNOWN must be advisory, never actionable")
	require.Empty(t, findings[0].FixAction, "an UNKNOWN must not advertise a fix")
	require.Nil(t, findings[0].fix, "an UNKNOWN must carry no fix closure, or --fix would run it")
	require.Contains(t, findings[0].Detail, want)
}

// TestHookHostsSilentWithNoPinStore is the local-only user's run: no
// provision_cmd has ever run, so there is no hook-hosts directory, and doctor
// must say nothing — and must not even ASK, since the enumeration dials the
// daemon and walks every project on the box.
func TestHookHostsSilentWithNoPinStore(t *testing.T) {
	f := newHookHostFixture(t, "")
	f.opts.sessionInventory = func() ([]session.InstanceData, error) {
		t.Error("the session inventory was read for a home with no pin store")
		return nil, nil
	}
	require.NoDirExists(t, f.root)

	report := f.run(t)

	require.Empty(t, hookHostFindings(report), "a home with no pin store must produce no findings")
	require.Empty(t, hookHostPassDetails(report), "a home with no pin store must produce no rows either")
}

// TestHookHostsEmptyStoreIsSilent covers the store that exists but holds
// nothing — a box whose last remote session was torn down cleanly.
func TestHookHostsEmptyStoreIsSilent(t *testing.T) {
	f := newHookHostFixture(t, "")
	require.NoError(t, os.MkdirAll(f.root, 0o700))
	f.opts.sessionInventory = func() ([]session.InstanceData, error) {
		t.Error("the session inventory was read for an empty pin store")
		return nil, nil
	}

	report := f.run(t)

	require.Empty(t, hookHostFindings(report))
	require.Empty(t, hookHostPassDetails(report))
}

// TestHookHostsOrphanIsReportedAndRemoved is the acceptance case: a pin no
// session owns, in a fully readable inventory, is reported and collected.
func TestHookHostsOrphanIsReportedAndRemoved(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	f.daemonSees("some other session")
	f.storeRecords(t, "repoalpha", session.InstanceData{Title: "yet another session"})

	report := f.run(t)

	findings := hookHostFindings(report)
	require.Len(t, findings, 1)
	require.True(t, findings[0].Actionable, "a proven orphan is an actionable condition")
	require.Equal(t, "remove "+dir, findings[0].FixAction)
	require.True(t, findings[0].Fixed, "--fix must remove a proven orphan")
	require.NoError(t, findings[0].FixErr)
	require.NoDirExists(t, dir)
}

// TestHookHostsOrphanSurvivesAReportOnlyRun is the other half of the contract:
// the same proven orphan is left alone when --fix was not asked for.
func TestHookHostsOrphanSurvivesAReportOnlyRun(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	f.opts.Fix = false
	dir := f.pinDir(hookHostOwnerSlug)

	report := f.run(t)

	findings := hookHostFindings(report)
	require.Len(t, findings, 1)
	require.True(t, findings[0].Actionable)
	require.False(t, findings[0].Fixed)
	requireIntactPin(t, dir)
}

// --- One test per owner kind. ---------------------------------------------
//
// The four populations below all own their slug, and each reaches doctor by a
// different route. A live session may exist only in the daemon (its record is
// not necessarily checkpointed when the pin is written); an archived row, a
// mid-kill row and a kill tombstone all live on disk, and the tombstone
// deliberately outlives the daemon that wrote it.

// TestHookHostsLiveSessionOwnerIsSpared — a running session, present in the
// daemon's inventory and NOT yet on disk. This is the "just-created session"
// case: an inventory that read only instances.json would call this pin an orphan
// and delete a live session's host key.
func TestHookHostsLiveSessionOwnerIsSpared(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	f.opts.sessionInventory = func() ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title:      hookHostOwnerTitle,
			Liveness:   session.LiveRunning,
			InFlightOp: session.OpCreating,
		}}, nil
	}
	require.NoDirExists(t, f.instancesDir(), "the fixture must have no on-disk record for this session")

	report := f.run(t)

	require.Empty(t, hookHostFindings(report), "a live session's pin is not an orphan")
	require.Contains(t, hookHostPassDetails(report), dir+` is owned by session "`+hookHostOwnerTitle+`"`)
	requireIntactPin(t, dir)
}

// TestHookHostsArchivedSessionOwnerIsSpared — an archived row still owns its
// pin: a restore re-enters the same slug, and its teardown still has to reach
// the machine.
func TestHookHostsArchivedSessionOwnerIsSpared(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	f.storeRecords(t, "repoarchived", session.InstanceData{
		Title:    hookHostOwnerTitle,
		Liveness: session.LiveArchived,
	})

	report := f.run(t)

	require.Empty(t, hookHostFindings(report), "an archived session's pin is not an orphan")
	require.Contains(t, hookHostPassDetails(report), dir+` is owned by session "`+hookHostOwnerTitle+`"`)
	requireIntactPin(t, dir)
}

// TestHookHostsMidKillSessionOwnerIsSpared — a kill in flight is the worst
// moment to remove the pin: the teardown has not run yet, and it needs the key
// to reach the machine it is about to destroy.
func TestHookHostsMidKillSessionOwnerIsSpared(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	f.opts.sessionInventory = func() ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title:      hookHostOwnerTitle,
			Liveness:   session.LiveRunning,
			InFlightOp: session.OpKilling,
		}}, nil
	}

	report := f.run(t)

	require.Empty(t, hookHostFindings(report), "a session mid-kill still owns its pin")
	require.Contains(t, hookHostPassDetails(report), dir+` is owned by session "`+hookHostOwnerTitle+`"`)
	requireIntactPin(t, dir)
}

// TestHookHostsKillTombstoneOwnerIsSpared — the population #3454 could not fix
// and this check must not destroy. A tombstone is a persisted row with
// user_killed set, waiting for a daemon to rebuild its teardown and reap the
// machine; the daemon that wrote it may be long gone, so it appears ONLY on
// disk.
func TestHookHostsKillTombstoneOwnerIsSpared(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	f.storeRecords(t, "repotombstone", session.InstanceData{
		Title:      hookHostOwnerTitle,
		Liveness:   session.LiveDead,
		UserKilled: true,
	})

	report := f.run(t)

	require.Empty(t, hookHostFindings(report), "a kill tombstone still owns its pin until its reap succeeds")
	require.Contains(t, hookHostPassDetails(report), dir+` is owned by session "`+hookHostOwnerTitle+`"`)
	requireIntactPin(t, dir)
}

// TestHookHostsOwnerInAnotherProjectIsSpared — hook slugs are ONE namespace for
// the whole box. The owning session lives in a project that is not the one
// doctor's cwd would resolve to, and there are two projects on disk, so an
// enumeration that stopped at the first (or scoped itself to the cwd's repo)
// would delete a live session's pin.
func TestHookHostsOwnerInAnotherProjectIsSpared(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	f.storeRecords(t, "repofirst", session.InstanceData{Title: "unrelated session"})
	f.storeRecords(t, "reposecond", session.InstanceData{Title: hookHostOwnerTitle})

	report := f.run(t)

	require.Empty(t, hookHostFindings(report), "a pin owned by ANY project's session is not an orphan")
	requireIntactPin(t, dir)
}

// --- A failed read is not an empty result. --------------------------------
//
// These are the tests that matter most (#2874). Each injects one way the
// inventory can be incomplete and asserts the same two things: the check reports
// UNKNOWN, and --fix removes nothing.

// TestHookHostsUnreachableDaemonIsUnknown drives the PRODUCTION inventory
// reader against a home with no daemon socket, so the transport failure is real
// rather than a fake standing in for one.
func TestHookHostsUnreachableDaemonIsUnknown(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	f.opts.sessionInventory = nil // resolve the production default
	require.NoFileExists(t, filepath.Join(f.home, "daemon-http.sock"))

	report := f.run(t)

	requireUnknown(t, report, "the running daemon's session list could not be read")
	requireIntactPin(t, dir)
}

// TestHookHostsDaemonAnsweringWithAnErrorIsUnknown is the "up but does not
// answer" case, and it is distinct from an unreachable one: something IS
// listening on the daemon's socket, it accepts the request, and it returns an
// error envelope. Read as "no sessions", that daemon's every live pin is an
// orphan.
func TestHookHostsDaemonAnsweringWithAnErrorIsUnknown(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	serveHookHostSnapshot(t, f.home, func(daemon.SnapshotRequest) apiproto.Envelope {
		return apiproto.Envelope{Error: &apiproto.EnvelopeError{Message: "manager is not ready"}}
	})
	f.opts.sessionInventory = nil // resolve the production default

	report := f.run(t)

	requireUnknown(t, report, "manager is not ready")
	requireIntactPin(t, dir)
}

// TestHookHostsUnreadableInstancesFileIsUnknown makes one project's
// instances.json unreadable. It is a DIRECTORY rather than a chmod'd file so the
// read fails for root too — a test that silently passes as root proves nothing.
func TestHookHostsUnreadableInstancesFileIsUnknown(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	f.storeRecords(t, "repohealthy", session.InstanceData{Title: "unrelated session"})
	broken, err := config.RepoInstancesPath("repobroken")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(broken, 0o755))

	report := f.run(t)

	requireUnknown(t, report, "could not be read")
	requireIntactPin(t, dir)
}

// TestHookHostsUnlistableProjectStoreIsUnknown removes the ability to enumerate
// projects at all: the directory that holds one subdirectory per project is a
// regular file, so the walk fails with ENOTDIR — again, for root as well.
func TestHookHostsUnlistableProjectStoreIsUnknown(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	require.NoError(t, os.WriteFile(f.instancesDir(), []byte("not a directory"), 0o644))

	report := f.run(t)

	requireUnknown(t, report, "the projects holding session records could not be enumerated")
	requireIntactPin(t, dir)
}

// TestHookHostsUnparseableRecordsAreUnknown covers the corrupt file. The
// all-projects loader hands corrupt bytes back verbatim rather than failing, so
// without a parse check this repo would contribute zero titles and look like an
// empty project.
func TestHookHostsUnparseableRecordsAreUnknown(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	path, err := config.RepoInstancesPath("repocorrupt")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{ this is not json"), 0o644))

	report := f.run(t)

	requireUnknown(t, report, "could not be parsed")
	requireIntactPin(t, dir)
}

// TestHookHostsHomeMismatchIsUnknown guards the worst way to be wrong: an
// inventory that is COMPLETE, and about a different af home than the pins came
// from. Options.ConfigDir is injectable while the records and the daemon socket
// resolve from AGENT_FACTORY_HOME, so the two can be pointed apart.
func TestHookHostsHomeMismatchIsUnknown(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	report := f.run(t)

	requireUnknown(t, report, "not the home under inspection")
	requireIntactPin(t, dir)
}

// TestHookHostsUnlistablePinStoreIsUnknown is the failure one level up: the pin
// store itself cannot be listed, so doctor cannot even name a candidate.
func TestHookHostsUnlistablePinStoreIsUnknown(t *testing.T) {
	f := newHookHostFixture(t, "")
	require.NoError(t, os.WriteFile(f.root, []byte("not a directory"), 0o644))

	report := f.run(t)

	requireUnknown(t, report, "could not be listed")
}

// --- Fix-time rechecks. ---------------------------------------------------

// TestHookHostsFixRefusesWhenAnOwnerAppearsAfterDetection covers the window
// between detection and the fix: findings are applied after every check has run,
// and a create that recycles a killed session's title lands on exactly the slug
// queued for removal.
func TestHookHostsFixRefusesWhenAnOwnerAppearsAfterDetection(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	reads := 0
	f.opts.sessionInventory = func() ([]session.InstanceData, error) {
		reads++
		if reads == 1 {
			return nil, nil // detection: nothing owns the slug
		}
		return instancesTitled(hookHostOwnerTitle), nil // fix time: it does now
	}

	report := f.run(t)

	findings := hookHostFindings(report)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Fixed, "the removal must not have happened")
	require.ErrorContains(t, findings[0].FixErr, "now owns it")
	require.Greater(t, reads, 1, "the fix must re-read the inventory rather than trust detection")
	requireIntactPin(t, dir)
}

// TestHookHostsFixRefusesWhenTheInventoryBreaksAfterDetection — a guard that
// cannot RUN has not passed. The inventory was readable at detection and is not
// at fix time (the daemon restarted in between), and the removal must refuse
// rather than fall back to the answer it already has.
func TestHookHostsFixRefusesWhenTheInventoryBreaksAfterDetection(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	reads := 0
	f.opts.sessionInventory = func() ([]session.InstanceData, error) {
		reads++
		if reads == 1 {
			return nil, nil
		}
		return nil, errors.New("connection refused")
	}

	report := f.run(t)

	findings := hookHostFindings(report)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Fixed)
	require.ErrorContains(t, findings[0].FixErr, "connection refused")
	requireIntactPin(t, dir)
}

// TestHookHostsFixReadsTheInventoryOncePerRun pins the cost of the recheck: one
// re-read shared by every removal, not one per orphan. Three orphans, two reads
// total — detection and the first fix.
func TestHookHostsFixReadsTheInventoryOncePerRun(t *testing.T) {
	f := newHookHostFixture(t, "")
	for _, slug := range []string{"orphan-a", "orphan-b", "orphan-c"} {
		f.writePin(t, slug)
	}
	reads := 0
	f.opts.sessionInventory = func() ([]session.InstanceData, error) {
		reads++
		return nil, nil
	}

	report := f.run(t)

	require.Len(t, hookHostFindings(report), 3)
	for _, finding := range hookHostFindings(report) {
		require.True(t, finding.Fixed, "each proven orphan should have been removed")
	}
	require.Equal(t, 2, reads, "the inventory should be read once at detection and once at fix time")
}

// --- The shape gate. ------------------------------------------------------

// TestHookHostsNonPinEntriesAreReportedNeverRemoved — --fix must remove only a
// directory it has PROVEN af wrote. A stray file, a directory holding something
// besides the pinned key, and a directory holding nothing are each reported and
// each left exactly where they are.
func TestHookHostsNonPinEntriesAreReportedNeverRemoved(t *testing.T) {
	f := newHookHostFixture(t, "")
	require.NoError(t, os.MkdirAll(f.root, 0o700))

	stray := filepath.Join(f.root, "loose-file")
	require.NoError(t, os.WriteFile(stray, []byte("not a pin"), 0o600))
	extra := filepath.Join(f.root, "extra-content")
	require.NoError(t, os.MkdirAll(extra, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(extra, session.HookHostsPinFileName), []byte("k\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(extra, "notes.txt"), []byte("mine\n"), 0o600))
	empty := filepath.Join(f.root, "empty-dir")
	require.NoError(t, os.MkdirAll(empty, 0o700))

	report := f.run(t)

	findings := hookHostFindings(report)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Actionable)
	require.Nil(t, findings[0].fix)
	for _, name := range []string{"loose-file", "extra-content", "empty-dir"} {
		require.Contains(t, findings[0].Detail, name)
	}
	require.FileExists(t, stray)
	require.DirExists(t, extra)
	require.FileExists(t, filepath.Join(extra, "notes.txt"))
	require.DirExists(t, empty)
}

// TestHookHostsPinShapeRejectsASymlinkedKey — a symlink named known_hosts is not
// a pin af wrote, and following one would let a removal reach outside the store.
func TestHookHostsPinShapeRejectsASymlinkedKey(t *testing.T) {
	f := newHookHostFixture(t, "")
	dir := filepath.Join(f.root, "linked")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	target := filepath.Join(f.home, "elsewhere")
	require.NoError(t, os.WriteFile(target, []byte("k\n"), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, session.HookHostsPinFileName)))

	require.ErrorContains(t, hookHostPinShape(dir), "af did not write it")

	report := f.run(t)

	findings := hookHostFindings(report)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Actionable)
	require.DirExists(t, dir)
	require.FileExists(t, target)
}

// --- The production inventory reader. -------------------------------------

// TestHookHostsDaemonInventoryIsGlobalAndLocal drives the PRODUCTION default
// against a real Unix-socket server answering with the daemon's own envelope
// writer. It proves three things a fake cannot: the default is actually wired,
// it decodes the real response shape, and it asks for EVERY project (an empty
// RepoID) rather than the cwd's repo.
func TestHookHostsDaemonInventoryIsGlobalAndLocal(t *testing.T) {
	f := newHookHostFixture(t, hookHostOwnerSlug)
	dir := f.pinDir(hookHostOwnerSlug)
	var asked []daemon.SnapshotRequest
	serveHookHostSnapshot(t, f.home, func(req daemon.SnapshotRequest) apiproto.Envelope {
		asked = append(asked, req)
		return apiproto.Success(daemon.SnapshotResponse{Instances: instancesTitled(hookHostOwnerTitle)})
	})
	f.opts.sessionInventory = nil // resolve the production default

	report := f.run(t)

	require.Empty(t, hookHostFindings(report), "the daemon named this session, so its pin is owned")
	requireIntactPin(t, dir)
	require.Len(t, asked, 1)
	require.Empty(t, asked[0].RepoID, "the inventory must span every project, not one repo")
	require.False(t, asked[0].Live, "archived rows still own their pins, so they must not be filtered out")
}

// serveHookHostSnapshot binds a real Unix-socket HTTP server at the af home's
// daemon socket path — the exact path apiclient.New resolves — and answers
// POST /v1/Snapshot through apiproto.WriteEnvelope, the same primitive
// daemon/httpserver.go writes with.
func serveHookHostSnapshot(t *testing.T, home string, handle func(daemon.SnapshotRequest) apiproto.Envelope) {
	t.Helper()
	sockPath := filepath.Join(home, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.SnapshotRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, handle(req))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// --- Record decoding. -----------------------------------------------------

// TestHookHostRecordTitlesAcceptsBothStoredShapes pins the two forms a project's
// records arrive in — the unwrapped array, and the raw envelope the all-projects
// loader hands back when it could not unwrap the file itself — and pins that
// anything else is an ERROR rather than an empty project.
func TestHookHostRecordTitlesAcceptsBothStoredShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		want  []string
		fails bool
	}{
		{name: "array", raw: `[{"title":"one"},{"title":"two"}]`, want: []string{"one", "two"}},
		{name: "envelope", raw: `{"schema_version":1,"instances":[{"title":"one"}]}`, want: []string{"one"}},
		{name: "empty array", raw: `[]`},
		{name: "null", raw: `null`},
		{name: "blank", raw: ``},
		{name: "corrupt", raw: `{ nope`, fails: true},
		{name: "wrong element type", raw: `[1,2,3]`, fails: true},
		{name: "envelope with corrupt instances", raw: `{"instances":[1]}`, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hookHostRecordTitles(json.RawMessage(tc.raw))
			if tc.fails {
				require.Error(t, err, "an undecodable file must be an error, never an empty project")
				return
			}
			require.NoError(t, err)
			if len(tc.want) == 0 {
				require.Empty(t, got)
				return
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// TestHookHostOwnersSlugifyTitles pins the mapping the whole check turns on: a
// directory name is a slug, and a session owns the slug its TITLE produces.
func TestHookHostOwnersSlugifyTitles(t *testing.T) {
	f := newHookHostFixture(t, session.Slugify("Deploy API v2"))
	dir := f.pinDir(session.Slugify("Deploy API v2"))
	f.daemonSees("Deploy API v2")

	report := f.run(t)

	require.Empty(t, hookHostFindings(report))
	requireIntactPin(t, dir)
}
