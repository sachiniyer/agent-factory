package session

import (
	"errors"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
)

// recordThrowawayEngine notes, in a throwaway sandbox home (#6844), the Docker
// engine about to host one of its containers, before the container exists
// (#7447). When an expired sandbox's containers are reaped by a later marked
// daemon, possibly one whose Docker environment selects a different engine,
// this record is how that reap knows it looked where the containers are:
// a reap that listed an engine the home never recorded is incomplete, and the
// home stays registered for one that matches.
//
// It writes nothing outside a marked home: an unmarked home, an unreadable
// marker (which no reaper honours either) and the default install's home are
// all left untouched. A seam so tests can fail it.
var recordThrowawayEngine = func(home, engineID string) error {
	if home == "" {
		return nil
	}
	if _, err := throwawaysandbox.LoadMarker(home); err != nil {
		return nil
	}
	if def, err := config.ConfigDirFor(""); err == nil && filepath.Clean(def) == filepath.Clean(home) {
		return nil
	}
	return config.WithFileLock(throwawaysandbox.RecordedEnginesPath(home), func() error {
		return throwawaysandbox.WriteRecordedEngine(home, engineID)
	})
}

// errThrowawayEngineUnrecorded marks a provision refused because its engine
// could not be recorded: a container nobody knows the engine of is the leak
// the record closes, so none is created (#7447).
var errThrowawayEngineUnrecorded = errors.New("backend=docker: cannot record the Docker engine for this throwaway sandbox's cleanup; creating no container")

// ensureThrowawayEngineRecorded runs recordThrowawayEngine for this
// provision's home and engine, joining any failure to the refusal.
func (p *dockerProvisioner) ensureThrowawayEngineRecorded() error {
	if err := recordThrowawayEngine(p.homeID, p.engineID); err != nil {
		return errors.Join(errThrowawayEngineUnrecorded, err)
	}
	return nil
}
