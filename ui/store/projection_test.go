package store

import (
	"bytes"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

func captureProjectionErrorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := aflog.ErrorLog.Writer()
	aflog.ErrorLog.SetOutput(&buf)
	t.Cleanup(func() { aflog.ErrorLog.SetOutput(prev) })
	return &buf
}

func newUnstartedProjectionInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	if err != nil {
		t.Fatalf("NewInstance() error = %v", err)
	}
	if inst.Started() {
		t.Fatalf("test fixture must be unstarted")
	}
	return inst
}

func TestProjectionRemoveUnstartedInstanceDoesNotLogError(t *testing.T) {
	t.Run("KillInstance", func(t *testing.T) {
		errLog := captureProjectionErrorLog(t)
		p := NewProjection()
		inst := newUnstartedProjectionInstance(t, "unstarted-kill")
		p.AddInstance(inst)

		if err := p.KillInstance(inst); err != nil {
			t.Fatalf("KillInstance() error = %v", err)
		}
		if p.ContainsInstance(inst) {
			t.Fatalf("KillInstance() left the instance in the projection")
		}
		if got := errLog.String(); got != "" {
			t.Fatalf("ErrorLog = %q, want empty", got)
		}
	})

	t.Run("RemoveInstanceByTitle", func(t *testing.T) {
		errLog := captureProjectionErrorLog(t)
		p := NewProjection()
		inst := newUnstartedProjectionInstance(t, "unstarted-remove")
		p.AddInstance(inst)

		if !p.RemoveInstanceByTitle(inst.Title) {
			t.Fatalf("RemoveInstanceByTitle() did not remove the instance")
		}
		if p.ContainsInstance(inst) {
			t.Fatalf("RemoveInstanceByTitle() left the instance in the projection")
		}
		if got := errLog.String(); got != "" {
			t.Fatalf("ErrorLog = %q, want empty", got)
		}
	})
}
