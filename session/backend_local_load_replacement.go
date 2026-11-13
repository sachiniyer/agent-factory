package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// loadReplacement is a load's replacement for the conversation it refused to
// resume. It becomes the record before its pane can write, as on Recover
// (#7209 review R6), and durable before then too, through the loader's persist
// hook (#7209 review R2-1). Both happen on tmux's re-spawn branch only: a
// reattached live pane keeps its record, and a write made before tmux knew
// which branch it takes would name the replacement for a pane that never ran
// it. A re-spawn that does not commit puts the refused id back, on disk too.
type loadReplacement struct {
	i           *Instance
	refusedID   string
	replacement AgentConversationData
	persist     func(*Instance) error
	undo        func()
	staged      bool
	persisted   bool
}

// armLoadReplacement installs the staging on ts's re-spawn branch. The caller
// clears the step once the restore returns and settles the outcome.
func (i *Instance) armLoadReplacement(ts *tmux.TmuxSession, refusedID string, replacement AgentConversationData) *loadReplacement {
	r := &loadReplacement{
		i: i, refusedID: refusedID, replacement: replacement, undo: func() {},
		persist: i.restoreCtx.Load().persistBeforeRespawnFn(),
	}
	ts.SetBeforeRespawn(r.stage)
	return r
}

// stage records the replacement and makes it durable. A write that fails puts
// the record back and refuses the re-spawn, deferring the row to Recover.
func (r *loadReplacement) stage() error {
	undo, staged := r.i.stageReplacementConversation(r.refusedID, r.replacement)
	if !staged {
		return nil
	}
	r.undo, r.staged = undo, true
	if r.persist == nil {
		// A load with no hook says so: the replacement is in memory only until
		// the load's own persist.
		log.InfoLog.Printf("session %q: the replacement for its refused conversation %s is recorded in memory only until the load persists it, because this load has no persist hook",
			r.i.Title, r.refusedID)
		return nil
	}
	if err := r.persist(r.i); err != nil {
		undo()
		r.undo, r.staged = func() {}, false
		return fmt.Errorf("%w: session %q, refused conversation %s: %w", ErrReplacementNotDurable, r.i.Title, r.refusedID, err)
	}
	r.persisted = true
	return nil
}

// settle reports whether the staged replacement stands: it does only when the
// re-spawn committed. Otherwise the refused id goes back, and a record already
// written is written again. A nil receiver staged nothing.
func (r *loadReplacement) settle(respawned bool) bool {
	if r == nil || !r.staged {
		return false
	}
	if respawned {
		return true
	}
	r.undo()
	if r.persisted {
		if err := r.persist(r.i); err != nil {
			log.WarningLog.Printf("session %q: could not persist its restored conversation record after a re-spawn that did not commit: %v", r.i.Title, err)
		}
	}
	return false
}
