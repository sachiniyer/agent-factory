package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// These tests are the #3264 self-heal suite: a fail-closed latch (#3241
// unloadable personal config, #3247 unlistable registry) must heal in the SAFE
// direction on the ensure cadence — a read that starts succeeding replaces
// "unknown" with the config's true answer, a still-failing read stays closed,
// and no daemon restart is required. Pre-#3264 both latches were pinned for
// the daemon's whole run, so a boot-time transient (a mount attaching seconds
// after autostart) suppressed every root agent until a human restarted the
// daemon — on a subsystem whose contract is back-off-but-never-give-up.

// TestEnsureRootAgentsHealsUnloadablePersonalConfig: the personal config was
// unloadable when the daemon snapshotted and becomes readable before the next
// ensure pass. The pass must resume resolution from the config's true answer.
func TestEnsureRootAgentsHealsUnloadablePersonalConfig(t *testing.T) {
	cases := []struct {
		name string
		// healedBody replaces the broken personal config before the ensure pass.
		healedBody  string
		wantCreates int
		wantReason  rootAgentMaterializeReason
	}{
		{
			// The heal is not a bypass: a now-readable disable resolves to a
			// provenanced DISABLED, not to a start — and no longer reads as
			// "af could not tell".
			name:        "readable disable stays down with the true reason",
			healedBody:  "enabled = false",
			wantCreates: 0,
			wantReason:  rootAgentDisabled,
		},
		{
			// The safe-direction start: the config's first successful read this
			// run says enabled, so the always-on root comes up without a restart.
			name:        "readable enable starts the root",
			healedBody:  "enabled = true",
			wantCreates: 1,
			wantReason:  rootAgentWillMaterialize,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			seen := installOptionsRecordingBackend(t)
			repoPath := setupControlRepo(t)
			project := registerTestProject(t, repoPath)
			writePersonalRootAgent(t, project.ID, "enabled = false")
			breakPersonalRootAgentToml(t, project.ID)

			manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentPersonalUnreadable {
				t.Fatalf("fixture: the broken personal config must fail closed at start, got reason %d", got)
			}

			// The config becomes readable again before the next ensure pass.
			writePersonalRootAgent(t, project.ID, tc.healedBody)
			manager.EnsureRootAgents()

			if len(*seen) != tc.wantCreates {
				t.Fatalf("want %d creates after the personal config heals, got %d — a read that starts succeeding must replace the fail-closed latch with the config's true answer, without a daemon restart", tc.wantCreates, len(*seen))
			}
			if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != tc.wantReason {
				t.Fatalf("verdict reason after the heal: want %d, got %d — a healed read must carry the true cause, not the stale unknown", tc.wantReason, got)
			}
		})
	}
}

// TestEnsureRootAgentsStillClosedWhileRepairIsIncomplete pins the direction of
// the heal: a personal config that STAYS unloadable keeps failing closed, pass
// after pass — the retry never reinterprets a failing read as absence.
func TestEnsureRootAgentsStillClosedWhileRepairIsIncomplete(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	breakPersonalRootAgentToml(t, project.ID)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("a still-unloadable personal config must keep failing closed, got %d creates", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentPersonalUnreadable {
		t.Fatalf("the unhealed latch must keep its cause, got reason %d", got)
	}
}

// TestEnsureRootAgentsHealsUnlistableRegistry: the registry was unlistable at
// daemon start (everything fails closed, #3247) and is repaired before the
// next ensure pass. The pass must re-read it once, freeze that read as the
// snapshot, and resume — legacy roots start, and a registered personal
// disable that the boot read could not see now applies.
func TestEnsureRootAgentsHealsUnlistableRegistry(t *testing.T) {
	cases := []struct {
		name string
		// disable registers the repo with a personal enabled=false before the
		// registry is corrupted.
		disable     bool
		wantCreates int
	}{
		{
			// The blast-radius heal: a legacy-only root suppressed by the boot
			// failure comes back the moment the registry lists again.
			name:        "legacy-only root resumes",
			disable:     false,
			wantCreates: 1,
		},
		{
			// The safe direction, proven: the re-read surfaces the personal
			// disable the boot failure hid, so the root stays down — now from
			// config, not from the latch.
			name:        "re-read personal disable applies",
			disable:     true,
			wantCreates: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			seen := installOptionsRecordingBackend(t)
			repoPath := setupControlRepo(t)
			if tc.disable {
				project := registerTestProject(t, repoPath)
				writePersonalRootAgent(t, project.ID, "enabled = false")
			}
			corruptProjectRegistry(t)

			manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentRegistryUnreadable {
				t.Fatalf("fixture: the corrupt registry must fail closed at start, got reason %d", got)
			}

			// Repair: remove the stray entry, prove the registry lists again.
			dir, err := config.ProjectRegistryDir()
			if err != nil {
				t.Fatalf("ProjectRegistryDir: %v", err)
			}
			if err := os.Remove(filepath.Join(dir, "stray")); err != nil {
				t.Fatalf("repair registry: %v", err)
			}
			if _, err := config.ListProjects(); err != nil {
				t.Fatalf("fixture: registry must list after repair: %v", err)
			}

			manager.EnsureRootAgents()

			if len(*seen) != tc.wantCreates {
				t.Fatalf("want %d creates after the registry heals, got %d — the ensure pass must re-read the registry it could not list at boot, without a daemon restart", tc.wantCreates, len(*seen))
			}
			if findRoot := findRootInstance(t, manager, repoPath) != nil; findRoot != (tc.wantCreates == 1) {
				t.Fatalf("root instance presence must match the healed resolution")
			}
		})
	}
}

// TestRootAgentHealLeavesLiveRootAlone pins adopt-first across the heal: a
// live root that predates the fail-closed state — however it was created, per
// the #1106 adopt rule — is not torn down, reaped, or duplicated either while
// the registry fails closed or by the pass that heals it.
func TestRootAgentHealLeavesLiveRootAlone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	corruptProjectRegistry(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The live root that survived into the fail-closed daemon: created through
	// the same reserved in-place shape the ensure loop uses.
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:         session.RootSessionTitle,
		RepoPath:      repoPath,
		InPlace:       true,
		Backend:       string(session.BackendLocal),
		allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("fixture: expected exactly the direct create, got %d", len(*seen))
	}
	root := findRootInstance(t, manager, repoPath)
	if root == nil {
		t.Fatalf("fixture: root instance missing after direct create")
	}

	manager.EnsureRootAgents()
	if len(*seen) != 1 {
		t.Fatalf("a live root must be left alone while the registry fails closed, got %d creates", len(*seen))
	}

	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "stray")); err != nil {
		t.Fatalf("repair registry: %v", err)
	}
	manager.EnsureRootAgents()
	if len(*seen) != 1 {
		t.Fatalf("a live root must be adopted, not re-created, by the pass that heals the registry, got %d creates", len(*seen))
	}
	if got := findRootInstance(t, manager, repoPath); got != root {
		t.Fatalf("the healed pass must keep the same adopted root instance")
	}
}

// TestRootAgentHealTreatsAbsentRegistryAsTransition pins the #3315 review's
// P1: a latched registry provably existed at daemon start, so the directory
// being ABSENT during recovery is a transition (a repair mv in flight, a
// mount blip) — ListProjects maps absence to an empty success, and accepting
// it would clear the latch onto a frozen EMPTY snapshot, failing open against
// personal disables that may be back moments later. The latch must hold until
// a PRESENT registry lists.
func TestRootAgentHealTreatsAbsentRegistryAsTransition(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	corruptProjectRegistry(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentRegistryUnreadable {
		t.Fatalf("fixture: the corrupt registry must fail closed at start, got reason %d", got)
	}

	// The registry path vanishes entirely: ListProjects now reports an empty
	// success, which recovery must refuse to freeze.
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove registry: %v", err)
	}
	manager.EnsureRootAgents()
	if len(*seen) != 0 {
		t.Fatalf("an absent registry during recovery must keep the latch, got %d creates — absence is not an empty registry", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentRegistryUnreadable {
		t.Fatalf("the latch must hold while the registry is absent, got reason %d", got)
	}

	// The registry returns (present, empty): recovery may now proceed.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("restore registry dir: %v", err)
	}
	manager.EnsureRootAgents()
	if len(*seen) != 1 {
		t.Fatalf("a present registry must heal the latch and let the legacy root start, got %d creates", len(*seen))
	}
}

// TestRootAgentHealRecomputesLegacyDedup pins the #3315 review's stale-dedup
// finding: a legacy path that resolved only AFTER boot must be in the healed
// snapshot's dedup set, or the singleton sweep can double-visit its repo
// behind a failing legacy attempt and create the root without the legacy
// layer.
func TestRootAgentHealRecomputesLegacyDedup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	rid := repoID(t, repoPath)

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	corruptProjectRegistry(t)
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Both come back before the next ensure pass: the mount and the registry.
	if err := os.Rename(hidden, repoPath); err != nil {
		t.Fatalf("restore repo dir: %v", err)
	}
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "stray")); err != nil {
		t.Fatalf("repair registry: %v", err)
	}
	manager.EnsureRootAgents()

	if !manager.rootAgentLayers.Load().legacyRepoIDs[rid] {
		t.Fatalf("the healed snapshot must recompute the legacy dedup set for a path that resolved after boot")
	}
	if len(*seen) != 1 {
		t.Fatalf("the repo must be ensured exactly once across both sweeps, got %d creates", len(*seen))
	}
}

// TestRootAgentHealKeepsPersonalLatchWhileRegistryAbsent pins the #3315
// round-2 P1: LoadProjectConfig maps ENOENT to an absent layer, so with the
// whole registry gone mid-outage every latched personal config would read as
// deliberately removed and the latch would drop exactly when the disable is
// about to come back. Only a PRESENT record directory proves a removal was
// deliberate.
func TestRootAgentHealKeepsPersonalLatchWhileRegistryAbsent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	breakPersonalRootAgentToml(t, project.ID)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rid := repoID(t, repoPath)
	if got := manager.rootAgentMaterializeVerdictFor(rid).reason; got != rootAgentPersonalUnreadable {
		t.Fatalf("fixture: the broken personal config must fail closed at start, got reason %d", got)
	}

	// The registry vanishes mid-outage: the latched config now reads ENOENT,
	// which must NOT count as a deliberate removal.
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	aside := dir + ".aside"
	if err := os.Rename(dir, aside); err != nil {
		t.Fatalf("set registry aside: %v", err)
	}
	manager.EnsureRootAgents()
	if len(*seen) != 0 {
		t.Fatalf("an absent registry must keep the personal latch, got %d creates — a vanished directory is not a deliberate config removal", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(rid).reason; got != rootAgentPersonalUnreadable {
		t.Fatalf("the personal latch must hold while the registry is absent, got reason %d", got)
	}

	// The registry returns with the config fixed: the latch heals from the
	// file's actual content.
	if err := os.Rename(aside, dir); err != nil {
		t.Fatalf("restore registry: %v", err)
	}
	writePersonalRootAgent(t, project.ID, "enabled = false")
	manager.EnsureRootAgents()
	if len(*seen) != 0 {
		t.Fatalf("the healed disable must keep the root down, got %d creates", len(*seen))
	}
	if got := manager.rootAgentMaterializeVerdictFor(rid).reason; got != rootAgentDisabled {
		t.Fatalf("the healed latch must carry the true cause, got reason %d", got)
	}
}

// TestRootAgentPersonalHealRecomputesLegacyDedup pins the #3315 round-2 P2:
// the dedup recompute must ride EVERY published heal, not only the registry
// branch — a legacy path resolving after boot plus a personal config healing
// in the same run must still leave the repo deduped out of the singleton
// sweep.
func TestRootAgentPersonalHealRecomputesLegacyDedup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	breakPersonalRootAgentToml(t, project.ID)
	rid := repoID(t, repoPath)

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// The mount returns and the personal config heals to an enable before the
	// next ensure pass.
	if err := os.Rename(hidden, repoPath); err != nil {
		t.Fatalf("restore repo dir: %v", err)
	}
	writePersonalRootAgent(t, project.ID, "enabled = true")
	manager.EnsureRootAgents()

	if !manager.rootAgentLayers.Load().legacyRepoIDs[rid] {
		t.Fatalf("a personal-config heal must also recompute the legacy dedup set for a path that resolved after boot")
	}
	if len(*seen) != 1 {
		t.Fatalf("the repo must be ensured exactly once across both sweeps, got %d creates", len(*seen))
	}
}
