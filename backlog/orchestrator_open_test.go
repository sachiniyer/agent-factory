package backlog

import (
	"strings"
	"testing"
)

func TestOrchestratorToOpen(t *testing.T) {
	b := boundGroup(t)
	if o, err := b.OrchestratorToOpen(1); err != nil || o == nil || o.SessionID != "o" {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := b.OrchestratorToOpen(2); err == nil || !strings.Contains(err.Error(), "not a top-level parent") {
		t.Fatalf("child: %v", err)
	}
	if _, err := b.OrchestratorToOpen(4); err == nil || !strings.Contains(err.Error(), "not a top-level parent") {
		t.Fatalf("leaf: %v", err)
	}
	if _, err := b.OrchestratorToOpen(9); err == nil {
		t.Fatal("missing item")
	}
	if _, err := b.ClearOrchestrator(1, t0); err != nil {
		t.Fatal(err)
	}
	if o, err := b.OrchestratorToOpen(1); err != nil || o != nil {
		t.Fatalf("unbound: %+v %v", o, err)
	}
	b.Item(1).Archived = true
	if _, err := b.OrchestratorToOpen(1); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("archived: %v", err)
	}
}

func TestBindNewOrchestratorRefusesABoundGroup(t *testing.T) {
	b := boundGroup(t)
	if err := b.BindNewOrchestrator(1, orch("p"), t0); err == nil || !strings.Contains(err.Error(), `"o" started orchestrating B-1`) {
		t.Fatalf("%v", err)
	}
	if _, err := b.ClearOrchestrator(1, t0); err != nil {
		t.Fatal(err)
	}
	if err := b.BindNewOrchestrator(1, orch("p"), t0); err != nil || b.Item(1).Orchestrator.SessionID != "p" {
		t.Fatalf("%v %+v", err, b.Item(1).Orchestrator)
	}
}
