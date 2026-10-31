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

// fenceTable remembers, per writer and doc, the highest save sequence seen. A save with a
// sequence at or below it is refused. It is in memory: after a daemon restart a writer
// is unfenced until its next save, which is the same exposure as before the fence.
type fenceTable struct {
	mu    sync.Mutex
	seen  map[string]int64
	order []string
}

// admit records seq for key and reports whether the save may proceed, with the sequence
// that fenced it when not.
func (f *fenceTable) admit(key string, seq int64) (ok bool, last int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]int64{}
	}
	last, known := f.seen[key]
	if known && seq <= last {
		return false, last
	}
	if !known {
		f.order = append(f.order, key)
		if len(f.order) > fenceCap {
			delete(f.seen, f.order[0])
			f.order = f.order[1:]
		}
	}
	f.seen[key] = seq
	return true, 0
}
