package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type preparedHookProgress struct {
	progress        *hookProgress
	temporary       string
	retainArtifacts bool
}

type hookProgressPublicationRollback struct {
	prepared *preparedHookProgress
	path     string
	renamed  bool
}

type hookProgressPrepareFlight struct {
	done     chan struct{}
	prepared *preparedHookProgress
	err      error
	timedOut bool
}

var hookProgressPrepareFlights = struct {
	sync.Mutex
	byDirectory map[string]*hookProgressPrepareFlight
}{byDirectory: make(map[string]*hookProgressPrepareFlight)}

var hookProgressPrepare = prepareHookProgress

var hookProgressDiscardPrepared = func(prepared *preparedHookProgress, journal string, renamed bool) {
	prepared.discard(journal, renamed)
}

// Failed publication cleanup never runs in the callback that owns .progress.
// If the shared journal name was reached, retire that name under a fresh lock,
// then delete only its non-resumable name and unique receipts after releasing
// the lock. An inconclusive retirement retains everything for pruning.
func (r *hookProgressPublicationRollback) cleanup() error {
	if r == nil || r.prepared == nil {
		return nil
	}
	journal, renamed := r.path, r.renamed
	var rollbackErr error
	if renamed {
		retainArtifacts := true
		rollbackErr = withHookProgressLock(filepath.Dir(r.path), func(string, os.FileInfo) error {
			current, readErr := readHookProgress(r.path)
			if os.IsNotExist(readErr) {
				renamed = false
				retainArtifacts = false
				return nil
			}
			if readErr != nil {
				return readErr
			}
			p := r.prepared.progress
			if current.Directory != p.Directory || current.SessionID != p.SessionID || current.Generation != p.Generation {
				renamed = false
				retainArtifacts = false
				return nil
			}
			journal = filepath.Join(filepath.Dir(r.path), "retired-"+filepath.Base(current.Directory)+".json")
			return os.Rename(r.path, journal)
		})
		// Neither a failed directory barrier nor a second failed lock/rename can
		// prove a namespace transition durable. Retain the journal and receipts;
		// a later retention pass re-establishes the barrier before deletion.
		r.prepared.retainArtifacts = retainArtifacts
	}
	return errors.Join(rollbackErr, boundedDiscardHookProgress(r.prepared, journal, renamed))
}

func boundedDiscardHookProgress(prepared *preparedHookProgress, journal string, renamed bool) error {
	done := make(chan struct{})
	discard := hookProgressDiscardPrepared
	go func() {
		discard(prepared, journal, renamed)
		close(done)
	}()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("hook journal rollback exceeded %s: %w", relocationIdentityTimeout, context.DeadlineExceeded)
	}
}

// Preparing a journal touches only unique, unpublished names. It may therefore
// finish and clean itself after a timeout without mutating shared state outside
// .progress. The atomic rename remains synchronous in the locked commit phase.
func boundedPrepareHookProgress(run hookRun, commands []string, prefix, generation, path string, worktreeIdentity *hookWorktreeIdentity, resumeDisabled bool) (*preparedHookProgress, error) {
	directory := filepath.Dir(path)
	hookProgressPrepareFlights.Lock()
	if hookProgressPrepareFlights.byDirectory[directory] != nil {
		hookProgressPrepareFlights.Unlock()
		return nil, fmt.Errorf("hook journal preparation under %s is still running after an earlier deadline: %w", directory, context.DeadlineExceeded)
	}
	flight := &hookProgressPrepareFlight{done: make(chan struct{})}
	hookProgressPrepareFlights.byDirectory[directory] = flight
	prepare := hookProgressPrepare
	hookProgressPrepareFlights.Unlock()
	go func() {
		prepared, err := prepare(run, commands, prefix, generation, path, worktreeIdentity, resumeDisabled)
		hookProgressPrepareFlights.Lock()
		if flight.timedOut {
			hookProgressPrepareFlights.Unlock()
			if prepared != nil {
				prepared.discard("", false)
			}
			hookProgressPrepareFlights.Lock()
		} else {
			flight.prepared, flight.err = prepared, err
		}
		if hookProgressPrepareFlights.byDirectory[directory] == flight {
			delete(hookProgressPrepareFlights.byDirectory, directory)
		}
		close(flight.done)
		hookProgressPrepareFlights.Unlock()
	}()
	return waitForHookProgressPreparation(directory, path, flight)
}

func waitForHookProgressPreparation(directory, path string, flight *hookProgressPrepareFlight) (*preparedHookProgress, error) {
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.prepared, flight.err
	case <-timer.C:
		hookProgressPrepareFlights.Lock()
		if hookProgressPrepareFlights.byDirectory[directory] == flight {
			flight.timedOut = true
			hookProgressPrepareFlights.Unlock()
			return nil, fmt.Errorf("timed out after %s while preparing hook journal %s: %w", relocationIdentityTimeout, path, context.DeadlineExceeded)
		}
		hookProgressPrepareFlights.Unlock()
		<-flight.done
		return flight.prepared, flight.err
	}
}

func prepareHookProgress(run hookRun, commands []string, prefix, generation, path string, worktreeIdentity *hookWorktreeIdentity, resumeDisabled bool) (_ *preparedHookProgress, resultErr error) {
	dir, err := os.MkdirTemp(filepath.Dir(path), "entries-")
	if err != nil {
		return nil, err
	}
	prepared := &preparedHookProgress{progress: &hookProgress{
		leaseMu:   &sync.Mutex{},
		SessionID: run.scopeSessionID, Commands: commands, Passthrough: run.passthrough, Worktree: run.worktreePath,
		Prefix: prefix, Generation: generation, Directory: dir, WorktreeIdentity: worktreeIdentity, ResumeDisabled: resumeDisabled,
	}}
	defer func() {
		if resultErr != nil {
			prepared.discard("", false)
		}
	}()
	if run.leaseProgress {
		prepared.progress.lease, err = newHookProgressLease(dir)
		if err != nil {
			return nil, err
		}
		prepared.progress.leaseHolds = 1
	}
	data, err := json.Marshal(prepared.progress)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".progress-")
	if err != nil {
		return nil, err
	}
	prepared.temporary = f.Name()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	resultErr = errors.Join(err, f.Close())
	if resultErr != nil {
		return nil, resultErr
	}
	return prepared, nil
}

func (p *preparedHookProgress) discard(journal string, renamed bool) {
	if p == nil {
		return
	}
	if p.progress != nil && p.progress.lease != nil {
		p.progress.releaseLease()
	}
	if p.retainArtifacts {
		return
	}
	if renamed && journal != "" {
		_ = os.Remove(journal)
	} else if p.temporary != "" {
		_ = os.Remove(p.temporary)
	}
	if p.progress != nil {
		_ = os.RemoveAll(p.progress.Directory)
	}
}
