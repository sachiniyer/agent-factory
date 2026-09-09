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
	progress  *hookProgress
	temporary string
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
		_ = p.progress.lease.Close()
		p.progress.lease = nil
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
