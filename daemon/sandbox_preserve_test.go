package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// sandboxProbeServer stands in for an in-sandbox `af agent-server` on the three
// postures recovery has to tell apart. The alive answer is what decides which
// branch the daemon takes; the archive handler is what proves whether the push
// happened before anything was reaped.
type sandboxProbeServer struct {
	url          string
	archiveCalls atomic.Int32
	archiveFails atomic.Bool
	branch       atomic.Value // string
	unreachable  atomic.Bool
}

func newSandboxProbeServer(t *testing.T, branch string) *sandboxProbeServer {
	t.Helper()
	s := &sandboxProbeServer{}
	s.branch.Store(branch)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if s.unreachable.Load() {
			// A transport failure — NOT af's not-provisioned sentinel. This is what a
			// genuinely destroyed container looks like, and also what a broken network
			// path to a live one looks like, which is exactly why it cannot license a
			// reap.
			http.Error(w, "transport unavailable", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/v1/agent/snapshot":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "capture failed"},
			})
		case "/v1/agent/alive":
			// Answered, agent gone: reachable. The sandbox is still there.
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alive": false}})
		case "/v1/agent/archive":
			s.archiveCalls.Add(1)
			if s.archiveFails.Load() {
				http.Error(w, "push rejected by origin", http.StatusInternalServerError)
				return
			}
			b, _ := s.branch.Load().(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"branch": b}})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

// REACHABLE: the sandbox answers that its agent is gone, so it is still there and
// may hold commits nothing else has a copy of. Recovery must push BEFORE it
// replaces anything, and must record the branch that push reports (#2923/#2925).
//
// On master this reaps immediately and never calls archive, so the replacement
// clones from origin — three hours behind — and `RestoreBranch` stays empty,
// which makes both sandbox runtimes skip the restore fetch and land on the
// repository's DEFAULT branch. That is the reported bug in #2959.
func TestRestoreSession_ReachableSandboxIsPushedBeforeItIsReplaced(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/session-branch")
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "reachable", srv.url, session.Lost)

	if inst.GetBranch() != "" {
		t.Fatalf("precondition: a never-archived sandbox session has no branch recorded, got %q", inst.GetBranch())
	}

	if _, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "reachable", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}

	if got := srv.archiveCalls.Load(); got != 1 {
		t.Fatalf("archive calls = %d, want 1: a reachable sandbox must have its work pushed before it is "+
			"replaced — recovery re-clones from origin, so anything unpushed is destroyed by the reap", got)
	}
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1 after a successful push", got)
	}
	if got := inst.GetBranch(); got != "af/session-branch" {
		t.Fatalf("recorded branch = %q, want the branch the SANDBOX reported: an empty one makes the "+
			"replacement skip the restore fetch and clone the repository's default branch", got)
	}
	if rec := recordFor(t, repoID, "reachable"); rec == nil || rec.Branch != "af/session-branch" {
		t.Fatalf("persisted record = %+v, want the branch durable the instant the push made it so", rec)
	}
}

// REACHABLE, PUSH FAILS: refuse. The archive path aborts to Lost when its push
// fails rather than tearing down anyway; recovery must do the same, because the
// sandbox is the only place that work exists.
func TestRestoreSession_ReachableSandboxIsNotReplacedWhenThePushFails(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/session-branch")
	srv.archiveFails.Store(true)
	_, backend := registerStartedRemote(t, manager, repoID, repoPath, "push-fails", srv.url, session.Lost)

	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "push-fails", RepoID: repoID})

	if err == nil {
		t.Fatal("a restore whose pre-reap push failed reported success; the sandbox would have been " +
			"replaced and its unpushed commits destroyed")
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("recover calls = %d, want 0: nothing may be replaced when the push did not land", got)
	}
	if !strings.Contains(err.Error(), "--force-reap") {
		t.Fatalf("the refusal must name the command that releases it (#2917), got: %v", err)
	}
}

// INDETERMINATE: the probe cannot be answered at all. Unreachable is not gone —
// the sandbox may be live behind a broken network path, still holding work — so
// recovery refuses, reaps nothing, and says how to override.
//
// Note the discriminator: this is a TRANSPORT error, which is what a genuinely
// destroyed container also produces. Keying the refusal off af's
// not-provisioned sentinel instead would strand every truly-gone sandbox forever.
func TestRestoreSession_IndeterminateSandboxIsNotReplaced(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/session-branch")
	srv.unreachable.Store(true)
	_, backend := registerStartedRemote(t, manager, repoID, repoPath, "indeterminate", srv.url, session.Lost)

	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "indeterminate", RepoID: repoID})

	if err == nil {
		t.Fatal("a restore against an unreachable sandbox reported success: it would have replaced a " +
			"sandbox that may still hold unpushed work, and cloned the default branch")
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("recover calls = %d, want 0: an unanswerable probe authorizes nothing", got)
	}
	if got := srv.archiveCalls.Load(); got != 0 {
		t.Fatalf("archive calls = %d, want 0: there is nothing to push to when the sandbox cannot be reached", got)
	}
	if !strings.Contains(err.Error(), "--force-reap") {
		t.Fatalf("the refusal must name the command that releases it (#2917), got: %v", err)
	}
}

// The escape hatch. Without it the refusal above is a dead end for a sandbox the
// operator knows is gone, which is the #2917 shape. It is per-session and
// per-command: this flag, this title, this invocation.
func TestRestoreSession_ForceReapDoesNotPushWhatItIsDiscarding(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/session-branch")
	_, backend := registerStartedRemote(t, manager, repoID, repoPath, "forced", srv.url, session.Lost)
	// A branch is already on record (an earlier archive), so the replacement has
	// somewhere correct to land without needing the push to discover one.
	inst := manager.instances[daemonInstanceKey(repoID, "forced")]
	inst.SetSandboxBranch("af/known-branch")
	// Durable, not just in memory: authorizing a reap reads the stored record.
	manager.persistInstance(repoID, inst)

	if _, _, err := manager.RestoreSession(RestoreSessionRequest{
		Title: "forced", RepoID: repoID, ForceReap: true,
	}); err != nil {
		t.Fatalf("RestoreSession(--force-reap): %v", err)
	}

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1: --force-reap is the operator overriding the refusal", got)
	}
	if got := srv.archiveCalls.Load(); got != 0 {
		t.Fatalf("archive calls = %d, want 0: --force-reap promises to replace the sandbox WITHOUT "+
			"pushing, and archive snapshots uncommitted files into a commit before pushing — so a push "+
			"here uploads exactly the material the operator chose to discard", got)
	}
}

// The advertised escape hatch has to actually release the guard that names it.
// The indeterminate refusal tells the operator to retry with --force-reap; if
// that retry takes the same branch and refuses again, the message is a dead end
// and the guard is the #2917 defect wearing a helpful sentence.
func TestRestoreSession_ForceReapReleasesTheIndeterminateRefusal(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/session-branch")
	srv.unreachable.Store(true)
	_, backend := registerStartedRemote(t, manager, repoID, repoPath, "forced-unknown", srv.url, session.Lost)
	inst := manager.instances[daemonInstanceKey(repoID, "forced-unknown")]
	inst.SetSandboxBranch("af/known-branch")
	manager.persistInstance(repoID, inst)

	if _, _, err := manager.RestoreSession(RestoreSessionRequest{
		Title: "forced-unknown", RepoID: repoID, ForceReap: true,
	}); err != nil {
		t.Fatalf("the refusal names --force-reap as its release, so that retry must work: %v", err)
	}
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}
}

// --force-reap does NOT override the branch requirement, and that boundary is
// the flag's promise: it discards what the sandbox never pushed, which is the
// operator's call. Landing on the default branch discards work they had already
// pushed, which they never agreed to.
func TestRestoreSession_ForceReapStillRefusesWhenTheBranchIsUnknown(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "")
	srv.unreachable.Store(true)
	_, backend := registerStartedRemote(t, manager, repoID, repoPath, "no-branch", srv.url, session.Lost)

	_, _, err := manager.RestoreSession(RestoreSessionRequest{
		Title: "no-branch", RepoID: repoID, ForceReap: true,
	})

	if err == nil {
		t.Fatal("forced a replacement with no branch on record: it clones the repository's default " +
			"branch, stranding even work the session had already pushed")
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("recover calls = %d, want 0", got)
	}
	if !strings.Contains(err.Error(), "af sessions kill") {
		t.Fatalf("a refusal that force cannot release must name the alternative that ends it, got: %v", err)
	}
}

// The pre-reap bound must sit ABOVE the transport's, not below it.
//
// The in-sandbox archive handler takes no context, so a client that gives up does
// not stop the git work it started. If the daemon's wait expired first it would
// release the session's op lock while that push was still staging and committing,
// and the retry loop would start a SECOND archive against the same worktree.
// Ordering them the other way makes the client's own deadline end the attempt, so
// a return here proves the call has stopped trying.
func TestSandboxPushTimeoutOutlastsTheTransportBudget(t *testing.T) {
	if sandboxPushTimeout <= session.AgentArchiveCallTimeout {
		t.Fatalf("sandboxPushTimeout (%s) must exceed the transport budget (%s): a caller that gives up "+
			"first leaves the in-sandbox git work running unbounded, and the retry starts a second "+
			"archive against the same worktree",
			sandboxPushTimeout, session.AgentArchiveCallTimeout)
	}
}

// A forced reap may not be authorized by a branch that exists only in memory.
// A partial archive populates it without recording it (the push landed, the
// teardown failed, the write was best-effort), and destroying the sandbox on that
// basis means a crash before the settlement leaves nothing pointing at the pushed
// work.
func TestRestoreSession_ForceReapRefusesABranchThatIsNotOnDisk(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/session-branch")
	srv.unreachable.Store(true)
	_, backend := registerStartedRemote(t, manager, repoID, repoPath, "volatile", srv.url, session.Lost)
	// In memory only — exactly what a partial archive whose persist failed leaves.
	manager.instances[daemonInstanceKey(repoID, "volatile")].SetSandboxBranch("af/only-in-memory")

	_, _, err := manager.RestoreSession(RestoreSessionRequest{
		Title: "volatile", RepoID: repoID, ForceReap: true,
	})

	if err == nil {
		t.Fatal("forced a reap on a branch that was never written to disk: a crash after the " +
			"replacement leaves the record branchless and the pushed work stranded")
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("recover calls = %d, want 0", got)
	}
	if !strings.Contains(err.Error(), "only in memory") {
		t.Fatalf("the refusal must say WHY it refused, got: %v", err)
	}
}

// A record on disk is not automatically a record of the CURRENT branch. The
// partial archive this whole guard is about pushes a NEW branch, updates the
// instance in memory, then fails its teardown and its best-effort persist — so
// the disk can still hold an OLDER nonempty branch from a previous archive.
//
// Accepting non-emptiness authorizes destroying the sandbox on the strength of a
// record pointing somewhere else: if recovery then fails, the next restore
// returns to the stored branch and strands exactly the work the archive just
// pushed. Which is the outcome this PR exists to prevent, reached by a different
// road (Codex on #2967).
func TestRestoreSession_ForceReapRefusesAStalePersistedBranch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/session-branch")
	srv.unreachable.Store(true)
	_, backend := registerStartedRemote(t, manager, repoID, repoPath, "stale", srv.url, session.Lost)
	inst := manager.instances[daemonInstanceKey(repoID, "stale")]

	// An earlier archive recorded this branch durably.
	inst.SetSandboxBranch("af/older-archive")
	manager.persistInstance(repoID, inst)
	// A later archive pushed a NEW branch and updated memory, then failed before
	// its persist landed. Disk still says af/older-archive.
	inst.SetSandboxBranch("af/newly-pushed")

	_, _, err := manager.RestoreSession(RestoreSessionRequest{
		Title: "stale", RepoID: repoID, ForceReap: true,
	})

	if err == nil {
		t.Fatal("forced a reap against a stale stored branch: a crash after the replacement sends the " +
			"next restore to af/older-archive and strands the work pushed to af/newly-pushed")
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("recover calls = %d, want 0", got)
	}
	// The refusal has to name BOTH branches, or an operator cannot tell which
	// one holds their work and which one the restore would go back to.
	for _, want := range []string{"af/older-archive", "af/newly-pushed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must name %q so the operator can see what diverged, got: %v", want, err)
		}
	}
}
