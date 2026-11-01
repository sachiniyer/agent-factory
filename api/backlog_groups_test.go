package api

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

func TestBacklogGroups(t *testing.T) {
	stubBacklogDaemon(t)
	for _, args := range [][]string{{"add", "group"}, {"add", "child", "--parent", "1"}, {"add", "other"}, {"queue", "1"}} {
		if _, err := runBacklogCmd(t, args...); err != nil {
			t.Fatal(args, err)
		}
	}
	out, err := runBacklogCmd(t, "pause", "B-1")
	if err != nil {
		t.Fatal(err)
	}
	if it := decodeOut[backlog.Item](t, out); !it.Paused {
		t.Fatalf("pause: %+v", it)
	}
	out, _ = runBacklogCmd(t, "list")
	if !strings.Contains(out, "B-1  group  · Queued · 0 of 1 done · paused\n") ||
		!strings.Contains(out, "  B-2  child  · Queued · paused (B-1)\n") {
		t.Fatalf("paused list:\n%s", out)
	}
	if _, err := runBacklogCmd(t, "pause", "2"); err == nil || err.Error() != "B-2 is not a parent; only parents can be paused" {
		t.Fatal(err)
	}

	if _, err := runBacklogCmd(t, "archive", "1"); err != nil {
		t.Fatal(err)
	}
	if out, _ := runBacklogCmd(t, "list"); out != "B-3  other  · Draft\n" {
		t.Fatalf("archived group listed:\n%s", out)
	}
	if out, _ := runBacklogCmd(t, "list", "--archived"); !strings.Contains(out, "B-1  group") || !strings.Contains(out, "· archived") || !strings.Contains(out, "B-2  child") {
		t.Fatalf("--archived:\n%s", out)
	}
	if _, err := runBacklogCmd(t, "unpause", "1"); err == nil || !strings.Contains(err.Error(), "unarchive it first") {
		t.Fatal(err)
	}
	out, err = runBacklogCmd(t, "unarchive", "1")
	if err != nil {
		t.Fatal(err)
	}
	if it := decodeOut[backlog.Item](t, out); it.Archived || !it.Paused {
		t.Fatalf("unarchive: %+v", it)
	}
	out, err = runBacklogCmd(t, "unpause", "1")
	if err != nil {
		t.Fatal(err)
	}
	if it := decodeOut[backlog.Item](t, out); it.Paused {
		t.Fatalf("unpause: %+v", it)
	}
}

func TestBacklogListPausedMarkerOnlyOnParents(t *testing.T) {
	stubBacklogDaemon(t)
	if _, err := runBacklogCmd(t, "add", "leaf"); err != nil {
		t.Fatal(err)
	}
	p, err := backlogProject()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backlog.Update(p.repoID, func(b *backlog.Backlog) error {
		b.Item(1).Paused = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out, _ := runBacklogCmd(t, "list"); out != "B-1  leaf  · Draft\n" {
		t.Fatalf("list:\n%s", out)
	}
}

func TestBacklogGroupsRefusedFromItemSession(t *testing.T) {
	methods := stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "s1", Title: "item", BacklogItem: 1})
	for _, cmd := range []string{"pause", "unpause", "archive", "unarchive"} {
		*methods = nil
		_, err := runBacklogCmd(t, cmd, "1")
		if err == nil || err.Error() != `item sessions cannot run "af backlog `+cmd+`"; ask the user with "af backlog ask --self"` {
			t.Fatalf("%s: %v", cmd, err)
		}
		if len(*methods) != 0 {
			t.Fatalf("%s called %v", cmd, *methods)
		}
	}
}
