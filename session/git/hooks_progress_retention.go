package git

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
)

// Hook progress retention has four cooperating authorities. A live owner row
// protects every resumable journal; an archived or tombstoned row is terminal
// and therefore permits eventual reclamation. A runner or pending create holds
// runner.lock across gaps where no scope is visible, and the active runner owns
// exactly one lease hold. Retirement first removes the resumable journal
// name, then deletes receipts; a retired name can never be adopted and is safe
// to finish reclaiming after the shared liveness probes and lease check. Unknown
// ownership, metadata, or manager liveness leaves all affected state in place.
// The complete owner snapshot is loaded under one deadline before .progress is
// acquired. The lock phases use bounded filesystem helpers to collect and later
// revalidate candidates; slow manager liveness probes run between those phases,
// outside .progress. The final lock phase publishes only revalidated atomic
// journal-name transitions, and receipt-tree removal runs afterward in one
// bounded, deduplicated batch.
// The age and count limits otherwise match #4045's hooklog retention policy.
const (
	keptProgressLimit = 20
	keptProgressAge   = 14 * 24 * time.Hour
	progressGraceAge  = 5 * time.Second
)

type keptProgress struct {
	path       string
	progress   *hookProgress
	modified   time.Time
	incomplete bool
	retired    bool
}

func pruneHookProgress(dir string, now time.Time) {
	ownerSnapshot, err := boundedHookProgressOwners()
	if err == nil {
		err = pruneHookProgressWithSnapshot(dir, now, ownerSnapshot)
	}
	if err != nil {
		log.WarningLog.Printf("cannot prune inactive hook journals: %v", err)
	}
}

func hookProgressActivity(path string, p *hookProgress) (time.Time, bool, error) {
	info, err := BoundedLstat(path)
	if err != nil {
		return time.Time{}, false, err
	}
	modified, complete := info.ModTime(), true
	files := []struct {
		path     string
		required bool
	}{{path: p.Directory}, {path: filepath.Join(p.Directory, "finished"), required: true}}
	for index := range p.Commands {
		files = append(files,
			struct {
				path     string
				required bool
			}{path: p.receipt(index)},
			struct {
				path     string
				required bool
			}{path: filepath.Join(p.receipt(index), "exit"), required: true},
		)
	}
	for _, file := range files {
		info, err := BoundedLstat(file.path)
		if os.IsNotExist(err) {
			if file.required {
				complete = false
			}
			continue
		}
		if err != nil {
			return time.Time{}, false, err
		}
		if file.required && !info.Mode().IsRegular() {
			complete = false
		}
		if info.ModTime().After(modified) {
			modified = info.ModTime()
		}
	}
	return modified, !complete, nil
}

func loadHookProgressOwners(load func() (map[string]json.RawMessage, []config.RepoInstancesSkip, error)) (map[string]bool, error) {
	repos, skipped, err := load()
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return nil, skipped[0].Err
	}
	owners := make(map[string]bool)
	for _, data := range repos {
		var rows []struct {
			ID         string `json:"id"`
			Liveness   int    `json:"liveness"`
			Status     int    `json:"status"`
			UserKilled bool   `json:"user_killed"`
		}
		if err := json.Unmarshal(data, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			// Append-only persisted enums: session.LiveArchived=5 and
			// legacy session.Archived=6. The git package cannot import session.
			archived := row.Liveness == 5 || (row.Liveness == 0 && row.Status == 6)
			owners[row.ID] = owners[row.ID] || (!archived && !row.UserKilled)
		}
	}
	return owners, nil
}

var hookProgressOwnerLoad = config.LoadAllRepoInstancesReportingSkipDetails

type hookProgressOwnerFlight struct {
	done     chan struct{}
	snapshot hookProgressOwnerSnapshot
	err      error
	timedOut bool
}

type hookProgressOwnerSnapshot struct {
	owners        map[string]bool
	hookDirectory string
}

var hookProgressOwnerFlights = struct {
	sync.Mutex
	byHome map[string]*hookProgressOwnerFlight
}{byHome: make(map[string]*hookProgressOwnerFlight)}

// boundedHookProgressOwners gives the complete cross-repository scan one total
// identity-probe deadline before any home-wide progress lock is acquired. A
// stalled instances file therefore yields an unknown retention answer without
// serially charging one timeout per repository or pinning hook publication.
func boundedHookProgressOwners() (hookProgressOwnerSnapshot, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return hookProgressOwnerSnapshot{}, err
	}
	hookProgressOwnerFlights.Lock()
	if active := hookProgressOwnerFlights.byHome[home]; active != nil {
		if active.timedOut {
			hookProgressOwnerFlights.Unlock()
			return hookProgressOwnerSnapshot{}, fmt.Errorf("hook progress owner scan for %s is still running after an earlier deadline: %w", home, context.DeadlineExceeded)
		}
		hookProgressOwnerFlights.Unlock()
		return waitForHookProgressOwners(home, active)
	}
	flight := &hookProgressOwnerFlight{done: make(chan struct{})}
	hookProgressOwnerFlights.byHome[home] = flight
	load := hookProgressOwnerLoad
	hookProgressOwnerFlights.Unlock()
	go func() {
		// These raw probes are inside this flight's one total deadline. Keeping
		// them here avoids paying a separate bounded-flight timeout per repo.
		pinnedHome := pathutil.ResolveForCompare(home)
		identity, identityErr := os.Lstat(pinnedHome)
		if identityErr == nil {
			flight.snapshot.owners, flight.err = loadHookProgressOwners(load)
			if flight.err == nil {
				current, currentErr := os.Lstat(pinnedHome)
				if currentErr != nil {
					flight.err = currentErr
				} else if !os.SameFile(identity, current) {
					flight.err = fmt.Errorf("AF home changed while hook owners were loaded")
				} else {
					flight.snapshot.hookDirectory = filepath.Join(pinnedHome, "logs", "hooks")
				}
			}
		}
		if identityErr != nil {
			flight.err = identityErr
		}
		hookProgressOwnerFlights.Lock()
		if hookProgressOwnerFlights.byHome[home] == flight {
			delete(hookProgressOwnerFlights.byHome, home)
		}
		close(flight.done)
		hookProgressOwnerFlights.Unlock()
	}()
	return waitForHookProgressOwners(home, flight)
}

func waitForHookProgressOwners(home string, flight *hookProgressOwnerFlight) (hookProgressOwnerSnapshot, error) {
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.snapshot, flight.err
	case <-timer.C:
		hookProgressOwnerFlights.Lock()
		if hookProgressOwnerFlights.byHome[home] == flight {
			flight.timedOut = true
			hookProgressOwnerFlights.Unlock()
			return hookProgressOwnerSnapshot{}, fmt.Errorf("timed out after %s while loading hook progress owners under %s: %w", relocationIdentityTimeout, home, context.DeadlineExceeded)
		}
		hookProgressOwnerFlights.Unlock()
		<-flight.done
		return flight.snapshot, flight.err
	}
}
