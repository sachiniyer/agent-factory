package bugreport

import (
	"encoding/json"
	"fmt"
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
	for _, want := range []string{"WORKTREE_MISSING_DETECTED", "identity unresolved", "[repo:1]", "[af-home]"} {
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
