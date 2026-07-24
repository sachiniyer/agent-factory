package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSetPRInfo_ArchiveWinningOpLockRaceKeepsPRInfo is #2437.
//
// SetPRInfo mutates and persists the same instance record every tab verb does,
// but it was the one such mutation with NO archived gate. Its post-lock checks —
// `current != instance` and `UserKilled()` — cannot see an archive: ArchiveSession
// holds this SAME op-lock, commits LiveArchived under it, and leaves the same
// instance tracked in m.instances (an archived row stays listed and restorable).
// So a SetPRInfo that resolved a live session and then queued behind an archive
// arrives with current == instance and UserKilled() false, passes every existing
// check, and overwrites the PR info the archive just preserved — then persists
// the loss, so the record is still wrong after a restore.
//
// This is the identical hole tabMutationTarget documents and closes for
// CloseTab/RenameTab/ReorderTab (see its doc comment, and
// TestCloseTab_ArchiveWinningOpLockRaceKeepsWebTab, which this mirrors).
//
// The window is real rather than theoretical: the TUI kicks off `gh pr view`
// against a target captured before an archive can begin and sends SetPRInfo when
// that async result lands, so the request routinely outlives the state it was
// resolved against.
func TestSetPRInfo_ArchiveWinningOpLockRaceKeepsPRInfo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "worker"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")

	// The PR info the archive is entitled to preserve.
	original := session.PRInfoData{Number: 42, Title: "original", URL: "https://example.test/pr/42", State: "OPEN"}
	if err := manager.SetPRInfo(SetPRInfoRequest{Title: title, RepoID: repo.ID, PRInfo: original}); err != nil {
		t.Fatalf("SetPRInfo(original): %v", err)
	}

	key := daemonInstanceKey(repo.ID, title)
	opLock := manager.opLockFor(key)
	opLock.Lock()

	done := make(chan error, 1)
	go func() {
		done <- manager.SetPRInfo(SetPRInfoRequest{
			Title:  title,
			RepoID: repo.ID,
			PRInfo: session.PRInfoData{Number: 99, Title: "corrupted", URL: "https://example.test/pr/99", State: "CLOSED"},
		})
	}()

	// The write must PARK on the op-lock rather than return: that is what proves it
	// resolved the session while it was still live, which is the only interleaving
	// that reaches the post-lock gate. Without this the test could pass on a
	// front-door archived check and prove nothing about the race.
	select {
	case err := <-done:
		opLock.Unlock()
		t.Fatalf("SetPRInfo returned while the op-lock was held (it never raced the archive): %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	// The archive commits under the op-lock and leaves the same pointer tracked —
	// exactly what ArchiveSession does between BeginArchive and releasing the lock.
	inst.SetStatusForTest(session.Archived)
	opLock.Unlock()

	err = <-done
	if err == nil {
		t.Fatal("SetPRInfo queued behind an archive returned nil: it overwrote the PR info the archive had just preserved")
	}
	if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("SetPRInfo lost-the-race error = %v, want the same actionable archived rejection the other session mutations give", err)
	}

	got := inst.GetPRInfo()
	if got == nil {
		t.Fatal("archived session PR info was cleared entirely; want the original #42 intact")
	}
	if got.Number != original.Number || got.Title != original.Title || got.URL != original.URL || got.State != original.State {
		t.Fatalf("archived session PR info = %+v, want the original %+v intact", *got, original)
	}
}

// TestSetPRInfo_RejectsAlreadyArchivedSession is the front-door half: a session
// that was ALREADY archived when the request arrived must be refused too, with
// the same actionable message. Without this the race gate above could be
// satisfied by something that only fires on the exact interleaving.
func TestSetPRInfo_RejectsAlreadyArchivedSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "worker"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")

	original := session.PRInfoData{Number: 7, Title: "keep me", URL: "https://example.test/pr/7", State: "OPEN"}
	if err := manager.SetPRInfo(SetPRInfoRequest{Title: title, RepoID: repo.ID, PRInfo: original}); err != nil {
		t.Fatalf("SetPRInfo(original): %v", err)
	}

	inst.SetStatusForTest(session.Archived)

	err = manager.SetPRInfo(SetPRInfoRequest{
		Title:  title,
		RepoID: repo.ID,
		PRInfo: session.PRInfoData{Number: 99, Title: "corrupted", URL: "https://example.test/pr/99", State: "CLOSED"},
	})
	if err == nil {
		t.Fatal("SetPRInfo on an archived session returned nil: archive is inert in BOTH directions (#1809)")
	}
	if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("SetPRInfo archived error = %v, want an actionable archived rejection", err)
	}

	got := inst.GetPRInfo()
	if got == nil || got.Number != original.Number || got.Title != original.Title {
		t.Fatalf("archived session PR info = %+v, want the original %+v intact", got, original)
	}
}
