package api

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/sachiniyer/agent-factory/session"
)

// The guidance an item session's agent reads names what af refuses it
// (session.AfBacklogItemSessionRefusals). This derives that list from the
// commands themselves — every af backlog subcommand, run from an item session —
// so a verb that gains or loses the guard fails here until the sentence says so.
func TestItemSessionGuidanceNamesEveryRefusal(t *testing.T) {
	stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "s1", Title: "B-1 work", BacklogItem: 1})

	refusal := regexp.MustCompile(`item sessions cannot run "af backlog ([^"]+)"`)
	const controls = `item sessions cannot change the runner or budget; ask the user with "af backlog ask --self"`
	got := map[string]bool{}
	for _, c := range BacklogCmd.Commands() {
		name := c.Name()
		if name == "help" || name == "completion" {
			continue
		}
		// Two arg shapes reach the guard of every verb: "done <id>" checks it has
		// exactly one argument first, "note <id>" that it has text.
		for _, args := range [][]string{{name, "1"}, {name, "1", "text"}} {
			if _, err := runBacklogCmd(t, args...); err != nil {
				if m := refusal.FindStringSubmatch(err.Error()); m != nil {
					got[m[1]] = true
				} else if strings.HasPrefix(err.Error(), "item sessions cannot") && err.Error() != controls {
					// A refusal in its own words would escape the list below: say it
					// the way refuseFromItemSession does, or teach this test the shape.
					t.Errorf("af backlog %v refuses an item session in a shape this test cannot read: %v", args, err)
				}
			}
		}
	}
	var derived []string
	for verb := range got {
		derived = append(derived, verb)
	}
	slices.Sort(derived)

	const lead, tail = "which are the user's: ", ", and changing the runner or budget."
	text := session.AfBacklogItemSessionRefusals
	start, end := strings.Index(text, lead), strings.Index(text, tail)
	if start < 0 || end < start {
		t.Fatalf("the refusal sentence lost its shape; update this parser with it:\n%s", text)
	}
	taught := strings.Split(text[start+len(lead):end], ", ")
	slices.Sort(taught)
	if !slices.Equal(derived, taught) {
		t.Fatalf("an item session is refused %v, but its guidance names %v", derived, taught)
	}

	// The sentence's last clause: the runner and the budget refuse changes.
	for _, args := range [][]string{{"runner", "on"}, {"budget", "--max-per-run", "5"}} {
		if _, err := runBacklogCmd(t, args...); err == nil || err.Error() != controls {
			t.Errorf("%v from an item session: %v", args, err)
		}
	}
}

// An item session may add the work it found as a draft (decision (3) on
// #6284), in one form: a title and the flags the guidance names
// (session.AfBacklogItemSessionAddForm). This tries every flag of add from an
// item session, derives the ones it allows, and requires the sentence to name
// exactly those; every other flag, --file included, is refused in the shape
// refuseFromItemSession uses.
func TestItemSessionGuidanceNamesTheAddForm(t *testing.T) {
	stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "s1", Title: "B-1 work", BacklogItem: 1})

	add, _, err := BacklogCmd.Find([]string{"add"})
	if err != nil {
		t.Fatal(err)
	}
	var allowed []string
	add.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		_, err := runBacklogCmd(t, "add", "found work", "--"+f.Name+"=1")
		want := `item sessions cannot run "af backlog add --` + f.Name + `"`
		switch {
		case err != nil && strings.HasPrefix(err.Error(), want):
		case err != nil && strings.HasPrefix(err.Error(), "item sessions cannot"):
			t.Errorf("add --%s refuses an item session in a shape this test cannot read: %v", f.Name, err)
		default:
			allowed = append(allowed, "--"+f.Name)
		}
	})
	if _, err := runBacklogCmd(t, "add", "found work"); err != nil {
		t.Fatalf("a bare add from an item session: %v", err)
	}

	form := regexp.MustCompile(`add takes only a title and (--[a-z-]+(?:, --[a-z-]+)*);`).FindStringSubmatch(session.AfBacklogItemSessionAddForm)
	if form == nil {
		t.Fatalf("the add-form sentence lost its shape; update this parser with it:\n%s", session.AfBacklogItemSessionAddForm)
	}
	taught := strings.Split(form[1], ", ")
	slices.Sort(allowed)
	slices.Sort(taught)
	if !slices.Equal(allowed, taught) {
		t.Fatalf("an item session's add allows %v, but its guidance names %v", allowed, taught)
	}
	if strings.Contains(session.AfBacklogItemSessionRefusals, " add,") {
		t.Fatal("the refusal sentence still names add")
	}
}
