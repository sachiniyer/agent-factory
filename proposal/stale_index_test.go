package proposal

import (
	"strings"
	"testing"
)

func TestStaleIndexFromACrashIsNotCommittedAsExternal(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	editOnDisk(t, s, "p", "goals.md", "agent work\n")
	if err := s.withLock(func() error { return s.stage([]string{"p"}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Checkpoint(sessA); err != nil {
		t.Fatal(err)
	}
	body, err := s.git(nil, "log", "-1", "--format=%B", "--", "p/goals.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Af-Author: agent") {
		t.Fatalf("the agent's edit, left staged by a crash, was committed as:\n%s", body)
	}
}
