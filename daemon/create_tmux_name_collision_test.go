package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestReserveCreateRejectsConcurrentTmuxNameCollision exercises the real
// reservation lifecycle: the first create owns its runtime name before it waits
// on the per-repo start lock, so a second punctuation-variant create is refused
// while that reservation is live.
func TestReserveCreateRejectsConcurrentTmuxNameCollision(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	repoPath := setupControlRepo(t)
	m, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, _, release, _, err := m.reserveCreate(CreateSessionRequest{
		Title: "a/b", RepoPath: repoPath, Program: "claude",
	})
	if err != nil {
		t.Fatalf("reserve first create: %v", err)
	}
	defer release()

	_, _, _, _, err = m.reserveCreate(CreateSessionRequest{
		Title: "a_b", RepoPath: repoPath, Program: "claude",
	})
	if err == nil {
		t.Fatal("second create reserved the same repo-scoped tmux name")
	}
	if !strings.Contains(err.Error(), "tmux") {
		t.Fatalf("collision error does not name the runtime namespace: %v", err)
	}
}

// TestValidateTitleRejectsTmuxNameCollisions pins the runtime-name half of
// create admission. Git can keep branches for "a/b" and "a_b" distinct, but
// tmux's positive name policy maps both titles onto the same repo-scoped session
// name. Every source of an existing claim must reject that collision before a
// worktree is created: an in-flight reservation, a live instance, and a row that
// exists only on disk.
func TestValidateTitleRejectsTmuxNameCollisions(t *testing.T) {
	fakeBin := t.TempDir()
	tmuxPath := filepath.Join(fakeBin, "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	const (
		repoID    = "repo-id"
		existing  = "a/b"
		candidate = "a_b"
	)
	repoPath := t.TempDir()

	for _, tc := range []struct {
		name  string
		claim func(*Manager) []session.InstanceData
	}{
		{
			name: "in-flight reservation",
			claim: func(m *Manager) []session.InstanceData {
				m.reservedTitles[daemonInstanceKey(repoID, existing)] = struct{}{}
				return nil
			},
		},
		{
			name: "live instance",
			claim: func(m *Manager) []session.InstanceData {
				m.instances[daemonInstanceKey(repoID, existing)] = &session.Instance{Title: existing}
				return nil
			},
		},
		{
			name: "disk row",
			claim: func(*Manager) []session.InstanceData {
				return []session.InstanceData{{Title: existing}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{
				cfg:               config.DefaultConfig(),
				instances:         make(map[string]*session.Instance),
				reservedTitles:    make(map[string]struct{}),
				reservedTmuxNames: make(map[string]string),
			}
			if tc.name == "in-flight reservation" {
				m.reservedTmuxNames[daemonInstanceKey(repoID, tmux.SanitizedNameForRepo(existing, repoPath))] = existing
			}
			disk := tc.claim(m)

			err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, "claude", runtimeNamespaceLocalTmux, false, disk)
			if err == nil {
				t.Fatal("accepted two titles that map to the same repo-scoped tmux session name")
			}
			for _, want := range []string{existing, candidate, "tmux"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("collision error %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestTmuxNameCollisionIsScopedToTwoLocalRuntimes prevents the new admission
// rule from becoming a phantom restriction on sandbox names. A runtime that has
// no tmux session on this host neither owns nor conflicts in that namespace.
func TestTmuxNameCollisionIsScopedToTwoLocalRuntimes(t *testing.T) {
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	const (
		repoID    = "repo-id"
		existing  = "a/b"
		candidate = "a_b"
	)
	repoPath := t.TempDir()
	m := &Manager{
		cfg:               config.DefaultConfig(),
		instances:         make(map[string]*session.Instance),
		reservedTitles:    make(map[string]struct{}),
		reservedTmuxNames: make(map[string]string),
	}

	localRow := []session.InstanceData{{Title: existing, BackendType: "local"}}
	if err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, "claude", runtimeNamespaceSandbox, false, localRow); err != nil {
		t.Fatalf("sandbox create was blocked by a host tmux name it will not claim: %v", err)
	}
	remoteRow := []session.InstanceData{{Title: existing, BackendType: "docker"}}
	if err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, "claude", runtimeNamespaceLocalTmux, false, remoteRow); err != nil {
		t.Fatalf("local create was blocked by a sandbox row with no host tmux name: %v", err)
	}
}
