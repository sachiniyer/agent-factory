package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The #3714 regression suite: a create-boundary identity refusal (#3366/#3711)
// has to be legible on the surface that answers "will this repo's root
// materialize", because that surface is what every send-prompt, watch and
// monitor delivery to `root` reads. Before this, the refusal lived only in the
// daemon log: the verdict was snapshot-derived, the snapshot still held a
// healthy projectRoots binding, and delivery waited out targetDeliverWait for a
// root that was never coming and then blamed a tmux blip.
//
// Hermetic on the rootagent_create_identity_test.go rules — temp
// AGENT_FACTORY_HOME, the in-process fake backend, no real daemon and no tmux —
// and they reuse that file's swap/registration fixtures, since this is the same
// class one surface further out.

// TestRootDeliveryNamesTheCreateBoundaryMismatch is the issue as a user meets
// it. A different clone occupies the registered path, so every create is
// refused; a delivery to `root` must answer with THAT — the mismatch and its
// rebind-then-restart remedy — instead of waiting out the recreation bound and
// reporting a momentarily-absent tmux, which is neither the cause nor a remedy.
func TestRootDeliveryNamesTheCreateBoundaryMismatch(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	// Long enough that waiting it out is unmistakable, short enough that a
	// regression fails the test rather than hanging it.
	previousWait := targetDeliverWait
	targetDeliverWait = 2 * time.Second
	t.Cleanup(func() { targetDeliverWait = previousWait })
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerEnabledRootProject(t, repoPath, "/opt/refused")

	// Boot with the ORIGINAL checkout in place: this is what binds the path,
	// and it is why the snapshot keeps reading healthy after the swap.
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rid := repoID(t, repoPath)
	swapCheckoutForStrangersClone(t, repoPath)

	manager.EnsureRootAgents()
	if len(*seen) != 0 {
		t.Fatalf("the swapped checkout must be refused at the create boundary, got %d creates", len(*seen))
	}

	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if repo.ID != rid {
		t.Fatalf("the repo ID is the hash of the resolved root PATH, so a clone reusing the path keeps it; got %s, want %s — without that the whole class this fixes cannot arise", repo.ID, rid)
	}

	started := time.Now()
	_, _, handled, derr := manager.deliverToReemergingRoot(repo, DeliverPromptRequest{Title: session.RootSessionTitle, Prompt: "ping"})
	elapsed := time.Since(started)

	if !handled {
		t.Fatalf("a delivery to a root that cannot be created must be answered here, not fall through to the reserved-name guard's add-it-to-root_agents advice")
	}
	if derr == nil {
		t.Fatalf("the delivery must fail: no root agent is coming while a stranger's clone holds the registered path")
	}
	msg := derr.Error()
	if strings.Contains(msg, "being recreated") {
		t.Fatalf("the root is not being recreated — every create is refused on identity; got: %v", derr)
	}
	if elapsed >= targetDeliverWait/2 {
		t.Fatalf("the refusal is known up front, so nothing may wait on a root that is never coming; took %v of a %v bound", elapsed, targetDeliverWait)
	}
	if !strings.Contains(msg, "does not carry the project's registry marker") {
		t.Fatalf("the delivery error must name the checkout mismatch as the cause; got: %v", derr)
	}
	if !strings.Contains(msg, "af projects rebind "+project.ID) {
		t.Fatalf("the delivery error must prescribe the rebind, naming the project to rebind; got: %v", derr)
	}
	if !strings.Contains(msg, "restart the daemon") {
		t.Fatalf("a rebind alone leaves the running snapshot holding the marker id it captured at start, so the restart must be prescribed too; got: %v", derr)
	}
	if !strings.Contains(msg, "keeps retrying on its ensure cadence") {
		t.Fatalf("the refusal is live and repeating, not a condition established at daemon start; the message must say so; got: %v", derr)
	}
}

// TestCreateBoundaryRefusalClearsWhenTheOriginalCheckoutReturns is the other
// half, and the reason the record is an OUTCOME rather than a latch or a
// counter: a refusal that heals on the sweep's backoff — a transiently
// unreadable marker, an original checkout coming back — must leave the verdict
// exactly where it found it. Nothing here restarts the daemon.
func TestCreateBoundaryRefusalClearsWhenTheOriginalCheckoutReturns(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	previousBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = previousBase })
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerEnabledRootProject(t, repoPath, "/opt/returned")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rid := repoID(t, repoPath)
	restore := swapCheckoutForStrangersClone(t, repoPath)

	manager.EnsureRootAgents()
	refused := manager.rootAgentMaterializeVerdictFor(rid)
	if refused.reason != rootAgentProjectUnresolved || !refused.rootIdentityMismatch {
		t.Fatalf("the standing refusal must reach the verdict, got reason %d (mismatch=%v)", refused.reason, refused.rootIdentityMismatch)
	}

	restore()
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("the registered checkout is back and carries its marker, so the root must be created, got %d creates", len(*seen))
	}
	healed := manager.rootAgentMaterializeVerdictFor(rid)
	if healed.reason != rootAgentWillMaterialize {
		t.Fatalf("a refusal the sweep has since disproven must not survive it: verdict reason %d, want rootAgentWillMaterialize", healed.reason)
	}
	if detail := rootAgentUnavailableDetail(healed); detail != "" {
		t.Fatalf("a materializing root has nothing to explain, got: %s", detail)
	}
}

// TestCreateBoundaryUnreadableMarkerVerdictPrescribesNoRebind pins the class
// through to the clause. A marker that cannot be READ leaves identity
// unknowable, which is neither absence nor a proven mismatch, and the original
// checkout may be sitting right there — so the verdict must reach for the
// unreadable-marker wording, not the rebind. Carrying the outcome class on the
// refusal is what makes that possible; deriving it from the message later would
// be a second classifier, and this is what fails if the mapping drifts.
func TestCreateBoundaryUnreadableMarkerVerdictPrescribesNoRebind(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not make a file unreadable for root")
	}
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerEnabledRootProject(t, repoPath, "/opt/unreadable")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rid := repoID(t, repoPath)
	marker := soleCheckoutMarker(t, repoPath)
	if err := os.Chmod(marker, 0o000); err != nil {
		t.Fatalf("chmod marker: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(marker, 0o644) })
	if _, err := os.ReadFile(marker); err == nil {
		t.Skip("environment does not enforce file permission bits; the unreadable fixture cannot exist here")
	}

	manager.EnsureRootAgents()

	verdict := manager.rootAgentMaterializeVerdictFor(rid)
	if verdict.reason != rootAgentProjectUnresolved || !verdict.rootMarkerUnreadable {
		t.Fatalf("an unreadable marker must reach the verdict as unknowable identity, got reason %d (markerUnreadable=%v, mismatch=%v)", verdict.reason, verdict.rootMarkerUnreadable, verdict.rootIdentityMismatch)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if !strings.Contains(detail, "cannot be read") {
		t.Fatalf("the clause must name the readability problem as the remedy, got: %s", detail)
	}
	if strings.Contains(detail, "rebind") {
		t.Fatalf("an unreadable marker is not a proven mismatch; rebinding over a possibly-original checkout is destructive advice, got: %s", detail)
	}
}

// TestCreateBoundaryRefusalAfterTheReapReachesTheVerdict pins the SECOND proof
// arm. The pre-reap check protects the record, but the proof the create runs on
// is the one after the reap's blocking work (#3711 review), and a swap landing
// in that window is exactly the durable mismatch a user then cannot see. Both
// arms record because both go through one seam; without that, this refusal
// would be invisible again.
func TestCreateBoundaryRefusalAfterTheReapReachesTheVerdict(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerEnabledRootProject(t, repoPath, "/opt/midpass")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rid := repoID(t, repoPath)
	manager.EnsureRootAgents()
	first := findRootInstance(t, manager, repoPath)
	if first == nil {
		t.Fatalf("root instance missing after the first ensure")
	}
	if verdict := manager.rootAgentMaterializeVerdictFor(rid); verdict.reason != rootAgentWillMaterialize {
		t.Fatalf("a proven create must leave no refusal behind, got reason %d", verdict.reason)
	}
	first.SetStatusForTest(session.Dead)

	// The checkout is the project's own when the pre-reap check reads it, and a
	// stranger's by the time the create's own proof runs.
	swapped := false
	rootCreateVerifyHookForTest = func() {
		if swapped {
			return
		}
		swapped = true
		swapCheckoutForStrangersClone(t, repoPath)
	}
	t.Cleanup(func() { rootCreateVerifyHookForTest = nil })

	manager.EnsureRootAgents()

	if !swapped {
		t.Fatalf("the create never went through createVerifiedRoot, so this pins nothing")
	}
	verdict := manager.rootAgentMaterializeVerdictFor(rid)
	if verdict.reason != rootAgentProjectUnresolved || !verdict.rootIdentityMismatch {
		t.Fatalf("a refusal from the post-reap proof must reach the verdict too, got reason %d (mismatch=%v)", verdict.reason, verdict.rootIdentityMismatch)
	}
}
