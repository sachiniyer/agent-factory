package daemon

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// idPlaneServer is a TabAddressableServer that records the ids it is addressed
// by. Only the id plane is driven; the embedded interface is never called.
type idPlaneServer struct {
	session.AgentServer
	inputs []string
}

func (s *idPlaneServer) SubscribeTab(id string, _ session.Seq) (session.PTYSubscription, error) {
	return nil, nil
}
func (s *idPlaneServer) InputTab(id string, _ []byte) error {
	s.inputs = append(s.inputs, id)
	return nil
}
func (s *idPlaneServer) ResizeTab(string, uint16, uint16) error { return nil }

// TestBindTab_LegacyOrdinalOnOffBoxSessionResolvesToStableID pins #5952: a
// ?tab=<idx> against an off-box session names a slot in the DAEMON's roster,
// which a reorder permutes without touching the sandbox's. The ordinal must be
// resolved against that roster at bind time and the connection must hold the
// resulting stable id — so input reaches the command the slot named when the
// connection opened, however the tabs are reordered afterwards.
func TestBindTab_LegacyOrdinalOnOffBoxSessionResolvesToStableID(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{Title: "sb", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(&session.HookBackend{})
	inst.AddTabWithIDForTest("agent-id", "agent", session.TabKindAgent)
	inst.AddTabWithIDForTest("a-id", "a", session.TabKindProcess)
	inst.AddTabWithIDForTest("b-id", "b", session.TabKindProcess)

	as := &idPlaneServer{}
	binding, err := bindTab(as, inst, "", 2)
	if err != nil {
		t.Fatalf("bindTab: %v", err)
	}
	idb, ok := binding.(idTabBinding)
	if !ok {
		t.Fatalf("an off-box legacy ordinal must bind by id, got %T", binding)
	}
	if idb.tabID != "b-id" {
		t.Fatalf("ordinal 2 resolved to %q, want b-id", idb.tabID)
	}

	// Reorder the daemon roster: b moves to slot 1. The open connection keeps b.
	if err := inst.ReorderTabByID("b-id", 1); err != nil {
		t.Fatalf("ReorderTabByID: %v", err)
	}
	if err := binding.input([]byte("x")); err != nil {
		t.Fatalf("input: %v", err)
	}
	if len(as.inputs) != 1 || as.inputs[0] != "b-id" {
		t.Fatalf("input reached %v after a reorder, want [b-id]", as.inputs)
	}
	// A fresh connection to slot 1 now resolves to the tab sitting there.
	fresh, err := bindTab(as, inst, "", 1)
	if err != nil {
		t.Fatalf("bindTab after reorder: %v", err)
	}
	if got := fresh.(idTabBinding).tabID; got != "b-id" {
		t.Fatalf("slot 1 after the reorder resolved to %q, want b-id", got)
	}

	// A slot the roster does not have is a definitive tab_gone, not a forward.
	if _, err := bindTab(as, inst, "", 9); !errors.Is(err, session.ErrTabGone) {
		t.Fatalf("out-of-range ordinal = %v, want ErrTabGone", err)
	}
}
