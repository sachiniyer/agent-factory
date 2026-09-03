package daemon

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The #3782 item 2 regression suite: the same function as item 1, at DAEMON
// START rather than on the poll goroutine — buildRootAgentSnapshot, reached from
// newManagerShellForDaemon under NewManager.
//
// It is a different lifecycle with a different consequence, which is why #3760's
// test installed its stall AFTER NewManager and why this is its own change: item
// 1 is a daemon whose poll loop is stuck, this is NO DAEMON AT ALL.
//
// THE DECISION, and it is a product one rather than a port. A daemon that
// refuses to start because one configured checkout sits on a stalled mount takes
// every other session on the box down with it — every session's status, every
// Lost recovery, every scheduled task, for a repo that may have no live sessions
// at all. Refusing to start is not the honest outcome here; STARTING DEGRADED
// AND SAYING SO is. So the boot probe is bounded, the candidate is recorded
// UNKNOWN, and it keeps being retried: the ensure sweep retries the path forever
// for the CREATE (#1122), and the heal pass retries the resolution for the DEDUP
// SET, which is what stops "unknown" from quietly becoming "absent".
//
// Scope, stated because the test could otherwise be read as proving more than it
// does: buildRootAgentSnapshot has OTHER unbounded probes — projectRootAgentLayers
// resolves every REGISTERED PROJECT's recorded root through config.RepoFromPath,
// and ResolveRegisteredProjectRepoID runs on context.Background(). A registered
// project on the same stalled mount still wedges daemon start, and that is not
// this bound. Filed separately; the tests here stall a root_agents path only.

// stallBootLegacyResolution stalls one configured path's resolution for the
// START-OF-DAY snapshot, before any manager exists to hold a seam. Same shape as
// the poll-side stall: it ends only when the caller's own context ends it, which
// is the property under test — a stand-in that returned after a fixed sleep would
// pass whether or not production bounded anything.
//
// The returned release exists for teardown. An unbounded boot can never end this
// stall, so on the red the constructing goroutine is still inside it when the
// test finishes; release lets it out so the package can finish reporting rather
// than sitting until the 20-minute timeout.
func stallBootLegacyResolution(t *testing.T, repoPath string) (release func()) {
	t.Helper()
	prev := legacyRootRepoFromPath
	released := make(chan struct{})
	var once sync.Once
	legacyRootRepoFromPath = func(ctx context.Context, path string) (*config.RepoContext, error) {
		if filepath.Clean(path) != filepath.Clean(repoPath) {
			return prev(ctx, path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-released:
			return prev(ctx, path)
		}
	}
	t.Cleanup(func() { legacyRootRepoFromPath = prev })
	return func() { once.Do(func() { close(released) }) }
}

// TestStalledLegacyPathDoesNotWedgeDaemonStart is the claim, stated as narrowly
// as it is true: a root_agents path whose repository resolution never answers
// must not stop the daemon from coming up.
//
// NewManager runs on its own goroutine so the test can OUTLIVE a construction
// that never returns — on master it never does, and a direct call would simply
// hang the test rather than report anything.
func TestStalledLegacyPathDoesNotWedgeDaemonStart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	release := stallBootLegacyResolution(t, repoPath)

	type built struct {
		manager *Manager
		err     error
	}
	done := make(chan built, 1)
	var constructing sync.WaitGroup
	constructing.Add(1)
	go func() {
		defer constructing.Done()
		manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
		done <- built{manager, err}
	}()
	// Registered AFTER the stall's own cleanup, so LIFO runs this first: the
	// seam is a package var, and restoring it while a wedged goroutine is still
	// reading it is a data race — measured on the fail-first run, where the
	// 60-second verdict below hit t.Fatal with the construction still inside
	// the stall. Releasing and JOINING first means no goroutine is in the seam
	// when it is put back. It runs after the verdict either way, so it cannot
	// help the assertion pass.
	t.Cleanup(func() {
		release()
		joined := make(chan struct{})
		go func() { constructing.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(30 * time.Second):
			t.Error("the constructing goroutine never returned after the stall was released; the seam cannot be restored safely")
		}
	})

	var result built
	select {
	case result = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("daemon start is wedged in the start-of-day root-agent snapshot: with one configured root_agents " +
			"path's repository resolution unanswered, NewManager never returned, so the daemon serves no session on " +
			"the box at all (#3782 item 2)")
	}
	if result.err != nil {
		t.Fatalf("NewManager failed rather than starting degraded: %v", result.err)
	}

	// And it came up saying so: the candidate is recorded UNKNOWN, not dropped.
	layers := result.manager.rootAgentLayers.Load()
	if !layers.legacy.unknownPaths[repoPath] {
		t.Fatalf("the unanswered path must be recorded as unknown so it keeps being retried, got unknownPaths=%v", layers.legacy.unknownPaths)
	}
	if len(layers.legacy.ids) != 0 {
		t.Fatalf("an unanswered probe must PROVE nothing, got ids=%v", layers.legacy.ids)
	}
}

// TestUnansweredBootProbeIsNotAbsentFromTheDedupSet pins the half that keeps the
// bound from being a regression, at the function that decides it.
//
// There is nothing to carry forward at boot — no previous pass — so item 1's
// rule has no purchase here and the repo would fall out of the dedup set
// entirely. The singleton sweep resolves with a NIL legacy layer
// (ensureSingletonRootAgent), so that is not a delayed root, it is the WRONG
// root: started without the root_agents entry the user wrote, because a probe on
// an unrelated mount was slow.
//
// The provisional identity is config.LegacyRootAgentForRepo's own idiom for the
// same question, and its limit is pinned too: it is the identity a MAIN-ROOTED
// checkout at that path has, so a linked-worktree path is a miss rather than a
// mismatch.
func TestUnansweredBootProbeIsNotAbsentFromTheDedupSet(t *testing.T) {
	const path = "/repos/alpha"
	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{path: {}}
	unanswered := func(string) (*config.RepoContext, error) {
		return nil, fmt.Errorf("failed to get git repo root for %s: %w", path, config.ErrRepoProbeUnanswered)
	}

	got := legacyRepoIDSet(cfg, unanswered, legacyRepoDedup{})

	if !maps.Equal(got.unknownPaths, map[string]bool{path: true}) {
		t.Fatalf("an unanswered path must be latched for retry, got unknownPaths=%v", got.unknownPaths)
	}
	provisional := config.RepoIDFromRoot(filepath.Clean(path))
	if !maps.Equal(got.unknownIDs, map[string]bool{provisional: true}) {
		t.Fatalf("an unanswered first probe must contribute the provisional main-root identity, got unknownIDs=%v want %v",
			got.unknownIDs, provisional)
	}
	if !got.covers(provisional) {
		t.Fatal("the dedup view must COVER the provisional identity: an unknown that reads as absent lets the singleton " +
			"sweep start the root without the legacy layer (#3315, reached through a timeout)")
	}
	if len(got.ids) != 0 {
		t.Fatalf("a probe that never answered must PROVE nothing, got ids=%v", got.ids)
	}
	// The provisional half is a guess and must never masquerade as a resolution.
	if !got.covers(provisional) || got.ids[provisional] {
		t.Fatalf("the provisional identity must cover without being proven, got ids=%v", got.ids)
	}
}

// TestNotYetClonedLegacyPathDoesNotLatchTheRetry is the bound on the bound, and
// it is what keeps #1122 from paying for #3782.
//
// A root_agents entry naming a repo that has not been cloned yet is NORMAL and
// may sit that way for weeks. If that latched the heal pass's unknown set, an
// ordinary configuration would pin the shared heal backoff at its maximum
// forever and slow the registry and personal-config retries that share the
// curve. It does not, and the reason is the classifier rather than a special
// case here: `git -C <missing> rev-parse` EXITS 128, which is an answer, so the
// failure carries no ErrRepoProbeUnanswered and takes the verdict branch.
//
// Measured rather than assumed — this drives the real config.RepoFromPathContext
// against a path that does not exist.
func TestNotYetClonedLegacyPathDoesNotLatchTheRetry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	missing := filepath.Join(t.TempDir(), "not-cloned-yet")
	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{missing: {}}

	got := legacyRepoIDSet(cfg, resolveLegacyRootRepo, legacyRepoDedup{})

	if len(got.unknownPaths) != 0 {
		t.Fatalf("a path git ANSWERED about must not latch the heal retry — #1122's not-yet-cloned entry would pin the "+
			"shared heal backoff at its maximum for the life of the daemon; got unknownPaths=%v", got.unknownPaths)
	}
	if len(got.unknownIDs) != 0 {
		t.Fatalf("a verdict must not produce a provisional identity, got unknownIDs=%v", got.unknownIDs)
	}
	if len(got.ids) != 0 || len(got.byPath) != 0 {
		t.Fatalf("a path that does not resolve is simply not in the dedup set, got ids=%v byPath=%v", got.ids, got.byPath)
	}
	if got.covers(config.RepoIDFromRoot(filepath.Clean(missing))) {
		t.Fatal("an ANSWERED absence must not be covered: the singleton sweep owns a repo no legacy path resolves to")
	}
}

// TestBootUnknownLegacyPathIsRetriedUntilKnown pins the retry, which is what
// makes "starts degraded" honest rather than "starts wrong and stays wrong".
//
// The snapshot's other unknowns (an unlistable registry, an unloadable personal
// config) already make healRootAgentLayers run; a legacy path whose probe never
// answered must do the same, or the boot-time guess is the answer for the life
// of the daemon and a path that came back is never noticed. Once it answers, the
// real identity replaces the provisional one and the latch clears.
func TestBootUnknownLegacyPathIsRetriedUntilKnown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	rid := repoID(t, repoPath)

	restore := unansweredLegacyRootResolution(t, repoPath)
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !manager.rootAgentLayers.Load().legacy.unknownPaths[repoPath] {
		t.Fatal("fixture: the boot probe must be recorded unknown, or there is no latch to retry")
	}

	// The mount answers again. Nothing else about the snapshot is unknown, so
	// the legacy latch is the only thing that can bring the heal pass back.
	restore()
	manager.ensureRootAgentsAndWait()

	layers := manager.rootAgentLayers.Load()
	if !layers.legacy.ids[rid] {
		t.Fatal("a legacy path whose boot probe never answered must be re-resolved by the heal pass once it answers; " +
			"without that latch the provisional guess stands for the life of the daemon (#3782 item 2)")
	}
	if len(layers.legacy.unknownPaths) != 0 {
		t.Fatalf("the latch must clear once the probe answers, got unknownPaths=%v", layers.legacy.unknownPaths)
	}
	if len(layers.legacy.unknownIDs) != 0 {
		t.Fatalf("the provisional identity must be retired by the real one, got unknownIDs=%v", layers.legacy.unknownIDs)
	}
}
