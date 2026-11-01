package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// #6503: a daemon that is UP but did not answer authoritatively (still
// restoring, a dial timeout under load, an undecodable reply) must not be
// answered for by the disk fallback when the request spans every project. The
// disk can lag the daemon's live state, so `[]` or "not found" would read as
// authoritative for a scope the disk cannot vouch for.

func notAnswering(cause string) func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
	return func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return nil, fmt.Errorf("%w: %s", daemon.ErrDaemonNotAnswering, cause)
	}
}

var notAnsweringKinds = map[string]string{
	"dial timeout":      "dial unix /x/daemon.sock: i/o timeout",
	"starting envelope": "agent-factory daemon is starting; retry shortly",
	"decode error":      "apiclient: malformed response envelope: unexpected end of JSON input",
}

// seedLaggingDisk leaves the disk EMPTY of the live session — the lag the daemon
// is ahead of.
func seedLaggingDisk(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	if err := config.SaveRepoInstances("repo-a", json.RawMessage(`[]`)); err != nil {
		t.Fatalf("seed disk: %v", err)
	}
}

func TestOutsideRepoListNeverAnswersFromDiskWhenDaemonIsNotAnswering(t *testing.T) {
	for kind, cause := range notAnsweringKinds {
		t.Run(kind, func(t *testing.T) {
			seedLaggingDisk(t)
			stubSnapshot(t, notAnswering(cause))
			got, err := listSessions("")
			if err == nil {
				t.Fatalf("outside-repo list answered %v from disk while the daemon was up but not answering", titlesOf(got))
			}
			if !errors.Is(err, daemon.ErrDaemonNotAnswering) {
				t.Fatalf("error %q does not carry ErrDaemonNotAnswering", err)
			}
		})
	}
}

func TestOutsideRepoLookupNeverReportsNotFoundWhenDaemonIsNotAnswering(t *testing.T) {
	for kind, cause := range notAnsweringKinds {
		t.Run(kind, func(t *testing.T) {
			seedLaggingDisk(t)
			stubSnapshot(t, notAnswering(cause))
			_, _, err := getSessionByTitleInScope("", "alpha")
			if err == nil {
				t.Fatal("lookup unexpectedly succeeded")
			}
			if errors.Is(err, errTitleNotFound) {
				t.Fatalf("lookup reported a clean not-found while the daemon was up but not answering: %v", err)
			}
			if !errors.Is(err, daemon.ErrDaemonNotAnswering) {
				t.Fatalf("error %q does not say the daemon is not ready", err)
			}
		})
	}
}

// Inside a repo the fallback stays: that repo's own instances.json is the whole
// record for that one scope.
func TestRepoScopedReadKeepsItsDiskFallbackWhenDaemonIsNotAnswering(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	if err := config.SaveRepoInstances("repo-a", json.RawMessage(`[{"title":"alpha"}]`)); err != nil {
		t.Fatalf("seed disk: %v", err)
	}
	stubSnapshot(t, notAnswering(notAnsweringKinds["starting envelope"]))
	got, err := listSessions("repo-a")
	if err != nil || !equalStrs(titlesOf(got), []string{"alpha"}) {
		t.Fatalf("repo-scoped list = %v, %v; want the repo's own disk record", titlesOf(got), err)
	}
}

// No daemon at all is not "not answering": the disk is the only truth there is,
// so scripts and CI with no daemon keep their outside-repo list.
func TestOutsideRepoListStillReadsDiskWhenNoDaemonExists(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	if err := config.SaveRepoInstances("repo-a", json.RawMessage(`[{"title":"alpha"}]`)); err != nil {
		t.Fatalf("seed disk: %v", err)
	}
	stubSnapshot(t, daemonUnavailable)
	got, err := listSessions("")
	if err != nil || !equalStrs(titlesOf(got), []string{"alpha"}) {
		t.Fatalf("no-daemon outside-repo list = %v, %v; want the disk scan", titlesOf(got), err)
	}
}
