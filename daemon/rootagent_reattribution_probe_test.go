package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

// TestForeignIdentityClassifiedBeforePublishing pins review finding 3911002404
// (P1). A foreign identity is deferred, so the probe must not publish it as a
// candidate or read its marker: publishing gates that REAL repository through
// rootAttributionPendingFor, and the marker read is an unbounded
// filesystem/Git operation — so a stalled foreign worktree could indefinitely
// block a legitimate root reached through another path in the same repository.
func TestForeignIdentityClassifiedBeforePublishing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	parent := testguard.CanonicalTempDir(t)
	main := filepath.Join(parent, "main")
	if err := exec.Command("git", "init", main).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	for _, args := range [][]string{
		{"-C", main, "config", "user.email", "t@t"},
		{"-C", main, "config", "user.name", "t"},
		{"-C", main, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	// A linked worktree: its identity is the main repo's, so the recorded path
	// hash cannot equal it.
	recorded := filepath.Join(parent, "wt")
	if err := exec.Command("git", "-C", main, "worktree", "add", recorded).Run(); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}
	mainID := repoID(t, main)
	if mainID == config.RepoIDForRecordedRoot(recorded) {
		t.Fatalf("fixture must produce a FOREIGN identity, both %s", mainID)
	}

	// If the marker were read at all, this hook would fire.
	markerRead := false
	rootReattributionProbeHookForTest = func(string) { markerRead = true }
	t.Cleanup(func() { rootReattributionProbeHookForTest = nil })

	probe := &rootReattributionProbe{done: make(chan struct{})}
	runRootReattributionProbe(probe, unresolvedProjectRecord{
		root: recorded, projectID: "prj_test", checkoutID: "chk_test",
	})
	<-probe.done

	if !probe.foreignIdentity {
		t.Fatalf("a recorded root that is not its repository's identity root must classify as foreign: %+v", probe)
	}
	if markerRead {
		t.Fatalf("a deferred identity must not have its marker read — that read waits on the recorded path and would gate a repository this scope does not attribute")
	}
	if c := probe.candidate.Load(); c != nil {
		t.Fatalf("a deferred identity must not be published as a candidate (%s would then be gated through another path)", c.ID)
	}
	if probe.repo != nil {
		t.Fatalf("a deferred identity must not be bound as this probe's repo: %+v", probe.repo)
	}
}

// TestStalledMarkerReadIsBoundedAndFailsClosed is the #3599 regression.
//
// The probe publishes the recorded root's identity as its candidate — gating
// that repository fail-closed — and THEN reads the checkout marker. That read
// forks git and had no deadline, so a path that flips to another repository
// mid-read left the flipped-to one ungated for as long as the read took: a
// legacy root_agents entry reaching it through a second worktree could start
// its root off the lower-precedence layers while the personal enabled = false
// still sat under the derived ID. The re-resolution added for review id
// 3787592555 rebinds the gate to whatever is at the path now — but it cannot
// run until the stalled call returns.
//
// Two halves, one claim: the wedged read ENDS, and what it ends as is a
// question that went unanswered — never a verdict about the checkout.
// Bounding it does not close the flip window (that is #3599 option 2, still
// filed); it converts "ungated indefinitely" into "ungated for at most one
// step's budget".
func TestStalledMarkerReadIsBoundedAndFailsClosed(t *testing.T) {
	t.Run("the gate follows the repository now at the path", func(t *testing.T) {
		recorded, other := reattributionProbeRepos(t)
		wedgeTheMarkerRead(t)
		withRootReattributionProbeStepTimeout(t, 400*time.Millisecond)

		// The flip, in the window the probe itself opens: the moment the wedged
		// read gives up, the recorded path becomes a linked worktree of ANOTHER
		// repository, whose identity is that repo's main root and therefore not
		// this pathname's hash. Held open by the hook rather than raced against
		// the deadline, so this pins the classification and not the scheduler.
		rootReattributionProbeHookForTest = func(root string) {
			if err := os.RemoveAll(root); err != nil {
				t.Errorf("remove the original checkout: %v", err)
				return
			}
			if out, err := exec.Command("git", "-C", other, "worktree", "add", root).CombinedOutput(); err != nil {
				t.Errorf("flip the recorded path to a worktree of %s: %v: %s", other, err, out)
			}
		}
		t.Cleanup(func() { rootReattributionProbeHookForTest = nil })

		probe := runBoundedProbe(t, recorded)
		wantID := repoID(t, other)
		if c := probe.candidate.Load(); c == nil || c.ID != wantID {
			got := "<nil>"
			if c != nil {
				got = c.ID
			}
			t.Fatalf("after the bounded read the gate must follow the repository now at "+
				"the path: candidate %s, want %s — leaving it on the first resolution is "+
				"the fail-open a legacy entry reaching that repo through another worktree "+
				"walks through (#3599)", got, wantID)
		}
		if probe.repo == nil || probe.repo.ID != wantID {
			t.Fatalf("the verdict must be bound to the identity that was actually verified: %+v", probe.repo)
		}
		if !probe.markerUnreadable {
			t.Fatalf("a read the deadline killed asked nothing, so the identity is "+
				"unknowable and must stay fail-closed: %+v", probe)
		}
		if probe.matches || probe.mismatch {
			t.Fatalf("a killed probe is not a verdict about the checkout (#3500): %+v", probe)
		}
	})

	t.Run("an unchanged path is unknowable, never a proven mismatch", func(t *testing.T) {
		// Nothing flips here, so the classification has nowhere to hide behind
		// the identity-change branch: the ONLY thing this probe learned is that
		// its marker read was killed. Settling that as a mismatch would release
		// the repository the read contradicts and prescribe a rebind for a
		// checkout nobody has actually looked at (#3500's line).
		recorded, _ := reattributionProbeRepos(t)
		wedgeTheMarkerRead(t)
		withRootReattributionProbeStepTimeout(t, 400*time.Millisecond)

		probe := runBoundedProbe(t, recorded)
		if !probe.markerUnreadable {
			t.Fatalf("a marker read the deadline killed must land on the 'could not ask' "+
				"side: %+v", probe)
		}
		if probe.mismatch {
			t.Fatalf("a killed read is not 'git answered no' — a mismatch here releases the " +
				"gate and tells the user to rebind a checkout that was never read (#3500)")
		}
		if probe.matches || probe.vanished {
			t.Fatalf("nor is it a match or a disappearance: %+v", probe)
		}
		if c := probe.candidate.Load(); c == nil || c.ID != config.RepoIDForRecordedRoot(recorded) {
			t.Fatalf("the recorded root's own identity must stay gated while its marker is unknowable: %+v", c)
		}
	})
}

// reattributionProbeRepos builds the two checkouts these subtests flip between:
// a recorded root that is its own identity root, and another repository whose
// main root can host a linked worktree.
func reattributionProbeRepos(t *testing.T) (recorded, other string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	parent := testguard.CanonicalTempDir(t)
	recorded = filepath.Join(parent, "repo")
	other = filepath.Join(parent, "other")
	for _, root := range []string{recorded, other} {
		if err := exec.Command("git", "init", root).Run(); err != nil {
			t.Fatalf("git init %s: %v", root, err)
		}
	}
	for _, args := range [][]string{
		{"-C", other, "config", "user.email", "t@t"},
		{"-C", other, "config", "user.name", "t"},
		{"-C", other, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return recorded, other
}

// wedgeTheMarkerRead puts a git shim on PATH that never returns for exactly the
// marker read. `--show-toplevel` and `--git-common-dir` in ONE argv is unique to
// the binding resolution that read runs; the probe's own resolution and
// re-resolution ask for them separately, so those still answer through real git.
func wedgeTheMarkerRead(t *testing.T) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("look up git: %v", err)
	}
	binDir := t.TempDir()
	pidFile := filepath.Join(binDir, "shim.pids")
	reapProbeShimChildren(t, pidFile)
	script := fmt.Sprintf(`#!/bin/sh
top=0
common=0
for a in "$@"; do
  [ "$a" = "--show-toplevel" ] && top=1
  [ "$a" = "--git-common-dir" ] && common=1
done
if [ "$top" = 1 ] && [ "$common" = 1 ]; then
  # Backgrounded with its pid recorded: killing the shim shell does not kill
  # its foreground child, and a shared box must not accumulate them.
  sleep 60 &
  echo $! >> '%s'
  wait
  exit 0
fi
exec '%s' "$@"
`, pidFile, realGit)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// runBoundedProbe runs one probe against root and fails the test if it does not
// finish — which is the pre-#3599 behaviour: the wedged read never returned, so
// the gate never moved.
func runBoundedProbe(t *testing.T, root string) *rootReattributionProbe {
	t.Helper()
	probe := &rootReattributionProbe{done: make(chan struct{})}
	go runRootReattributionProbe(probe, unresolvedProjectRecord{
		root: root, projectID: "prj_test", checkoutID: "chk_absent",
	})
	select {
	case <-probe.done:
		return probe
	case <-time.After(10 * time.Second):
		t.Fatal("every git step in the probe must be bounded: an unbounded marker read " +
			"holds the recorded root's identity gated and any repository the path " +
			"flipped to UNGATED for the whole of the stall (#3599)")
		return nil
	}
}

// reapProbeShimChildren kills every pid the git shim recorded. The shim's
// sleeps outlive the git process on purpose — that is the stall under test —
// and exec kills only the shell it started, never the shell's children.
func reapProbeShimChildren(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		for _, field := range strings.Fields(string(raw)) {
			if pid, convErr := strconv.Atoi(field); convErr == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

// withRootReattributionProbeStepTimeout drives the production budget for one
// test and restores it after.
func withRootReattributionProbeStepTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := rootReattributionProbeStepTimeout
	rootReattributionProbeStepTimeout = d
	t.Cleanup(func() { rootReattributionProbeStepTimeout = prev })
}
