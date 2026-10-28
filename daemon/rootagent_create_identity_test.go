package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The #3366 create-boundary identity re-check. The snapshot binds a repo ID to
// a create path once — at boot here, which is the case #3334 deliberately does
// not cover — and every later create through that binding trusted the path for
// the rest of the daemon run. These pin that a create re-proves the checkout,
// that only a CREATE pays for it, and that the refusal is retryable rather than
// a latch.
//
// Hermetic on the same rules as the rest of the root-agent tests: temp
// AGENT_FACTORY_HOME, the in-process fake backend, no real daemon and no tmux.

// swapCheckoutForStrangersClone replaces the registered checkout at repoPath
// with a DIFFERENT clone: a real git repository at the same path, carrying no
// marker for the registered project. It is the shape the whole issue is about —
// the original removed, the path reused — and the returned restore puts the
// original checkout back.
func swapCheckoutForStrangersClone(t *testing.T, repoPath string) (restore func()) {
	t.Helper()
	hidden := repoPath + ".original"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide the registered checkout: %v", err)
	}
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("git init the stranger's clone: %v", err)
	}
	return func() {
		t.Helper()
		if err := os.RemoveAll(repoPath); err != nil {
			t.Fatalf("remove the stranger's clone: %v", err)
		}
		if err := os.Rename(hidden, repoPath); err != nil {
			t.Fatalf("restore the registered checkout: %v", err)
		}
	}
}

// captureRootEnsureWarnings redirects the daemon WARNING log for one test. The
// refusals below are logged, never returned to a caller, so the log is the only
// place the diagnosis a user acts on survives.
func captureRootEnsureWarnings(t *testing.T) *logCapture {
	t.Helper()
	return captureWarnings(t)
}

// registerEnabledRootProject registers repoPath and turns its root agent on
// through the personal singleton, the shape with no legacy root_agents entry —
// so every create below goes through the singleton sweep.
func registerEnabledRootProject(t *testing.T, repoPath, program string) config.Project {
	t.Helper()
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \""+program+"\"")
	return project
}

// TestSingletonRootCreateRefusesSwappedCheckout is the #3366 regression, on the
// BOOT-RESOLVED case: the project resolved when the daemon started, so the
// snapshot bound its path with the marker verified nowhere — #3334's
// verification runs only on re-attribution. The checkout is then removed and a
// different clone takes the path, and the very next create started the
// autonomous root there, under the registered project's personal layer, in a
// checkout nothing had ever proven was the project's.
func TestSingletonRootCreateRefusesSwappedCheckout(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerEnabledRootProject(t, repoPath, "/opt/hijacked")

	// Boot with the ORIGINAL checkout in place: this is what binds the path.
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	swapCheckoutForStrangersClone(t, repoPath)
	warnings := captureRootEnsureWarnings(t)

	manager.ensureRootAgentsAndWait()

	if len(*seen) != 0 {
		t.Fatalf("a create must re-prove the bound path's checkout, not trust the boot binding; got %d creates into a clone that carries no marker for project %s", len(*seen), project.ID)
	}
	if inst := findRootInstance(t, manager, repoPath); inst != nil {
		t.Fatalf("no root instance may be registered for a checkout that failed identity")
	}
	logged := warnings.String()
	if !strings.Contains(logged, project.ID) || !strings.Contains(logged, "rebind") {
		t.Fatalf("the refusal must name the project and prescribe the rebind, got: %s", logged)
	}
	if !strings.Contains(logged, project.CheckoutID) {
		t.Fatalf("the refusal must name the marker the checkout failed to carry, got: %s", logged)
	}
}

// TestSingletonRootHealRefusesSwappedCheckoutWithoutReaping pins the placement,
// not just the refusal. A heal reaps the dead record before re-creating, and
// that record holds the ONLY pointer to the conversation (#2616) and tab roster
// (#2628) the replacement carries. Verifying after the reap would delete all of
// that and then refuse — turning a checkout swap, or a transiently unreadable
// marker, into permanent loss of exactly what the heal exists to preserve.
func TestSingletonRootHealRefusesSwappedCheckoutWithoutReaping(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerEnabledRootProject(t, repoPath, "/opt/healed")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	if first == nil {
		t.Fatalf("root instance missing after the first ensure")
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	// The root's tmux vanishes AND the checkout is swapped — the crash-recovery
	// arm the issue names.
	first.SetStatusForTest(session.Dead)
	swapCheckoutForStrangersClone(t, repoPath)

	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("a heal must re-prove the checkout before re-creating, got %d creates", len(*seen))
	}
	if got := findRootInstance(t, manager, repoPath); got != first {
		t.Fatalf("the refusal must leave the dead root record in place — reaping it first would discard the conversation and tabs the heal carries")
	}
	data, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		t.Fatalf("loadRepoInstanceData: %v", err)
	}
	roots := 0
	for _, d := range data {
		if d.Title == session.RootSessionTitle {
			roots++
		}
	}
	if roots != 1 {
		t.Fatalf("the persisted root record must survive a refused heal, got %d", roots)
	}
}

// TestRootCreateReprovesTheCheckoutAfterTheReap closes the window the review
// found (#3711 P1). The pre-reap check protects the record, but it is not the
// proof the create runs on: between the two sits real blocking work — the
// reap's tmux teardown, editor shutdown and record delete, then a
// transcript-store scan — and a swap landing in there used to reach
// CreateSession on a checkout that had passed a check taken before any of it.
// The hook stands in for that elapsed work.
func TestRootCreateReprovesTheCheckoutAfterTheReap(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerEnabledRootProject(t, repoPath, "/opt/midpass")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	if first == nil {
		t.Fatalf("root instance missing after the first ensure")
	}
	first.SetStatusForTest(session.Dead)

	// The checkout is the project's own when the pre-reap check reads it, and a
	// stranger's by the time the create runs.
	swapped := false
	rootCreateVerifyHookForTest = func() {
		if swapped {
			return
		}
		swapped = true
		swapCheckoutForStrangersClone(t, repoPath)
	}
	t.Cleanup(func() { rootCreateVerifyHookForTest = nil })
	warnings := captureRootEnsureWarnings(t)

	manager.ensureRootAgentsAndWait()

	if !swapped {
		t.Fatalf("the create never went through createVerifiedRoot — a create path that bypasses it is unverified by construction")
	}
	if len(*seen) != 1 {
		t.Fatalf("a swap landing after the pre-reap check must still be refused at the create, got %d creates", len(*seen))
	}
	logged := warnings.String()
	if !strings.Contains(logged, "a different clone may be reusing the path") {
		t.Fatalf("the refusal must be the identity one, not an incidental create failure, got: %s", logged)
	}
	// The refusal returns BEFORE CreateSession, so reporting it as a failed
	// create asserts something that never happened — and it would read
	// differently from the identical refusal on the pre-reap arm, which carries
	// no such prefix.
	if strings.Contains(logged, "failed to create root session") {
		t.Fatalf("no create was attempted, so none may be reported as having failed; got: %s", logged)
	}
}

// TestLiveRootAdoptedDespiteSwappedCheckout pins the other half of the
// placement: adopt-first is untouched. A live root is the root agent whatever
// is at the path, and the check sits BELOW that early return — which is also
// what keeps the marker read off the daemon's one-second poll cadence, where a
// stalled mount would block the poll goroutine every tick.
func TestLiveRootAdoptedDespiteSwappedCheckout(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerEnabledRootProject(t, repoPath, "/opt/live")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	if first == nil {
		t.Fatalf("root instance missing after the first ensure")
	}

	swapCheckoutForStrangersClone(t, repoPath)
	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("a live root must be adopted, not re-created, got %d creates", len(*seen))
	}
	if got := findRootInstance(t, manager, repoPath); got != first {
		t.Fatalf("a live root must be left completely alone; the identity check may not tear one down")
	}
	if got := first.GetStatus(); got == session.Dead {
		t.Fatalf("the identity check may not kill a live root, got status %v", got)
	}
}

// TestSingletonRootCreateResumesWhenOriginalCheckoutReturns: the refusal is a
// retryable ensure failure, not a latch. The always-on contract (#1122) is that
// any cause which clears heals on the next attempt without a daemon restart,
// and an original checkout coming back is exactly such a cause.
func TestSingletonRootCreateResumesWhenOriginalCheckoutReturns(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerEnabledRootProject(t, repoPath, "/opt/returned")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	restore := swapCheckoutForStrangersClone(t, repoPath)

	manager.ensureRootAgentsAndWait()
	if len(*seen) != 0 {
		t.Fatalf("the swapped checkout must be refused, got %d creates", len(*seen))
	}

	restore()
	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("the registered checkout is back and carries its marker; the root must be created without a daemon restart, got %d creates", len(*seen))
	}
	if (*seen)[0].Program != "/opt/returned" {
		t.Fatalf("the resolved personal program must still reach the create verbatim, got %q", (*seen)[0].Program)
	}
}

// TestSingletonRootCreateHoldsAnUnreadableMarker: a marker that cannot be READ
// leaves identity unknowable, which is neither absence nor a proven mismatch.
// It must still fail closed — unknown is not permission to start an autonomous
// agent — but it must NOT prescribe a rebind: the original checkout may be
// sitting right there, transiently unreadable, and rebinding over it is
// destructive (#3299 review round 5).
func TestSingletonRootCreateHoldsAnUnreadableMarker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not make a file unreadable for root")
	}
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerEnabledRootProject(t, repoPath, "/opt/unreadable")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	marker := soleCheckoutMarker(t, repoPath)
	if err := os.Chmod(marker, 0o000); err != nil {
		t.Fatalf("chmod marker: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(marker, 0o644) })
	if _, err := os.ReadFile(marker); err == nil {
		t.Skip("environment does not enforce file permission bits; the unreadable fixture cannot exist here")
	}
	warnings := captureRootEnsureWarnings(t)

	manager.ensureRootAgentsAndWait()

	if len(*seen) != 0 {
		t.Fatalf("an unprovable checkout must not get the project's autonomous root, got %d creates", len(*seen))
	}
	logged := warnings.String()
	if !strings.Contains(logged, project.ID) || !strings.Contains(logged, "marker") {
		t.Fatalf("the refusal must name the project and the unreadable marker, got: %s", logged)
	}
	if strings.Contains(logged, "rebind") {
		t.Fatalf("an unreadable marker is not a proven mismatch — it must not prescribe a rebind over a possibly-original checkout; got: %s", logged)
	}
}

// TestSingletonRootCreateNamesAGoneRecordedRoot: config.ProjectCheckoutMatches
// answers a determinately-absent root with a plain false, so "carries no
// marker" covers a checkout that was SWAPPED and one that is simply GONE. They
// need different remedies — bring the path back, versus rebind onto the
// replacement — so the refusal must not send a user whose disk died hunting for
// a clone that is not there.
func TestSingletonRootCreateNamesAGoneRecordedRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerEnabledRootProject(t, repoPath, "/opt/gone")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.RemoveAll(repoPath); err != nil {
		t.Fatalf("remove the registered checkout: %v", err)
	}
	warnings := captureRootEnsureWarnings(t)

	manager.ensureRootAgentsAndWait()

	if len(*seen) != 0 {
		t.Fatalf("there is no checkout at the bound path; nothing may be created, got %d creates", len(*seen))
	}
	logged := warnings.String()
	if !strings.Contains(logged, project.ID) || !strings.Contains(logged, "bring the path back") {
		t.Fatalf("a gone recorded root must be named as gone, with the remedy that fits it, got: %s", logged)
	}
	if strings.Contains(logged, "a different clone may be reusing the path") {
		t.Fatalf("nothing is at the path — the refusal must not claim a different clone occupies it; got: %s", logged)
	}
}

// TestLegacyRootAgentCreateIsNotGatedByCheckoutIdentity pins the SCOPE of the
// #3366 gate rather than endorsing what it leaves alone. A root_agents entry is
// an opt-in the user wrote against a PATH, with no registry record behind it and
// so no recorded checkout id to match; #3334 settled that shape in the same
// direction, RELEASING a repo on a proven mismatch precisely so a legacy opt-in
// naming the path still applies. Changing that is a separate decision, and this
// test is what makes an accidental widening visible.
func TestLegacyRootAgentCreateIsNotGatedByCheckoutIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerEnabledRootProject(t, repoPath, "/opt/legacy")

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	swapCheckoutForStrangersClone(t, repoPath)

	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("the #3366 gate is scoped to registry-backed creates; a legacy root_agents path must behave exactly as it did before, got %d creates", len(*seen))
	}
}

// soleCheckoutMarker returns the one checkout marker this AF home wrote into
// repoPath's Git directory, failing if the fixture is not the single-marker
// shape the tests assume.
func soleCheckoutMarker(t *testing.T, repoPath string) string {
	t.Helper()
	candidates, err := filepath.Glob(filepath.Join(repoPath, ".git", "agent-factory", "checkout-id-*"))
	if err != nil {
		t.Fatalf("glob checkout markers: %v", err)
	}
	var markers []string
	for _, candidate := range candidates {
		if !strings.HasSuffix(candidate, ".lock") {
			markers = append(markers, candidate)
		}
	}
	if len(markers) != 1 {
		t.Fatalf("expected exactly one checkout marker, got %v", candidates)
	}
	return markers[0]
}
