package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestRegistryHealStreakSpacingHoldsAcrossLegacyNarrowing pins the registry
// recovery's two-strike SPACING contract: the streak's two matching reads must
// be one backoff cadence apart, not one poll tick apart, even when a legacy
// root_agents path heals (narrows its unknown set) in the SAME pass as the
// streak's first observation.
//
// Before the fix, the registry arm paced its reads on the SHARED
// rootHealNextAttempt clock, and a legacy narrowing in that same pass reset
// that clock to now — so the streak's second read landed on the very next
// poll tick. A mount transition that returned an identical PARTIAL project
// list across two consecutive ticks but completed (returned the full list)
// within one backoff cadence was committed and frozen until the next daemon
// restart: exactly the false recovery the two-strike discipline exists to
// prevent.
//
// The fix gives the registry arm its OWN clock
// (rootHealRegistryNextAttempt), which a legacy narrowing is not allowed to
// move. The contrast between the two arms is the whole of the proof:
//
//   - Bug arm (stallLegacyAtBoot=true): the boot snapshot latches BOTH
//     registryUnreadable=true AND legacy.unknownPaths[repoPath]=true (the
//     single-shared-mount return — registry and a root_agents path on the
//     same filesystem, both down at boot, both back by pass N). Pass N takes
//     the streak's first observation AND narrows the legacy set. One poll
//     tick later the streak's second read must NOT yet be due, so the latch
//     must STILL be closed; only one full backoff cadence later may it
//     commit. On the unfixed code the latch is open after one tick.
//   - Control arm (stallLegacyAtBoot=false): the legacy path resolves at boot
//     (unknownPaths empty), so pass N has nothing legacy to narrow and the
//     streak's first read charges a full backoff. One tick later the latch
//     must still be closed. This arm passes on the unfixed code too, which is
//     the point: the spacing discipline is intact on its own; it is the
//     legacy reset specifically that collapses it.
//
// The test uses the REAL rootEnsureBackoffBase (10s) and an injected clock:
// with base zeroed (every other registry-streak test's idiom) the backoff
// cadence degenerates to the poll-tick cadence and the spacing defeat is
// invisible.
func TestRegistryHealStreakSpacingHoldsAcrossLegacyNarrowing(t *testing.T) {
	cases := []struct {
		name              string
		stallLegacyAtBoot bool
	}{
		{name: "legacy_narrowing_must_not_collapse_registry_streak_spacing", stallLegacyAtBoot: true},
		{name: "no_legacy_narrowing", stallLegacyAtBoot: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

			// Real backoff cadence (10s) — the bug only shows when the cadence
			// is nonzero, so every registry-streak test that zeroes the base
			// misses it.
			prevBase := rootEnsureBackoffBase
			rootEnsureBackoffBase = 10 * time.Second
			t.Cleanup(func() { rootEnsureBackoffBase = prevBase })
			if got := rootEnsureBackoffFor(1); got != 10*time.Second {
				t.Fatalf("fixture: rootEnsureBackoffFor(1) must be one cadence (10s) for this test to mean anything, got %v", got)
			}

			// A personal disable so a committed snapshot resolves to DISABLED
			// (no root create): the assertion is on the latch itself, not on a
			// started root, and the disable is byte-stable across both reads so
			// the streak can actually reach two.
			seen := installOptionsRecordingBackend(t)
			repoPath := setupControlRepo(t)
			project := registerTestProject(t, repoPath)
			writePersonalRootAgent(t, project.ID, "enabled = false")

			// Deterministic injected clock — advance it past the trace instead
			// of sleeping through a real cadence.
			base := time.Unix(1_700_000_000, 0)
			clock := base
			origNow := nowFunc
			nowFunc = func() time.Time { return clock }
			t.Cleanup(func() { nowFunc = origNow })

			// In the bug arm the boot probe for the root_agents path is
			// unanswered, latching it unknown alongside the unlistable
			// registry. In the control arm the path resolves at boot.
			var restoreLegacy func()
			if tc.stallLegacyAtBoot {
				restoreLegacy = unansweredLegacyRootResolution(t, repoPath)
			}
			repair := breakProjectRegistryEnumeration(t)
			manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}

			// Fixture: the registry must be latched unreadable at boot.
			if !manager.rootAgentLayers.Load().registryUnreadable {
				t.Fatalf("fixture: the unenumerable registry must fail closed at start")
			}
			layers := manager.rootAgentLayers.Load()
			if tc.stallLegacyAtBoot {
				if !layers.legacy.unknownPaths[repoPath] {
					t.Fatalf("fixture: the stalled legacy path must be latched unknown at start, got unknownPaths=%v", layers.legacy.unknownPaths)
				}
			} else {
				if len(layers.legacy.unknownPaths) != 0 {
					t.Fatalf("fixture: the legacy path must be resolved at boot in the control arm, got unknownPaths=%v", layers.legacy.unknownPaths)
				}
			}

			// Both the registry and the legacy path heal BEFORE pass N: the
			// contested single-pass return is the filesystem coming back, not
			// the heal timing.
			repair()
			if restoreLegacy != nil {
				restoreLegacy()
			}

			// Pass N: the registry streak takes its FIRST observation, and —
			// in the bug arm — a concurrently-healing legacy path narrows in
			// the same pass (the contested ordering).
			clock = base
			manager.ensureRootAgentsAndWait()

			// Direct clock-state assertions (guarantees 5, 6, 8): after pass N
			// the registry arm's OWN clock must carry a charged backoff
			// (it took a non-committing read), while the SHARED clock reflects
			// whatever the legacy block did. In the bug arm the legacy block
			// narrowed and reset the shared clock to now — that reset is the
			// thing that used to drag the registry streak onto the per-tick
			// cadence; the registry's OWN clock must be unaffected by it.
			manager.mu.Lock()
			gotRegNext, gotRegFailures := manager.rootHealRegistryNextAttempt, manager.rootHealRegistryFailures
			gotSharedNext, gotSharedFailures := manager.rootHealNextAttempt, manager.rootHealFailures
			manager.mu.Unlock()
			if want := base.Add(rootEnsureBackoffFor(1)); !gotRegNext.Equal(want) {
				t.Fatalf("after pass N the registry clock must carry one charged backoff: rootHealRegistryNextAttempt=%v want %v (rootHealRegistryFailures=%d)", gotRegNext, want, gotRegFailures)
			}
			if gotRegFailures != 1 {
				t.Fatalf("a non-committing registry read must charge the registry backoff: rootHealRegistryFailures=%d want 1", gotRegFailures)
			}
			if tc.stallLegacyAtBoot {
				// The legacy block narrowed and reset the SHARED clock to now —
				// that reset is legitimate for the shared clock, and it must NOT
				// have moved the registry clock (asserted above).
				if !gotSharedNext.Equal(base) {
					t.Fatalf("in the bug arm the legacy narrowing resets the SHARED clock to pass N's now: rootHealNextAttempt=%v want %v", gotSharedNext, base)
				}
				if gotSharedFailures != 0 {
					t.Fatalf("a legacy narrowing is progress on the SHARED clock: rootHealFailures=%d want 0", gotSharedFailures)
				}
			} else {
				// Control arm: no legacy narrowing, and the registry branch ran
				// (it no longer touches the shared clock), so the shared clock
				// stays at its boot zero value (never charged by the registry).
				if !gotSharedNext.IsZero() {
					t.Fatalf("in the control arm the registry arm must not touch the shared clock: rootHealNextAttempt=%v want zero", gotSharedNext)
				}
			}

			// One poll tick (1s) later — well under one backoff cadence (10s).
			// The streak's second observation must NOT yet be due, so the latch
			// must still be closed.
			clock = base.Add(time.Second)
			manager.ensureRootAgentsAndWait()
			if got := manager.rootAgentLayers.Load(); got.registryUnreadable {
				// latch holds — spacing intact
			} else {
				t.Fatalf("the registry streak's two observations were one tick (1s) apart, not one backoff cadence (10s) — " +
					"the latch must still be closed, but it committed (registryUnreadable=false)")
			}
			if len(*seen) != 0 {
				t.Fatalf("a still-latched registry must keep every root fail-closed, got %d creates", len(*seen))
			}

			// One full backoff cadence later the streak's second observation
			// IS due: the matching snapshot commits and the latch releases.
			// This is the happy path the spacing discipline preserves — the
			// fix delays the recovery by exactly the cadence it always
			// promised, never skipping it.
			clock = base.Add(rootEnsureBackoffFor(1) + time.Second)
			manager.ensureRootAgentsAndWait()
			if manager.rootAgentLayers.Load().registryUnreadable {
				t.Fatalf("one backoff cadence later the matching registry snapshots must commit and release the latch, but it is still closed")
			}
			// Guarantee 11: a commit resets the registry clock.
			manager.mu.Lock()
			gotRegNext, gotRegFailures = manager.rootHealRegistryNextAttempt, manager.rootHealRegistryFailures
			manager.mu.Unlock()
			if gotRegFailures != 0 {
				t.Fatalf("a committed registry recovery must reset the registry backoff: rootHealRegistryFailures=%d want 0", gotRegFailures)
			}
			if want := base.Add(rootEnsureBackoffFor(1) + time.Second); !gotRegNext.Equal(want) {
				t.Fatalf("a committed registry recovery must reset the registry clock to now: rootHealRegistryNextAttempt=%v want %v", gotRegNext, want)
			}
			// The committed snapshot carries a personal disable, so no root
			// starts: the disable is the recovery's true answer, not a
			// fail-open.
			if len(*seen) != 0 {
				t.Fatalf("the recovered snapshot must publish the personal disable, got %d creates", len(*seen))
			}
			if got := manager.rootAgentMaterializeVerdictFor(repoID(t, repoPath)).reason; got != rootAgentDisabled {
				t.Fatalf("the recovered snapshot must resolve to a provenanced disable, got reason %d", got)
			}
		})
	}
}
