package tmux

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGroupMatesFromListing(t *testing.T) {
	listing := "login\tlogin\nlogin\taf-takeover-1\nlogin\taf-takeover-2\nother\tother\nother\tother-mate\n\tsolo\n"
	for name, want := range map[string][]string{
		"login":          {"af-takeover-1", "af-takeover-2"},
		"af-takeover-2":  {"login", "af-takeover-1"},
		"other":          {"other-mate"},
		"solo":           nil, // ungrouped: no mates, even though other rows have an empty group
		"missing":        nil,
		"af-takeover-99": nil,
	} {
		if got := groupMatesFromListing(listing, name); !reflect.DeepEqual(got, want) {
			t.Errorf("mates of %q = %v, want %v", name, got, want)
		}
	}
	// Two ungrouped sessions are not mates of each other.
	if got := groupMatesFromListing("\ta\n\tb\n", "a"); got != nil {
		t.Errorf("ungrouped sessions must not be mates: %v", got)
	}
}

// #6078: closing a session kills the sessions grouped with it BEFORE the source,
// synchronously, so no window (and no process) survives behind a takeover's
// private session. The fake tmux records every kill-session target in order.
func TestCloseKillsGroupMatesBeforeTheSource(t *testing.T) {
	log := filepath.Join(t.TempDir(), "kills")
	scriptedTmuxOnPath(t, `
display-message) echo "0" ;;
list-panes)      exit 0 ;;
list-sessions)   printf 'g\tafsrc\ng\taf-takeover-aa\ng\taf-takeover-bb\nz\tunrelated\n' ;;
kill-session)    echo "$3" >> `+log+`; exit 0 ;;
has-session)     exit 1 ;;`)

	ts := NewTmuxSessionFromSanitizedName("afsrc", "")
	if _, err := ts.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(b))
	want := []string{"=af-takeover-aa:", "=af-takeover-bb:", "=afsrc:"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kill-session targets = %v, want mates first then the source: %v", got, want)
	}
}

// #6078 CI: a wedged server must not cost Close an extra deadline for the mate
// listing. The capture's list-panes trips the deadline; the listing is skipped, so
// Close spends the same two deadlines it did before grouped-session teardown.
func TestCloseOnWedgedServerSkipsTheGroupMateListing(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$1\" >> " + log + "\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shortTmuxTimeout(t, 200*time.Millisecond)

	ts := NewTmuxSessionFromSanitizedName("afwedged", "")
	_, _ = ts.Close()

	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "list-sessions") {
		t.Fatalf("Close listed group mates against a wedged server (calls: %q)", strings.Fields(string(b)))
	}
}
