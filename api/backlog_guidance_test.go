package api

import (
	"regexp"
	"slices"
	"strings"
	"testing"

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
	const controls = `item sessions cannot change the runner or budget; ask the user with "af backlog ask --self"`
	for _, args := range [][]string{{"runner", "on"}, {"budget", "--max-per-run", "5"}} {
		if _, err := runBacklogCmd(t, args...); err == nil || err.Error() != controls {
			t.Errorf("%v from an item session: %v", args, err)
		}
	}
}
