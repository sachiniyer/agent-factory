package daemon

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestOutageEndToEnd_LostRestoreAndReplay is the shared #1108/#1129 e2e: the
// full 2026-07-03-outage story on a real (sandboxed, package-private) tmux
// server, through the real LocalBackend — no fakes on the session side.
//
//  1. A real session is created (a cheap non-agent program via
//     program_overrides; #1132's generic readiness accepts it).
//  2. Its tmux session is killed out from under it — the outage.
//  3. The daemon status pass classifies it Lost (#1108 PR 1): no kill
//     intent on record, so recovery-eligible. Not Dead, not reaped.
//  4. A watch task fires events during the outage; every delivery fails and
//     lands in the durable per-task queue (#1129 PR 3) instead of dropping.
//  5. The restore loop re-spawns the session in place (#1108 PR 2).
//  6. The drainer replays the queued events into the RESTORED session, in
//     emission order, and the queue files clean up (#1129 PR 3/4).
//
// Everything the joint design promised, observable in one pane capture.
func TestOutageEndToEnd_LostRestoreAndReplay(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	// Cheap real "agent": prints a banner and idles. The resolved command has
	// no agent token, so no agent flags are injected and readiness is the
	// generic any-output heuristic (#1132) — at create AND at recover.
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{"claude": "sh -c 'echo agent-ready; exec sleep 600'"}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "worker",
		RepoPath: repoPath,
		Program:  "claude",
		InPlace:  true, // repo's own tree: kill/cleanup never touches it (#1107)
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() {
		_, _ = manager.KillSession(KillSessionRequest{Title: "worker", RepoID: repo.ID})
	})
	key := daemonInstanceKey(repo.ID, "worker")
	manager.mu.Lock()
	inst := manager.instances[key]
	manager.mu.Unlock()
	if inst == nil || data.TmuxName == "" {
		t.Fatalf("created session not tracked or missing tmux name: %+v", data)
	}

	// ---- The outage: the tmux session dies out from under the live record.
	if out, err := exec.Command("tmux", "kill-session", "-t", "="+data.TmuxName).CombinedOutput(); err != nil {
		t.Fatalf("kill-session: %v: %s", err, out)
	}

	// The status pass must classify it Lost — recovery-eligible, no tombstone.
	waitUntil(t, 10*time.Second, "the vanished session to be classified Lost", func() bool {
		manager.RefreshStatuses()
		return inst.GetStatus() == session.Lost
	})

	// ---- Events fire DURING the outage: a real watch script, deliveries
	// into the dead session via the real SendPrompt path (tmux send-keys),
	// which fails and queues.
	dir := t.TempDir()
	script := `echo replay-e1; echo replay-e2; echo replay-e3; sleep 300`
	s, _ := newTestSupervisor(t, staticTasks(watchTask("ab140001", script, dir)))
	s.deliver = func(taskID, line string) error {
		return manager.SendPrompt(SendPromptRequest{Title: "worker", RepoID: repo.ID, Prompt: line})
	}
	queueDir, _ := s.queueDir()
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "all outage events to be queued durably", func() bool {
		return newEventQueue(queueDir, "ab140001").pendingCount() == 3
	})

	// ---- The outage ends: the restore loop re-spawns the session in place.
	zeroRestoreBackoff(t)
	waitUntil(t, 10*time.Second, "the Lost session to be restored", func() bool {
		manager.RestoreLostSessions()
		return inst.GetStatus() != session.Lost && inst.TmuxAlive()
	})

	// ---- The drainer replays into the restored session, in emission order.
	// The sent text is echoed by the pane's tty line discipline, so the
	// replayed lines are observable in a real capture of the restored pane.
	waitUntil(t, 30*time.Second, "queued events to replay into the restored pane in order", func() bool {
		content, err := inst.AgentServer().Preview(0, false)
		if err != nil {
			return false
		}
		i1 := strings.Index(content.Content, "replay-e1")
		i2 := strings.Index(content.Content, "replay-e2")
		i3 := strings.Index(content.Content, "replay-e3")
		return i1 >= 0 && i2 > i1 && i3 > i2
	})
	waitUntil(t, 10*time.Second, "the drained queue files to clean up", func() bool {
		return newEventQueue(queueDir, "ab140001").pendingCount() == 0
	})

	// The restored pane is the SAME session identity: re-spawned under its
	// exact persisted tmux name. A fresh capture by that name proving the
	// replayed content is the strongest identity check available.
	if got := inst.GetStatus(); got == session.Lost || got == session.Dead {
		t.Fatalf("session status = %v after the full recovery story; want live", got)
	}
}
