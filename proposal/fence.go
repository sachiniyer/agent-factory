package proposal

import (
	"fmt"
	"net/http"
	"sync"
)

// ErrorCodeStaleSave is the envelope error code of a save the writer has already moved past.
const ErrorCodeStaleSave = "proposal_stale_save"

// fenceCap bounds the writers the daemon remembers. The oldest are forgotten first; a writer
// that is forgotten is simply no longer fenced.
const fenceCap = 4096

// StaleSaveError means a save arrived after a later save of the same writer: the request
// outlived the client that sent it (a timeout, a closed tab, a reordered reply) and the
// writer has moved on, so applying it would put back text the writer has since changed.
// It commits nothing.
type StaleSaveError struct {
	Writer string
	Seq    int64
	Last   int64
}

func (e *StaleSaveError) Error() string {
	return fmt.Sprintf("this save (writer %s, seq %d) is older than a save already seen from the same writer (seq %d), so it was not applied", e.Writer, e.Seq, e.Last)
}

// APIErrorCode and HTTPStatus shape the daemon's HTTP reply.
func (e *StaleSaveError) APIErrorCode() string { return ErrorCodeStaleSave }
func (e *StaleSaveError) HTTPStatus() int      { return http.StatusConflict }

// fenceTable remembers, per writer and doc, the highest save sequence that has committed.
// A save with a sequence at or below it is refused. It is in memory: after a daemon restart
// a writer is unfenced until its next save, which is the same exposure as before the fence.
//
// The table is only read and written from inside Store's lock (see SaveGuard), so a check, the
// save it admits and the update that follows are one critical section.
type fenceTable struct {
	mu    sync.Mutex
	seen  map[string]int64
	order []string
}

// check reports whether a save with seq may proceed, and the high-water mark when not.
func (f *fenceTable) check(key string, seq int64) (ok bool, last int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	last, known := f.seen[key]
	return !known || seq > last, last
}

// advance raises the high-water mark of key to seq.
func (f *fenceTable) advance(key string, seq int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]int64{}
	}
	if last, known := f.seen[key]; known {
		if seq > last {
			f.seen[key] = seq
		}
		return
	}
	f.order = append(f.order, key)
	if len(f.order) > fenceCap {
		delete(f.seen, f.order[0])
		f.order = f.order[1:]
	}
	f.seen[key] = seq
}

// SaveGuard lets a caller hold a save to a condition that must hold at the moment the store
// commits it. Admit runs first inside the store's lock, before anything is written, and a
// non-nil error refuses the save. Committed runs inside the same lock once the save has
// succeeded, and only then.
type SaveGuard interface {
	Admit() error
	Committed()
}

// writerGuard is the fence of one writer's save.
type writerGuard struct {
	table  *fenceTable
	key    string
	writer string
	seq    int64
}

func (g *writerGuard) Admit() error {
	if ok, last := g.table.check(g.key, g.seq); !ok {
		return &StaleSaveError{Writer: g.writer, Seq: g.seq, Last: last}
	}
	return nil
}

func (g *writerGuard) Committed() { g.table.advance(g.key, g.seq) }
