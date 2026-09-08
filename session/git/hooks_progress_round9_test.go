//go:build linux

package git

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressPublicationLockTimesOut(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.MkdirAllUnderAFHome(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := relocationIdentityTimeout
	relocationIdentityTimeout = 80 * time.Millisecond
	locked, release := make(chan struct{}), make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- config.WithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error { close(locked); <-release; return nil })
	}()
	select {
	case <-locked:
	case err := <-lockDone:
		t.Fatal(err)
	}
	done := make(chan struct{})
	var publishErr error
	go func() {
		_, publishErr = newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
		close(done)
	}()
	t.Cleanup(func() { close(release); <-lockDone; <-done; relocationIdentityTimeout = original })
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("publication waited indefinitely for the held progress lock")
	}
	if !errors.Is(publishErr, config.ErrLockTimeout) {
		t.Fatalf("publication error = %v, want lock timeout", publishErr)
	}
	if !strings.Contains(publishErr.Error(), "hook journal lock held by another process; hooks could not start") {
		t.Fatalf("missing timeout guidance: %v", publishErr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("timed-out publisher wrote a journal: %v", err)
	}
}

func TestHookProgressUnheldPublicationLockPublishes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := hookProgressPath(p.Worktree)
	if _, err := readHookProgress(path); err != nil {
		t.Fatal(err)
	}
}

func TestHookProgressRetiredActiveOwnerReclaimed(t *testing.T) {
	for _, kind := range []string{"retired", "live", "scope", "lease", "young", "missing-exit"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			installScopeShim(t)
			if err := config.SaveRepoInstances("test", json.RawMessage(`[{"id":"owner"}]`)); err != nil {
				t.Fatal(err)
			}
			p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner", leaseProgress: kind == "lease"}, []string{"true"}, "af-hook-owner", "test")
			if err != nil {
				t.Fatal(err)
			}
			if p.lease != nil {
				t.Cleanup(func() { _ = p.lease.Close() })
			}
			if err := os.Mkdir(p.receipt(0), 0700); err != nil {
				t.Fatal(err)
			}
			if kind != "missing-exit" {
				if err := os.WriteFile(filepath.Join(p.receipt(0), "exit"), []byte("0\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.markFinished(); err != nil {
				t.Fatal(err)
			}
			path, _ := hookProgressPath(p.Worktree)
			dir := filepath.Dir(path)
			if kind != "live" {
				retired := filepath.Join(dir, "retired-"+filepath.Base(p.Directory)+".json")
				if err := os.Rename(path, retired); err != nil {
					t.Fatal(err)
				}
				path = retired
			}
			if kind == "scope" {
				installSurvivorSystemctl(t, "echo 'af-hook-owner-test-0.scope loaded active running Hook'\nexit 0\n")
			}
			now := time.Now().Add(time.Minute)
			if kind == "live" {
				now = time.Now().Add(15 * 24 * time.Hour)
			} // Ownership, not the history quota, protects this journal.
			if kind == "young" {
				now = time.Now()
			}
			pruneHookProgress(dir, now)
			remove := kind == "retired" || kind == "missing-exit"
			for _, file := range []string{path, p.Directory} {
				_, err := os.Stat(file)
				if remove && !os.IsNotExist(err) {
					t.Errorf("retired active-owner artifact retained: %s", file)
				}
				if !remove && err != nil {
					t.Errorf("protected %s artifact removed: %v", kind, err)
				}
			}
			if kind == "lease" || kind == "scope" || kind == "young" {
				if kind == "lease" {
					if err := p.lease.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "scope" {
					installSurvivorSystemctl(t, "exit 0\n")
				}
				pruneHookProgress(dir, time.Now().Add(time.Minute))
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("retirement did not resume after protection cleared: %v", err)
				}
			}
		})
	}
}

func TestHookProgressRetirementRetriesRemovalError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installScopeShim(t)
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	p.finish()
	path, err := hookProgressPath(g.worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	original := hookProgressRemove
	failed := true
	hookProgressRemove = func(path string, progress *hookProgress) error {
		if failed {
			failed = false
			return errors.New("injected removal failure")
		}
		return original(path, progress)
	}
	t.Cleanup(func() { hookProgressRemove = original })
	if err := g.retireHookProgress(); err != nil {
		t.Fatal(err)
	}
	if g.hooksRetirementDone == nil {
		t.Fatal("reclamation failure did not schedule a retry")
	}
	waitForClosed(t, g.hooksRetirementDone, 5*time.Second, "reclamation retry did not finish")
	for _, file := range []string{path, p.Directory} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("retry retained %s: %v", file, err)
		}
	}
}

func TestHookProgressRetirementRetriesHalfRetiredJournal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installScopeShim(t)
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	p.finish()
	path, err := hookProgressPath(g.worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	original := hookProgressRemove
	failed := true
	hookProgressRemove = func(path string, progress *hookProgress) error {
		if failed {
			failed = false
			retired := filepath.Join(filepath.Dir(path), "retired-"+filepath.Base(progress.Directory)+".json")
			if err := os.Rename(path, retired); err != nil {
				return err
			}
			return errors.New("injected post-rename failure")
		}
		return original(path, progress)
	}
	t.Cleanup(func() { hookProgressRemove = original })
	if err := g.retireHookProgress(); err != nil {
		t.Fatal(err)
	}
	waitForClosed(t, g.hooksRetirementDone, 5*time.Second, "half-retired journal retry did not finish")
	for _, file := range []string{path, p.Directory} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("half-retired retry retained %s: %v", file, err)
		}
	}
}
