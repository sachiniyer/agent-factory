package tmux

import (
	"os/exec"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/stretchr/testify/assert"
)

// TestHasUpdated_NilMonitor pins the #999 fix at its source. A TmuxSession that
// never had Start/Restore called (the shape of a persisted Dead instance whose
// LocalBackend.Start returns before Restore initializes the monitor, #970) has a
// nil monitor field. HasUpdated must treat that as nothing-to-report and return
// (false,false) rather than panic on the nil deref, which would kill the
// daemon's refresh goroutine and zombify the daemon.
func TestHasUpdated_NilMonitor(t *testing.T) {
	exec := cmd_test.MockCmdExec{
		// Should never be reached: the nil-monitor guard returns before any
		// capture-pane. Fail loudly if the guard regresses and we fall through.
		RunFunc:    func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}
	ts := NewTmuxSessionFromSanitizedNameWithDeps("af_nil_monitor", "claude", MakePtyFactory(), exec)

	updated, hasPrompt, _ := ts.HasUpdated()
	assert.False(t, updated, "a session with no live monitor has nothing to report")
	assert.False(t, hasPrompt)
}
