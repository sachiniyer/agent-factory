package api

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// unreadableReasonOnTheWire is the Reason a daemon sends for a repo whose
// instances.json it could not read (#4783). Spelled as the literal wire value
// rather than the daemon constant, because it is a contract with daemons of
// other versions, not an internal name.
const unreadableReasonOnTheWire = "unreadable-instances-json"

const unreadableTestRepo = "unreadable-repo"

func unreadableSkip(repoID string) daemon.SkippedRepo {
	return daemon.SkippedRepo{RepoID: repoID, Reason: unreadableReasonOnTheWire}
}

// assertReadsAsUnreadable checks that a refusal about an unreadable repo says
// so, and does not tell the operator to fix JSON that may be perfectly good.
func assertReadsAsUnreadable(t *testing.T, msg string) {
	t.Helper()
	if !strings.Contains(msg, unreadableTestRepo) {
		t.Fatalf("must name the unreadable repo's file, got: %s", msg)
	}
	if !strings.Contains(msg, "could not read") {
		t.Fatalf("must say the file could not be read, got: %s", msg)
	}
	if strings.Contains(msg, "corrupt") || strings.Contains(msg, "fix the JSON") {
		t.Fatalf("an unreadable file is not a corrupt one; must not call it corrupted or say to fix the JSON, got: %s", msg)
	}
	if !strings.Contains(msg, "readable by the user the af daemon runs as") || !strings.Contains(msg, "do not delete them") {
		t.Fatalf("must carry the unreadable remedy, got: %s", msg)
	}
}

// TestListSessions_DaemonUpUnreadableSkipSaysUnreadable: #4742 made list refuse
// on a skipped repo, but every refusal said "corrupted instances.json" and
// "fix the JSON". Once the daemon also skips repos it cannot READ (#4783), that
// advice is wrong: the bytes may be intact, and the fix is access, not an edit.
func TestListSessions_DaemonUpUnreadableSkipSaysUnreadable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	corruptStub(t, []session.InstanceData{{Title: "survivor"}}, []daemon.SkippedRepo{unreadableSkip(unreadableTestRepo)})

	_, err := listSessions("")
	if err == nil {
		t.Fatal("all-repo list must refuse when a repo could not be read, got nil error")
	}
	assertReadsAsUnreadable(t, err.Error())
}

// TestGetSessionByTitle_DaemonUpUnreadableMissSaysUnreadable is the get miss
// caveat counterpart.
func TestGetSessionByTitle_DaemonUpUnreadableMissSaysUnreadable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	corruptStub(t, []session.InstanceData{{Title: "someone-else"}}, []daemon.SkippedRepo{unreadableSkip(unreadableTestRepo)})

	_, _, err := getSessionByTitle("ghost")
	if err == nil {
		t.Fatal("miss against an unreadable repo must caveat, not return nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("must still read as a not-found, got: %v", err)
	}
	assertReadsAsUnreadable(t, err.Error())
}

// TestWhoamiSession_DaemonUpUnreadableNoMatchSaysUnreadable is the whoami
// counterpart.
func TestWhoamiSession_DaemonUpUnreadableNoMatchSaysUnreadable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	corruptStub(t, []session.InstanceData{{Title: "other", TmuxName: "af_other_agent"}}, []daemon.SkippedRepo{unreadableSkip(unreadableTestRepo)})

	_, err := whoamiSession("af_nobody_agent")
	if err == nil {
		t.Fatal("no-match whoami against an unreadable repo must caveat, not return nil")
	}
	assertReadsAsUnreadable(t, err.Error())
}

// TestListSessions_DaemonUpMixedSkipsNameEachWithItsRemedy: one corrupt and one
// unreadable repo each get their own sentence and remedy, so neither is
// described with the other's advice.
func TestListSessions_DaemonUpMixedSkipsNameEachWithItsRemedy(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	corruptStub(t, nil, append(skippedSet(skippedTestRepo), unreadableSkip(unreadableTestRepo)))

	_, err := listSessions("")
	if err == nil {
		t.Fatal("list must refuse, got nil error")
	}
	corruptPart, unreadablePart, ok := strings.Cut(err.Error(), "repo(s) have an instances.json the daemon could not read")
	if !ok {
		t.Fatalf("must carry an unreadable sentence, got: %v", err)
	}
	if !strings.Contains(corruptPart, skippedTestRepo) || !strings.Contains(corruptPart, "corrupted instances.json") || !strings.Contains(corruptPart, "fix the JSON") {
		t.Fatalf("the corrupt repo keeps its #4742 sentence and remedy, got: %v", err)
	}
	if strings.Contains(corruptPart, unreadableTestRepo) {
		t.Fatalf("the unreadable repo must not be listed as corrupted, got: %v", err)
	}
	if !strings.Contains(unreadablePart, unreadableTestRepo) || strings.Contains(unreadablePart, skippedTestRepo) {
		t.Fatalf("the unreadable sentence names only the unreadable repo, got: %v", err)
	}
}

// TestListSessions_DaemonUpUnknownReasonReadsAsCorrupted pins version skew: an
// older daemon sends the corrupted reason or none, and a newer one may send a
// reason this client does not know. Those keep the #4742 corrupted wording.
func TestListSessions_DaemonUpUnknownReasonReadsAsCorrupted(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	corruptStub(t, nil, []daemon.SkippedRepo{{RepoID: skippedTestRepo}, {RepoID: "future-repo", Reason: "some-future-reason"}})

	_, err := listSessions("")
	if err == nil {
		t.Fatal("list must refuse, got nil error")
	}
	if !strings.Contains(err.Error(), "2 repo(s) have a corrupted instances.json") {
		t.Fatalf("empty and unknown reasons read as corrupted, got: %v", err)
	}
}
