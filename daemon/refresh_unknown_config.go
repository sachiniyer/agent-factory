package daemon

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// A row whose load failed because its project config could not be read
// (#7077) is not re-materialized on every refresh. refreshLocked runs under
// m.mu, and every caller of that lock — snapshot, list, create, send-prompt,
// task delivery — would pay the row's config read again on each tick for as
// long as the box stays loaded (#7077 review, P1-2). Instead the row is
// deferred: the refresh skips it without any I/O, and once its backoff
// expires a probe reads the project config OFF the lock, with a launch's retry
// budget, into a primed RestoreContext. The next refresh loads the row from
// that snapshot and reads no config under the lock at all.
//
// A deferred row still counts as a row that failed to load — the repo stays
// rows-failed, the startup orphan sweep keeps deferring, and a task run it
// holds stays counted — exactly as before; only the cost and the log volume
// change.

type deferredConfigRow struct {
	path     string
	title    string
	cause    error
	failures int
	next     time.Time
	probing  bool
	primed   *session.RestoreContext
	seen     uint64
}

var deferredConfigRows = struct {
	sync.Mutex
	rows map[string]*deferredConfigRow
	tick uint64
}{rows: map[string]*deferredConfigRow{}}

// Seams, read once per use.
var (
	fromInstanceDataWithContextForRefresh = session.FromInstanceDataWithContext
	primeRestoreContextForRefresh         = session.PrimeRestoreContext
	// startDeferredConfigProbe runs a probe off the refresh's lock; tests run it
	// inline to observe the hand-off deterministically.
	startDeferredConfigProbe = func(probe func()) { go probe() }
	deferredConfigBackoff    = func(failures int) time.Duration {
		backoff := 2 * time.Second
		for i := 1; i < failures && backoff < time.Minute; i++ {
			backoff *= 2
		}
		if backoff > time.Minute {
			backoff = time.Minute
		}
		return backoff
	}
)

// errDeferredConfigRow marks a refresh that skipped a deferred row without
// loading it. The chain also carries the row's last unknown-config error.
var errDeferredConfigRow = errors.New("row deferred until its project config can be read")

type deferredConfigRowError struct {
	title string
	next  time.Time
	cause error
}

func (e *deferredConfigRowError) Error() string {
	return fmt.Sprintf("session %q waits on its project config, which could not be read in time; a background re-read retries it (next at %s): %v",
		e.title, e.next.Format(time.RFC3339), e.cause)
}

func (e *deferredConfigRowError) Unwrap() []error { return []error{errDeferredConfigRow, e.cause} }

// beginDeferredConfigTick opens a refresh pass. Rows not seen by its end left
// the disk (archived, deleted) and are dropped from the registry.
func beginDeferredConfigTick() uint64 {
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	deferredConfigRows.tick++
	return deferredConfigRows.tick
}

func endDeferredConfigTick(tick uint64) {
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	for key, row := range deferredConfigRows.rows {
		if row.seen != tick {
			delete(deferredConfigRows.rows, key)
		}
	}
}

// materializeRefreshRow loads one on-disk row that has no in-memory instance,
// honoring the row's deferral: a deferred row is skipped without I/O until a
// probe primed its config off the lock, then loaded from that snapshot.
func materializeRefreshRow(tick uint64, key string, item session.InstanceData) (*session.Instance, error) {
	deferredConfigRows.Lock()
	row := deferredConfigRows.rows[key]
	var primed *session.RestoreContext
	if row != nil {
		row.seen = tick
		switch {
		case row.primed != nil:
			primed, row.primed = row.primed, nil
		case row.probing || time.Now().Before(row.next):
			err := &deferredConfigRowError{title: item.Title, next: row.next, cause: row.cause}
			deferredConfigRows.Unlock()
			return nil, err
		default:
			row.probing = true
			path := row.path
			deferredConfigRows.Unlock()
			startDeferredConfigProbe(func() { probeDeferredConfigRow(key, path) })
			deferredConfigRows.Lock()
			// An inline probe (tests) may already have primed the row.
			if row.primed != nil {
				primed, row.primed = row.primed, nil
			} else {
				err := &deferredConfigRowError{title: item.Title, next: row.next, cause: row.cause}
				deferredConfigRows.Unlock()
				return nil, err
			}
		}
	}
	deferredConfigRows.Unlock()

	var instance *session.Instance
	var err error
	if primed != nil {
		instance, err = fromInstanceDataWithContextForRefresh(item, primed)
	} else {
		instance, err = fromInstanceDataForRefresh(item)
	}
	noteRefreshRowOutcome(tick, key, item, err)
	return instance, err
}

// noteRefreshRowOutcome defers a row whose load reported its project config
// unknown and forgets one that loaded or failed for any other reason.
func noteRefreshRowOutcome(tick uint64, key string, item session.InstanceData, err error) {
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	row := deferredConfigRows.rows[key]
	if !session.IsProjectConfigUnknown(err) {
		if row != nil {
			delete(deferredConfigRows.rows, key)
			if err == nil {
				log.WarningLog.Printf("daemon: session %q loaded once its project config could be read", item.Title)
			}
		}
		return
	}
	if row == nil {
		row = &deferredConfigRow{path: item.Path, title: item.Title}
		deferredConfigRows.rows[key] = row
	}
	row.seen = tick
	row.cause = err
	row.failures++
	row.next = time.Now().Add(deferredConfigBackoff(row.failures))
	log.WarningLog.Printf("daemon: deferring session %q until its project config can be read; it is re-read off the refresh lock in %s: %v",
		item.Title, time.Until(row.next).Round(time.Second), err)
}

// probeDeferredConfigRow reads the row's project config with a launch's retry
// budget, holding no manager lock, and hands the primed snapshot to the next
// refresh. An unknown answer backs the row off again.
func probeDeferredConfigRow(key, path string) {
	primed, err := primeRestoreContextForRefresh(path)
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	row := deferredConfigRows.rows[key]
	if row == nil {
		return // the row left the disk while the probe ran
	}
	row.probing = false
	if session.IsProjectConfigUnknown(err) {
		row.cause = err
		row.failures++
		row.next = time.Now().Add(deferredConfigBackoff(row.failures))
		log.WarningLog.Printf("daemon: session %q still waits on its project config; next re-read in %s: %v",
			row.title, time.Until(row.next).Round(time.Second), err)
		return
	}
	row.primed = primed
}

// isDeferredConfigSkip reports a refresh that skipped a deferred row; the
// caller already logged its deferral and its every re-read.
func isDeferredConfigSkip(err error) bool { return errors.Is(err, errDeferredConfigRow) }
