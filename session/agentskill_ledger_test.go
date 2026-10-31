package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// A root af wrote under while consent was granted must still be cleaned when
// the operator later moves the launch root and withdraws consent: the cleanup
// list the declined path derives (this launch's root plus the daemon's) names
// neither the old root nor anything else it never saw (#4510 review).
func TestEnsureAfSkillDir_DeclinedCleansARootAnEarlierGrantedLaunchWrote(t *testing.T) {
	oldRoot := filepath.Join(t.TempDir(), "skills")
	newRoot := filepath.Join(t.TempDir(), "skills")

	grantGlobalAgentSkills(t)
	if _, err := ensureAfSkillDir(oldRoot); err != nil {
		t.Fatalf("granted write under the old root: %v", err)
	}
	oldFile := filepath.Join(oldRoot, afSkillDirName, "SKILL.md")
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("expected the granted launch to write %s: %v", oldFile, err)
	}

	cfg := config.DefaultConfig()
	cfg.GlobalAgentSkills = false
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatalf("save af config: %v", err)
	}
	// The operator has since pointed the launch at a different root.
	if _, err := ensureAfSkillDir(newRoot); err != nil {
		t.Fatalf("declined launch under the new root: %v", err)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("af's marked file under the previous root must not outlive the withdrawn consent (stat err: %v)", err)
	}
}

// A user's own un-marked file under a recorded root is never removed, and the
// root stays recorded so a later opt-out still retries it.
func TestCleanManagedSkillDirs_LeavesTheUsersOwnFile(t *testing.T) {
	writeAfConfig(t, false)
	root := filepath.Join(t.TempDir(), "skills", afSkillDirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(own, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordManagedSkillDir(root)
	cleanManagedSkillDirs()
	if b, err := os.ReadFile(own); err != nil || string(b) != "mine" {
		t.Fatalf("the user's own file must survive cleanup (got %q, err %v)", b, err)
	}
}
