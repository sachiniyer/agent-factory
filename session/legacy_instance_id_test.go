package session

import "testing"

func TestFromInstanceData_BackfillsLegacyIDInMemory(t *testing.T) {
	inst, err := FromInstanceData(InstanceData{
		Title: "legacy", BackendType: "docker", Status: Archived, Liveness: LiveArchived,
	})
	if err != nil {
		t.Fatalf("FromInstanceData: %v", err)
	}
	if inst.ID == "" {
		t.Fatal("FromInstanceData preserved an empty legacy ID")
	}
}
