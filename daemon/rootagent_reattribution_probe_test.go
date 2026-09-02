package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// Probe mechanics for the #3299 re-attribution pass: what runRootReattributionProbe
// concludes when the recorded path changes under it, and what the heal pass keeps
// when a retry brings back no evidence. These sit apart from the behavioural suite
// in rootagent_reattribution_test.go because they drive the probe directly — the
// window between the marker read and the re-resolution is microseconds wide in
// real life, so it is held open with rootReattributionProbeHookForTest rather
// than raced.

// TestPresentButUnresolvableIsNotVanished pins review finding 3787021890 (P2).
// The re-resolution that binds a marker verdict to an identity can fail while
// the recorded path is still THERE — unreadable or invalid .git metadata, a
// safe.directory refusal, git itself failing. Classifying every such error as
// physical disappearance made both the log and the verdict prescribe "bring
// the path back" for a path already present, sending the user after the wrong
// thing while every retry kept failing.
func TestPresentButUnresolvableIsNotVanished(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	for _, tc := range []struct {
		name        string
		breakIt     func(t *testing.T, root string)
		wantVanish  bool
		description string
	}{
		{
			name: "metadata broken, path present",
			breakIt: func(t *testing.T, root string) {
				if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
					t.Fatalf("break git metadata: %v", err)
				}
			},
			wantVanish:  false,
			description: "a present path whose repository metadata broke needs REPAIR, not restoring",
		},
		{
			name: "path removed",
			breakIt: func(t *testing.T, root string) {
				if err := os.RemoveAll(root); err != nil {
					t.Fatalf("remove root: %v", err)
				}
			},
			wantVanish:  true,
			description: "a genuinely absent path is the one case whose remedy IS restoring it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := filepath.Join(testguard.CanonicalTempDir(t), "checkout")
			if err := exec.Command("git", "init", work).Run(); err != nil {
				t.Fatalf("git init: %v", err)
			}
			rootReattributionProbeHookForTest = func(root string) { tc.breakIt(t, root) }
			t.Cleanup(func() { rootReattributionProbeHookForTest = nil })

			probe := &rootReattributionProbe{done: make(chan struct{})}
			runRootReattributionProbe(probe, unresolvedProjectRecord{
				root: work, projectID: "prj_test", checkoutID: "ckt_absent",
			})
			<-probe.done

			if !probe.markerUnreadable {
				t.Fatalf("an unverifiable identity must stay fail-closed (markerUnreadable), got %+v", probe)
			}
			if probe.matches {
				t.Fatalf("an unverifiable identity must never report a match")
			}
			if probe.vanished != tc.wantVanish {
				t.Fatalf("%s: vanished = %v, want %v", tc.description, probe.vanished, tc.wantVanish)
			}
		})
	}
}

// TestFencedVerifiedProbeSurvivesFreshnessTTL pins review finding 3787592559
// (P1). A verified probe that the consume phase deliberately holds back
// because its project is mid-delete is the ONLY thing keeping the real repo ID
// attribution-pending, while the delete's fence and tombstone are still keyed
// by the derived ID alone. The freshness TTL replaced it after 30s — a third
// way to retire the probe, past the round-12 fix — so a replacement that
// stalls or finds the path gone re-opened the window where a legacy entry
// through another path in that repository recreates the deleted root.
func TestFencedVerifiedProbeSurvivesFreshnessTTL(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	realID := repoID(t, repoPath)
	derivedID := config.RepoIDForRecordedRoot(repoPath)

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := os.Rename(hidden, repoPath); err != nil {
		t.Fatalf("restore repo dir: %v", err)
	}

	// A VERIFIED match, completed long enough ago to be stale, held back by an
	// active derived-ID delete fence — exactly the state the consume phase
	// leaves behind when it finds the project mid-delete.
	resolved, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	fenced := &rootReattributionProbe{done: make(chan struct{})}
	fenced.repo = resolved
	fenced.candidate.Store(resolved)
	fenced.matches = true
	fenced.completedAt = nowFunc().Add(-2 * rootHealProbeResultTTL)
	close(fenced.done)
	manager.mu.Lock()
	manager.rootHealProbes[derivedID] = fenced
	if manager.projectDeletes == nil {
		manager.projectDeletes = make(map[string]struct{})
	}
	manager.projectDeletes[derivedID] = struct{}{}
	manager.mu.Unlock()

	manager.EnsureRootAgents()

	manager.mu.Lock()
	held := manager.rootHealProbes[derivedID]
	manager.mu.Unlock()
	if held != fenced {
		t.Fatalf("a stale-but-VERIFIED probe must not be replaced while its project is fenced mid-delete: the candidate is the only thing holding %s attribution-pending, and the fence/tombstone are keyed by %s alone", realID, derivedID)
	}
	if !manager.rootAttributionPendingFor(realID) {
		t.Fatalf("the real identity must stay attribution-pending across the pass, or a legacy entry resolving there can recreate the deleted root")
	}

	// Once the fence clears, staleness applies again: the pass replaces it
	// with a fresh check, which is the first moment a re-check means anything.
	manager.mu.Lock()
	delete(manager.projectDeletes, derivedID)
	manager.mu.Unlock()

	manager.EnsureRootAgents()

	manager.mu.Lock()
	after := manager.rootHealProbes[derivedID]
	manager.mu.Unlock()
	if after == fenced {
		t.Fatalf("with the fence gone the stale result must no longer be retained — the exception exists only for the mid-delete window")
	}
}

// TestSamePathSwapIsNotAVerdict pins review finding 3884615893 (P1). When the
// recorded root is a repository's own main root, its identity is a hash of
// that PATHNAME — so the original checkout and a stranger's clone put there
// resolve to the same id, and the re-resolution added for 3787592555 cannot
// see the swap. Accepting the first read would apply this project's policy to
// a stranger's checkout; rejecting it would discard a disable that is still
// correct once the original returns. Two agreeing reads, or nothing.
func TestSamePathSwapIsNotAVerdict(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	parent := testguard.CanonicalTempDir(t)
	recorded := filepath.Join(parent, "repo")
	if err := exec.Command("git", "init", recorded).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project := registerTestProject(t, recorded)
	beforeID := repoID(t, recorded)

	// A stranger's clone replaces the checkout at the SAME pathname, inside
	// the probe's own verification window.
	rootReattributionProbeHookForTest = func(root string) {
		if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
			t.Fatalf("remove original metadata: %v", err)
		}
		if err := exec.Command("git", "init", root).Run(); err != nil {
			t.Fatalf("git init stranger: %v", err)
		}
	}
	t.Cleanup(func() { rootReattributionProbeHookForTest = nil })

	probe := &rootReattributionProbe{done: make(chan struct{})}
	runRootReattributionProbe(probe, unresolvedProjectRecord{
		root: recorded, projectID: project.ID, checkoutID: project.CheckoutID,
	})
	<-probe.done

	if got := repoID(t, recorded); got != beforeID {
		t.Fatalf("fixture must keep the identity CONSTANT across the swap — that is what makes the id check blind: %s vs %s", beforeID, got)
	}
	if probe.matches {
		t.Fatalf("a checkout swapped under verification must not be accepted: its policy would run in a stranger's tree")
	}
	if probe.mismatch {
		t.Fatalf("a swap is unknowable, not a proven mismatch — prescribing a rebind here would discard a disable that is still correct")
	}
	if !probe.markerUnreadable {
		t.Fatalf("a swap must land fail-closed (markerUnreadable), got %+v", probe)
	}
}

// TestRecordRootAbsentAcceptsEveryDeterminateAbsence pins review finding
// 3910107324 (P2). Determinate absence is more than os.ErrNotExist: an
// ancestor replaced by a regular file (ENOTDIR), a symlink loop, or an
// over-long name all PROVE nothing is at the path. Recognising only
// ErrNotExist answered "unknown" for a path that is provably gone, which left
// a delete's tombstone unclaimed and therefore unreleasable — so an unrelated
// repository later rooted there stayed suppressed for the daemon's lifetime.
// config owns this rule; this pins that the daemon defers to it.
func TestRecordRootAbsentAcceptsEveryDeterminateAbsence(t *testing.T) {
	base := testguard.CanonicalTempDir(t)

	file := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loop := filepath.Join(base, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatalf("symlink loop: %v", err)
	}
	present := filepath.Join(base, "present")
	if err := os.MkdirAll(present, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for _, tc := range []struct {
		name       string
		root       string
		wantAbsent bool
	}{
		{"missing", filepath.Join(base, "gone"), true},
		{"ancestor is a regular file", filepath.Join(file, "child"), true},
		{"symlink loop", loop, true},
		{"over-long name", filepath.Join(base, strings.Repeat("n", 512)), true},
		{"present directory", present, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			absent, err := recordRootAbsent(tc.root)
			if err != nil {
				t.Fatalf("recordRootAbsent(%q) must classify rather than error: %v", tc.root, err)
			}
			if absent != tc.wantAbsent {
				t.Fatalf("recordRootAbsent(%q) = %v, want %v — an unclassified absence leaves a tombstone unclaimed and unreleasable", tc.root, absent, tc.wantAbsent)
			}
		})
	}
}
