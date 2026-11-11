package daemon

import (
	"errors"
	"fmt"
	"math/rand/v2"
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
	repoID   string
	path     string
	title    string
	cause    error
	failures int
	next     time.Time
	// probing marks a row waiting on the in-flight probe of probePath — the
	// path it was due under, kept apart from path so a record that moved while
	// the probe ran still collects that probe's answer (#7080 review).
	probing   bool
	probePath string
	primed    *session.RestoreContext
	// primedTick is the refresh pass a probe primed the row in: the snapshot
	// stays good for deferredPrimedMaxPasses passes, counted in passes rather
	// than wall time so a long daemon_poll_interval never ages it (#7087 review).
	primedTick uint64
	seen       uint64
	// seenAt is when a pass last listed the row. A repo that stops being
	// listed keeps its rows only for deferredRowForgetAfter (#7083 review).
	seenAt time.Time
}

var deferredConfigRows = struct {
	sync.Mutex
	rows map[string]*deferredConfigRow
	// probes is the set of project paths with a probe in flight. Rows of one
	// project share its probe and its primed snapshot, and at most
	// maxDeferredConfigProbes projects are probed at once (#7080 review).
	probes map[string]bool
	tick   uint64
}{rows: map[string]*deferredConfigRow{}, probes: map[string]bool{}}

// Seams, read once per use.
var (
	fromInstanceDataWithContextForRefresh = session.FromInstanceDataWithContext
	primeRestoreContextForRefresh         = session.PrimeRestoreContext
	// startDeferredConfigProbe runs a probe off the refresh's lock. It must
	// never run the probe inline: the refresh holds m.mu, and the probe is the
	// launch-budget config read the deferral exists to keep out from under it.
	startDeferredConfigProbe = func(probe func()) { go probe() }
	// deferredConfigNow is the deferral's clock — backoffs, sightings — so a
	// test can let a whole poll interval go by between two passes.
	deferredConfigNow     = time.Now
	deferredConfigBackoff = func(failures int) time.Duration {
		backoff := 2 * time.Second
		for i := 1; i < failures && backoff < time.Minute; i++ {
			backoff *= 2
		}
		if backoff > time.Minute {
			backoff = time.Minute
		}
		return backoff
	}
	// deferredConfigJitter spreads re-reads that came due together so a load
	// window does not synchronize every deferred project onto one tick.
	deferredConfigJitter = func(backoff time.Duration) time.Duration {
		if backoff <= 0 {
			return 0
		}
		return time.Duration(rand.Int64N(int64(backoff/4) + 1))
	}
	maxDeferredConfigProbes = 4
	// deferredPrimedMaxPasses bounds how many refresh passes may go by between
	// a probe priming a row and the row loading from it: an older snapshot is
	// re-probed, so a row whose repo was unlisted for a while never loads from a
	// config read long ago (#7083 review). Passes, not seconds: the refresh runs
	// at daemon_poll_interval, which accepts any positive duration, and a
	// wall-clock bound shorter than it would stale every snapshot before its
	// next pass and re-probe the row forever (#7087 review).
	deferredPrimedMaxPasses = uint64(2)
	// deferredRowForgetAfter drops a deferred row no pass has listed for this
	// long — its repo unreadable, missing or gone — so the registry cannot
	// grow without bound (#7083 review). It reloads normally if it reappears.
	deferredRowForgetAfter = 10 * time.Minute
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

// beginDeferredConfigTick opens a refresh pass.
func beginDeferredConfigTick() uint64 {
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	deferredConfigRows.tick++
	return deferredConfigRows.tick
}

// endDeferredConfigTick drops the deferred rows that left the disk: rows of a
// repo this pass READ but no longer listed. A repo the pass did not read — its
// instances.json unreadable or missing this tick — keeps its rows and their
// backoff, or one transient read failure would reset every one of them (#7080
// review) — but only for deferredRowForgetAfter: a repo that stays unlisted
// drops its rows instead of keeping them forever (#7083 review).
func endDeferredConfigTick(tick uint64, readRepos map[string]bool) {
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	now := deferredConfigNow()
	for key, row := range deferredConfigRows.rows {
		if row.seen == tick {
			continue
		}
		if readRepos[row.repoID] || now.Sub(row.seenAt) > deferredRowForgetAfter {
			delete(deferredConfigRows.rows, key)
		}
	}
}

// materializeRefreshRow loads one on-disk row that has no in-memory instance,
// honoring the row's deferral: a deferred row is skipped without I/O until a
// probe primed its project's config off the lock, then loaded from that
// snapshot.
func materializeRefreshRow(tick uint64, repoID, key string, item session.InstanceData) (*session.Instance, error) {
	deferredConfigRows.Lock()
	row := deferredConfigRows.rows[key]
	var primed *session.RestoreContext
	if row != nil {
		row.seen, row.seenAt = tick, deferredConfigNow()
		row.path, row.title = item.Path, item.Title // the record may have moved
		if row.primed != nil && tick-row.primedTick > deferredPrimedMaxPasses {
			// Read too long ago to stand for the config now: probe again.
			row.primed, row.next = nil, time.Time{}
		}
		if row.primed == nil && !row.probing && !deferredConfigNow().Before(row.next) {
			switch {
			case deferredConfigRows.probes[row.path]:
				// Another row of this project is already being probed: share it.
				row.probing, row.probePath = true, row.path
			case len(deferredConfigRows.probes) < maxDeferredConfigProbes:
				row.probing, row.probePath = true, row.path
				deferredConfigRows.probes[row.path] = true
				path := row.path
				deferredConfigRows.Unlock()
				startDeferredConfigProbe(func() { probeDeferredConfigPath(path) })
				deferredConfigRows.Lock()
			}
			// At the cap the row stays due and asks again on the next tick.
		}
		if row.primed == nil {
			err := &deferredConfigRowError{title: item.Title, next: row.next, cause: row.cause}
			deferredConfigRows.Unlock()
			return nil, err
		}
		primed, row.primed = row.primed, nil
	}
	deferredConfigRows.Unlock()

	var instance *session.Instance
	var err error
	if primed != nil {
		instance, err = fromInstanceDataWithContextForRefresh(item, primed)
	} else {
		instance, err = fromInstanceDataForRefresh(item)
	}
	noteRefreshRowOutcome(tick, repoID, key, item, err)
	return instance, err
}

// backOffLocked books the row's next re-read after an unknown answer.
func (row *deferredConfigRow) backOffLocked(cause error) {
	row.cause = cause
	row.failures++
	backoff := deferredConfigBackoff(row.failures)
	row.next = deferredConfigNow().Add(backoff + deferredConfigJitter(backoff))
}

// noteRefreshRowOutcome defers a row whose load reported its project config
// unknown and forgets one that loaded or failed for any other reason.
func noteRefreshRowOutcome(tick uint64, repoID, key string, item session.InstanceData, err error) {
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
		row = &deferredConfigRow{}
		deferredConfigRows.rows[key] = row
	}
	row.repoID, row.path, row.title, row.seen, row.seenAt = repoID, item.Path, item.Title, tick, deferredConfigNow()
	row.backOffLocked(err)
	log.WarningLog.Printf("daemon: deferring session %q until its project config can be read; it is re-read off the refresh lock in %s: %v",
		item.Title, row.next.Sub(deferredConfigNow()).Round(time.Second), err)
}

// probeDeferredConfigPath reads one project's config with a launch's retry
// budget, holding no manager lock, and hands the primed snapshot to every row
// of that project waiting on it. An unknown answer backs those rows off again.
func probeDeferredConfigPath(path string) {
	primed, err := primeRestoreContextForRefresh(path)
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	delete(deferredConfigRows.probes, path)
	unknown := session.IsProjectConfigUnknown(err)
	waiting := 0
	for _, row := range deferredConfigRows.rows {
		if !row.probing || row.probePath != path {
			continue
		}
		row.probing, row.probePath = false, ""
		waiting++
		if unknown {
			row.backOffLocked(err)
			continue
		}
		row.primed, row.primedTick = primed, deferredConfigRows.tick
	}
	if unknown && waiting > 0 {
		log.WarningLog.Printf("daemon: the project config for %q still could not be read; %d deferred session(s) wait on it: %v", path, waiting, err)
	}
}

// isDeferredConfigSkip reports a refresh that skipped a deferred row; the
// caller already logged its deferral and its every re-read.
func isDeferredConfigSkip(err error) bool { return errors.Is(err, errDeferredConfigRow) }
