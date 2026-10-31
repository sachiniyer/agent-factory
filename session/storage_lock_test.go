package session

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DeleteInstanceByStableID is the LAST step of a session kill, and the #1917
// field evidence points straight at it: the daemon's restart log ("tombstoned
// record survived its teardown") proves the kill-intent tombstone reached disk
// and the record delete did not. The delete took a blocking flock, so any other
// af process holding it parked the kill forever — while the kill held the guard
// that makes every retry fail and the session undeletable.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: with config.WithFileLock (no deadline) this
// test hangs until the go test timeout instead of failing its deadline.
func TestDeleteInstanceByStableID_ContendedLock_TimesOutInsteadOfHanging(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const repoID = "deadbeef"
	path, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := config.SaveRepoInstances(repoID, []byte(`[{"title":"doomed"}]`)); err != nil {
		t.Fatalf("seed instances: %v", err)
	}

	prev := InstanceDeleteLockTimeout
	InstanceDeleteLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { InstanceDeleteLockTimeout = prev })

	// Hold the same flock from an independent file description, exactly as a
	// second af process would (flock contends per open-file-description).
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}

	storage, err := NewStorage(config.LoadState(), repoID)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, derr := storage.DeleteInstanceByStableID("doomed", "")
		done <- derr
	}()

	select {
	case derr := <-done:
		if !errors.Is(derr, config.ErrLockTimeout) {
			t.Fatalf("delete must fail with a retryable ErrLockTimeout, got: %v", derr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HUNG: DeleteInstanceByStableID never returned against a held instances lock — " +
			"this is the #1917 wedge: the kill holds killsInFlight across this call, so the " +
			"session becomes undeletable until the daemon restarts")
	}

	// Releasing the contention must make the very same delete succeed: the bound
	// has to leave a RETRYABLE state, not a poisoned one.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	deleted, err := storage.DeleteInstanceByStableID("doomed", "")
	if err != nil {
		t.Fatalf("delete after the lock cleared must succeed, got: %v", err)
	}
	if !deleted {
		t.Fatal("delete after the lock cleared reported nothing deleted")
	}
}

// The daemon's missing-file restore checkpoints through SaveInstancesWithin: a
// held instances lock must give a bounded ErrLockTimeout, with NO write (the
// checkpoint never proceeds unlocked), and the same call must succeed once the
// lock is free (#6048 review).
func TestSaveInstancesWithin_HeldLock(t *testing.T) {
	cases := []struct {
		name   string
		budget time.Duration
	}{
		{"short budget", 100 * time.Millisecond},
		{"spent budget still returns", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			storage, err := NewStorage(config.LoadState(), "")
			if err != nil {
				t.Fatalf("NewStorage: %v", err)
			}
			live := &Instance{Title: "live", Path: repoPath, started: true}
			live.SetStatusForTest(Running)
			repoID := live.repoIDForStorage()
			path, err := config.RepoInstancesPath(repoID)
			if err != nil {
				t.Fatalf("RepoInstancesPath: %v", err)
			}
			if err := config.SaveRepoInstances(repoID, []byte(`[{"title":"seed"}]`)); err != nil {
				t.Fatalf("seed: %v", err)
			}
			seedBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read seed: %v", err)
			}
			seed := string(seedBytes)

			f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
			if err != nil {
				t.Fatalf("open lock: %v", err)
			}
			defer f.Close()
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatalf("flock: %v", err)
			}

			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- storage.SaveInstancesWithin([]*Instance{live}, tc.budget) }()
			select {
			case serr := <-done:
				if !errors.Is(serr, config.ErrLockTimeout) {
					t.Fatalf("a held lock must give ErrLockTimeout, got: %v", serr)
				}
				if took := time.Since(start); took > 3*time.Second {
					t.Fatalf("returned after %s, not bounded by the budget", took)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("HUNG: SaveInstancesWithin waited on a held instances lock past its budget")
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != seed {
				t.Fatalf("no write may happen without the lock; file = %q, err = %v", raw, err)
			}

			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatalf("unlock: %v", err)
			}
			if err := storage.SaveInstancesWithin([]*Instance{live}, time.Second); err != nil {
				t.Fatalf("once the lock is free the same call must succeed: %v", err)
			}
			raw, _ = os.ReadFile(path)
			if string(raw) == seed {
				t.Fatal("the checkpoint did not write after the lock cleared")
			}
		})
	}
}

// The checkpoint rewrites a repo's instances.json wholesale from the in-memory
// group. A same-titled on-disk row whose stable ID differs from the in-memory
// instance is a replacement the daemon has not observed (refreshDaemonInstances
// keeps the old in-memory pointer by title even when the disk row's ID differs),
// so the checkpoint would write the stale instance and clobber the replacement's
// identity — the data-loss class persistInstanceData's title+ID guard prevents.
// The checkpoint must drop the stale in-memory row and preserve the on-disk
// replacement verbatim instead (#6048 review).
func TestSaveInstances_ReplacementIdentityNotOverwrittenByStaleInMemory(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	// The on-disk row is a replacement: same title, different stable ID.
	replacement := InstanceData{Title: "a", ID: "id-new", Path: repoPath}
	seedDisk(t, state, repoPath, []InstanceData{replacement})

	// The in-memory instance is the stale one the daemon kept by title.
	stale := makeAliveInstance("a", repoPath)
	stale.ID = "id-old"
	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{stale}))

	got := rowsByTitle(t, state, repoPath)
	require.Contains(t, got, "a")
	assert.Equal(t, "id-new", got["a"].ID,
		"the on-disk replacement's stable ID must not be clobbered by the stale in-memory instance")
}

// A conflicting-ID replacement that is itself a Loading/Deleting row with no
// retention marker looks like a disposable legacy transient to the ghost reaper.
// The checkpoint must still keep it verbatim: with the stale in-memory row
// dropped, reaping it would lose BOTH records and orphan the replacement
// (#6048 review).
func TestSaveInstances_TransientReplacementRowIsRetainedVerbatim(t *testing.T) {
	for name, status := range map[string]Status{"loading": Loading, "deleting": Deleting} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			replacement := InstanceData{Title: "a", ID: "id-new", Path: repoPath, Status: status}
			require.True(t, isLegacyTransientGhostRow(replacement), "fixture: must look like a disposable transient")
			seedDisk(t, state, repoPath, []InstanceData{replacement})

			stale := makeAliveInstance("a", repoPath)
			stale.ID = "id-old"
			storage, err := NewStorage(state, "")
			require.NoError(t, err)
			require.NoError(t, storage.SaveInstances([]*Instance{stale}))

			got := rowsByTitle(t, state, repoPath)
			require.Contains(t, got, "a", "the transient replacement must not be reaped")
			assert.Equal(t, "id-new", got["a"].ID)
		})
	}
}

// A spent shared budget must be a genuinely non-blocking attempt: no extra wait
// per remaining repo (#6048 review). The timeout error names the budget it
// waited, so a zero budget reads "after 0s", not a minimum floor.
func TestSaveInstancesWithin_SpentBudgetIsNonBlocking(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	storage, err := NewStorage(config.LoadState(), "")
	require.NoError(t, err)
	live := &Instance{Title: "live", Path: repoPath, started: true}
	live.SetStatusForTest(Running)
	path, err := config.RepoInstancesPath(live.repoIDForStorage())
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(live.repoIDForStorage(), []byte(`[{"title":"seed"}]`)))

	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX))

	err = storage.SaveInstancesWithin([]*Instance{live}, 0)
	require.ErrorIs(t, err, config.ErrLockTimeout)
	assert.Contains(t, err.Error(), "after 0s", "a spent budget must not be padded with a minimum wait")
}

// With duplicate disk rows for one title, only the authoritative one (newest
// UpdatedAt, as dedupeInstanceData resolves them) is compared with the live
// instance. An older stale duplicate with a different ID must not be taken for a
// replacement and supersede the current row that matches (#6048 review).
func TestSaveInstances_OlderDuplicateDiskRowIsNotAReplacement(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	now := time.Now()
	seedDisk(t, state, repoPath, []InstanceData{
		{Title: "a", ID: "id-old", Path: repoPath, UpdatedAt: now.Add(-time.Hour)},
		{Title: "a", ID: "id-current", Path: repoPath, UpdatedAt: now},
	})
	live := makeAliveInstance("a", repoPath)
	live.ID = "id-current"
	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{live}))

	got := readDisk(t, state, repoPath)
	require.Len(t, got, 1, "the live instance's row survives and the stale duplicate is not resurrected: %+v", got)
	assert.Equal(t, "id-current", got[0].ID)
}
