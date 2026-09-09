package bugreport

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

const (
	siblingLeakTitle     = "fix bug (urgent)"
	siblingLeakDiskTitle = "fix-bug-urgent"
	siblingLeakRepo      = "/srv/ConfidentialClient/repo"
	siblingLeakAFHome    = "/srv/ConfidentialClient/af"
	siblingLeakArchive   = siblingLeakAFHome + "/archived/0f8fc14cb4d0/fix bug (urgent)"
	siblingLeakAlternate = siblingLeakRepo + "-" + siblingLeakDiskTitle
)

// TestScrubLogRedactsSanitizedTitleInSiblingRecoveryPath is the #4099 witness.
// The lost-restore diagnostic and task lifecycle warnings both print an error
// that can name the registered archive destination and the unregistered
// pre-move sibling on one line. The title scrub knows the display title, but the
// sibling carries its distinct on-disk spelling.
func TestScrubLogRedactsSanitizedTitleInSiblingRecoveryPath(t *testing.T) {
	r := &redactor{}
	r.noteAFHome(siblingLeakAFHome)
	r.redactInstancesJSON(mustMarshalInstances(t, []session.InstanceData{{
		ID:    "instance-1",
		Title: siblingLeakTitle,
		Worktree: session.GitWorktreeData{
			RepoPath:     siblingLeakRepo,
			WorktreePath: siblingLeakArchive,
		},
	}}))

	got := r.scrubLog(siblingRecoveryLogLine())
	if strings.Contains(got, siblingLeakDiskTitle) {
		t.Fatalf("scrubLog leaked the sanitized session title %q from the sibling worktree path:\n%s",
			siblingLeakDiskTitle, got)
	}
	for _, secret := range []string{siblingLeakTitle, "ConfidentialClient"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{"WORKTREE_MISSING_DETECTED", "identity unresolved", "[repo:1]", "[worktree:1]"} {
		if !strings.Contains(got, want) {
			t.Errorf("scrubLog removed triage value %q:\n%s", want, got)
		}
	}
}

// TestScrubLogRedactsSanitizedTitleFromRejectedRecord keeps the generic JSON
// fallback at least as private as the accepted typed path. A legacy string
// status rejects []InstanceData decoding. The fallback must both omit its
// sensitive alternate_path value and retain enough title context to scrub the
// same sibling spelling from the separately collected daemon log.
func TestScrubLogRedactsSanitizedTitleFromRejectedRecord(t *testing.T) {
	r := &redactor{}
	r.noteAFHome(siblingLeakAFHome)
	raw := json.RawMessage(fmt.Sprintf(`[{
		"status": "legacy-status",
		"title": %q,
		"worktree": {
			"repo_path": %q,
			"worktree_path": %q,
			"relocation_recovery": {"alternate_path": %q}
		}
	}]`, siblingLeakTitle, siblingLeakRepo, siblingLeakArchive, siblingLeakAlternate))

	instances := string(r.redactInstancesJSON(raw))
	if strings.Contains(instances, siblingLeakAlternate) {
		t.Fatalf("generic fallback leaked alternate_path %q:\n%s", siblingLeakAlternate, instances)
	}
	got := r.scrubLog(siblingRecoveryLogLine())
	if strings.Contains(got, siblingLeakDiskTitle) {
		t.Fatalf("scrubLog leaked the sanitized session title %q after typed decode rejection:\n%s",
			siblingLeakDiskTitle, got)
	}
}

// TestCollapseKnownRootsDoesNotRewriteAnUnrelatedSibling pins the text-pass
// half of the same separator rule collapsePathField already enforces. A sibling
// is not under the registered root, so consuming only its shared prefix would
// mislabel it and strand its private suffix.
func TestCollapseKnownRootsDoesNotRewriteAnUnrelatedSibling(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	line := "probe failed at " + siblingLeakRepo + "-backup/worktree"

	if got := r.collapseKnownRoots(line); got != line {
		t.Fatalf("collapseKnownRoots rewrote a sibling path without a separator boundary:\n got: %s\nwant: %s", got, line)
	}
}

func TestScrubWorktreePathTitlesKeepsCollisionSuffixAndUnrelatedText(t *testing.T) {
	r := &redactor{}
	r.noteWorktreeTitle(siblingLeakRepo, siblingLeakTitle)
	line := siblingLeakAlternate + "-2 failed; classification=" + siblingLeakDiskTitle

	got := r.scrubWorktreePathTitles(line)
	if want := siblingLeakRepo + "-" + redactedMarker + "-2 failed; classification=" + siblingLeakDiskTitle; got != want {
		t.Fatalf("contextual worktree-title scrub changed the wrong value:\n got: %s\nwant: %s", got, want)
	}
}

func TestScrubLogRedactsSiblingWhenTitleAppearsInRepoPath(t *testing.T) {
	const (
		title     = "fix bug"
		diskTitle = "fix-bug"
		repo      = "/srv/fix bug/repo"
	)
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title: title,
		Worktree: session.GitWorktreeData{
			RepoPath: repo,
		},
	})

	line := "recovery location: " + repo + "-" + diskTitle
	for _, tc := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scrub(line)
			if strings.Contains(got, diskTitle) {
				t.Fatalf("scrubber leaked %q after the raw title changed its registered repo root: %s", diskTitle, got)
			}
			if want := "[repo:1]-" + redactedMarker; !strings.Contains(got, want) {
				t.Errorf("scrubber lost the registered sibling layout, want %q in %q", want, got)
			}
		})
	}
}

func TestCollapseKnownRootsRecognizesFileURIPath(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)

	got := r.scrub("editor target file://" + siblingLeakRepo)
	if strings.Contains(got, "ConfidentialClient") {
		t.Fatalf("known repo root survived inside a file URI: %s", got)
	}
	if want := "file://[repo:1]"; !strings.Contains(got, want) {
		t.Errorf("file URI lost its useful wrapper, want %q in %q", want, got)
	}
	for _, longer := range []string{
		"nested path /mnt" + siblingLeakRepo,
		"nested URI file:///mnt" + siblingLeakRepo,
	} {
		if got := r.collapseKnownRoots(longer); got != longer {
			t.Errorf("root suffix inside a longer path was rewritten:\n got: %s\nwant: %s", got, longer)
		}
	}
}

func TestRejectedRecordsKeepWorktreeTitlePairOwnership(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[
		{"status":"legacy", "title":"alpha secret", "worktree":{"repo_path":"/srv/alpha/repo"}},
		{"status":"legacy", "title":"beta secret", "worktree":{"repo_path":"/srv/beta/repo"}}
	]`)
	r.redactInstancesJSON(raw)

	line := "unrelated sibling: /srv/alpha/repo-beta-secret"
	if got := r.scrubWorktreePathTitles(line); got != line {
		t.Fatalf("fallback fabricated a cross-record repo/title pair:\n got: %s\nwant: %s", got, line)
	}
	if got, want := len(r.worktreePathTitles), 2; got != want {
		t.Errorf("registered fallback repo/title pairs = %d, want %d", got, want)
	}
}

func TestScrubWorktreePathTitlesHandlesFilesystemRootRepo(t *testing.T) {
	r := &redactor{}
	r.noteWorktreeTitle(string(filepath.Separator), siblingLeakTitle)
	line := string(filepath.Separator) + "-" + siblingLeakDiskTitle

	got := r.scrubWorktreePathTitles(line)
	if strings.Contains(got, siblingLeakDiskTitle) {
		t.Fatalf("filesystem-root repo leaked its sibling worktree title %q: %s", siblingLeakDiskTitle, got)
	}
}

func siblingRecoveryLogLine() string {
	recovery := fmt.Sprintf("archive recovery location: either %s or %s (identity unresolved)",
		siblingLeakArchive, siblingLeakAlternate)
	return fmt.Sprintf("WORKTREE_MISSING_DETECTED classification=%q title=%q instance_id=%q repo_path=%q worktree_path=%q recover_error=%q",
		"missing", siblingLeakTitle, "instance-1", siblingLeakRepo, siblingLeakArchive, recovery)
}

func mustMarshalInstances(t *testing.T, datas []session.InstanceData) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(datas)
	if err != nil {
		t.Fatalf("marshal instances fixture: %v", err)
	}
	return raw
}
