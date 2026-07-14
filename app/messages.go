package app

import (
	"github.com/sachiniyer/agent-factory/session"
)

type hideErrMsg struct {
	noticeID uint64
}
type previewTickMsg struct{}
type instanceChangedMsg struct{}

// startKillMsg is emitted by the kill confirmation action right after the
// target row has been marked Deleting on the event loop. Its handler
// dispatches killInstanceCmd, which runs the slow teardown in a background
// goroutine (#844).
type startKillMsg struct {
	title string
}

// instanceKilledMsg reports completion of an async kill. A nil err means the
// daemon tore the session down and deleted its record; a non-nil err means
// the session is still alive and the row must become retryable again.
type instanceKilledMsg struct {
	title string
	err   error
}

// startArchiveMsg is emitted by the archive confirmation (#1028); its handler
// dispatches archiveInstanceCmd to run the daemon teardown+move off the event
// loop, mirroring startKillMsg → killInstanceCmd.
type startArchiveMsg struct {
	title string
}

// startDeleteProjectMsg is emitted by the delete-project confirmation (#1735);
// its handler dispatches deleteProjectCmd to run the daemon archive-then-remove
// off the event loop, mirroring startArchiveMsg → archiveInstanceCmd.
type startDeleteProjectMsg struct {
	root   string
	repoID string
	name   string
}

// projectDeletedMsg reports completion of an async delete-project (#1735). On
// success the archived rows leave the active list via the next daemon Snapshot
// reconcile; a non-nil err is surfaced in the error box.
type projectDeletedMsg struct {
	root     string
	repoID   string
	name     string
	archived int
	err      error
}

// instanceArchivedMsg / instanceRestoredMsg report completion of an async
// archive / restore (#1028). On success the row's new status arrives via the
// next daemon Snapshot reconcile (which re-partitions it into / out of the
// Archived folder); a non-nil err is surfaced in the error box.
type instanceArchivedMsg struct {
	title string
	err   error
}

type instanceRestoredMsg struct {
	title string
	err   error
}

// runOnEventLoopMsg is a test-only primitive: when received by Update, it
// runs fn with the home pointer on the tea goroutine, then closes done.
// Production code never emits these — it exists purely so e2e tests can
// read home state without racing concurrent Update handlers.
type runOnEventLoopMsg struct {
	fn   func(*home)
	done chan struct{}
}

type instanceStartedMsg struct {
	instance *session.Instance
	started  *session.Instance
	err      error
}
