package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// pendingCreateIDs lists the instance ids the pending-create journal currently
// holds, sorted so tests can compare the whole set.
func pendingCreateIDs(t *testing.T) []string {
	t.Helper()
	markers, err := readPendingCreateMarkers()
	if err != nil {
		t.Fatalf("readPendingCreateMarkers: %v", err)
	}
	ids := make([]string, 0, len(markers))
	for _, marker := range markers {
		ids = append(ids, marker.InstanceID)
	}
	sort.Strings(ids)
	return ids
}

// seedPendingCreate writes the journal entry a create leaves behind when its
// daemon dies between `tmux new-session` and the instance-record write (#6301).
func seedPendingCreate(t *testing.T, repoID, instanceID, sessionName, program string) pendingCreateMarker {
	t.Helper()
	marker := pendingCreateMarker{
		InstanceID:  instanceID,
		RepoID:      repoID,
		Title:       "worker",
		SessionName: sessionName,
		Program:     program,
		MintedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := notePendingCreate(marker); err != nil {
		t.Fatalf("notePendingCreate: %v", err)
	}
	return marker
}

func TestPendingCreateJournalRoundTrip(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", resolvedTempDir(t))
	marker := seedPendingCreate(t, "repo-1", "inst-1", "af_worker_a1b2c3d4", "claude")
	markers, err := readPendingCreateMarkers()
	if err != nil {
		t.Fatalf("readPendingCreateMarkers: %v", err)
	}
	if len(markers) != 1 || markers[0] != marker {
		t.Fatalf("journal round-trip = %+v, want %+v", markers, marker)
	}
	clearPendingCreate(marker.InstanceID)
	if ids := pendingCreateIDs(t); len(ids) != 0 {
		t.Fatalf("journal after clear = %v, want empty", ids)
	}
}

// The marker must exist BEFORE any spawn can happen. Driving the create with a
// fake backend, the backend factory callback runs inside session.NewInstance —
// the last code point before worktree/tmux provisioning — so observing the
// journaled entry there proves the write precedes the spawn boundary (#6301).
func TestCreateSession_JournalsBeforeSpawnAndClearsOnCommit(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	var journaled []pendingCreateMarker
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		markers, rerr := readPendingCreateMarkers()
		if rerr != nil {
			t.Errorf("journal must be readable inside NewInstance: %v", rerr)
		}
		journaled = markers
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return readyFakeBackend{backend}, nil
	})
	t.Cleanup(restore)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "journaled", RepoPath: repoPath, Program: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(journaled) != 1 {
		t.Fatalf("journal inside NewInstance = %+v, want the one attempt", journaled)
	}
	got := journaled[0]
	wantName := tmux.SanitizedNameForRepo("journaled", repo.IdentityPath())
	if got.InstanceID != data.ID || got.SessionName != wantName || got.RepoID != repo.ID || got.Program != "claude" {
		t.Fatalf("journaled marker = %+v, want {id:%s session:%s repo:%s program:claude}", got, data.ID, wantName, repo.ID)
	}
	if ids := pendingCreateIDs(t); len(ids) != 0 {
		t.Fatalf("journal after committed create = %v, want empty (the row owns the id now)", ids)
	}
}

// A start failure whose teardown ANSWERED leaves nothing live to prove, so the
// journal entry must be dropped with the create — only a possibly-live,
// unrecorded residue may keep it.
func TestCreateSession_FailedStartClearsJournal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	restore := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		backend := session.NewFakeBackend()
		backend.FailStart(errors.New("launch refused"))
		return readyFakeBackend{backend}, nil
	})
	t.Cleanup(restore)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "failed", RepoPath: repoPath, Program: "claude",
	}); err == nil {
		t.Fatal("CreateSession should report the failed start")
	}
	if ids := pendingCreateIDs(t); len(ids) != 0 {
		t.Fatalf("journal after answered teardown = %v, want empty", ids)
	}
}

// A create stranded between `tmux new-session` and the record write is reaped
// by proof: the journaled id, unowned by any committed row, still stamped on
// the live session's environment. The kill binds the exact incarnation ($7),
// never the name.
func TestSweepUnrecordedCreate_ReapsJournaledLeftover(t *testing.T) {
	_, repo, sanitized, home := fakeReapFixture(t, "worker")
	seedPendingCreate(t, repo.ID, reapInstanceID, sanitized, "claude")
	logPath := installFakeReapTmux(t, sanitized, fakeReapTmux{instance: reapInstanceID, home: home})

	sweepUnrecordedCreateSessions()

	kills := killsIn(fakeTmuxLog(t, logPath))
	if len(kills) != 1 || kills[0] != "kill-session -t $7" {
		t.Fatalf("the proven leftover must die by its incarnation id and nothing else; got %q", kills)
	}
	if ids := pendingCreateIDs(t); len(ids) != 0 {
		t.Fatalf("journal after reap = %v, want empty", ids)
	}
}

// Never by name: a session that does not prove the journaled attempt — or an
// attempt that was never journaled — is never sent a kill, and every doubtful
// outcome keeps both the session and the marker for af doctor.
func TestSweepUnrecordedCreate_LeavesUnprovenSessions(t *testing.T) {
	cases := []struct {
		name string
		// session is the live tmux session answering under the journaled name.
		session func(home string) fakeReapTmux
		// journal, when false, leaves the journal empty: the session's stamp
		// has no recorded attempt to be proven against.
		journal bool
		// commit gives the journaled id a committed instance row.
		commit bool
		// commitOwner commits a row that owns the session by its tmux name
		// under a different id — the shape a fire-replay quarantine row takes.
		commitOwner bool
		// journalKept: the marker must survive the sweep (retry next start)
		// rather than be consumed.
		journalKept bool
	}{
		{
			name:    "a committed row owns the id",
			session: func(home string) fakeReapTmux { return fakeReapTmux{instance: reapInstanceID, home: home} },
			journal: true,
			commit:  true,
		},
		{
			name:    "a quarantine row owns the session by tmux name",
			session: func(home string) fakeReapTmux { return fakeReapTmux{instance: reapInstanceID, home: home} },
			journal: true,
			// The row's fresh id does not equal the journaled id, but its
			// TmuxName records durable ownership of the live session — the
			// session is recorded, not unrecorded.
			commitOwner: true,
		},
		{
			name:    "the session carries a foreign id",
			session: func(home string) fakeReapTmux { return fakeReapTmux{instance: "someone-elses-attempt", home: home} },
			journal: true,
		},
		{
			name:    "the session carries no stamp",
			session: func(home string) fakeReapTmux { return fakeReapTmux{home: home} },
			journal: true,
		},
		{
			name: "the environment cannot be read",
			session: func(home string) fakeReapTmux {
				return fakeReapTmux{instance: reapInstanceID, home: home, unreadable: true}
			},
			journal:     true,
			journalKept: true,
		},
		{
			name:        "the id matches but the home is foreign",
			session:     func(string) fakeReapTmux { return fakeReapTmux{instance: reapInstanceID, home: "/some/other/af-home"} },
			journal:     true,
			journalKept: true,
		},
		{
			name:    "the id was never journaled",
			session: func(home string) fakeReapTmux { return fakeReapTmux{instance: reapInstanceID, home: home} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoPath, repo, sanitized, home := fakeReapFixture(t, "worker")
			if tc.journal {
				seedPendingCreate(t, repo.ID, reapInstanceID, sanitized, "claude")
			}
			if tc.commit {
				if err := appendInstanceData(repo.ID, session.InstanceData{
					ID: reapInstanceID, Title: "worker", Path: repoPath, Program: "claude",
				}); err != nil {
					t.Fatalf("appendInstanceData: %v", err)
				}
			}
			if tc.commitOwner {
				if err := appendInstanceData(repo.ID, session.InstanceData{
					ID: "quarantine-row-" + reapInstanceID, Path: repoPath,
					Program: "claude", TmuxName: sanitized, StartupStateUnknown: true,
				}); err != nil {
					t.Fatalf("appendInstanceData: %v", err)
				}
			}
			logPath := installFakeReapTmux(t, sanitized, tc.session(home))

			sweepUnrecordedCreateSessions()

			if kills := killsIn(fakeTmuxLog(t, logPath)); len(kills) != 0 {
				t.Fatalf("an unproven session must not be killed; tmux was sent %q", kills)
			}
			ids := pendingCreateIDs(t)
			if !tc.journal && len(ids) != 0 {
				t.Fatalf("journal = %v, want empty (no marker was written)", ids)
			}
			if tc.journal && (len(ids) == 1) != tc.journalKept {
				t.Fatalf("journal = %v, want journalKept=%v", ids, tc.journalKept)
			}
		})
	}
}

// A marker whose recorded name no longer answers is stale — consumed without
// any kill attempt.
func TestSweepUnrecordedCreate_ClearsStaleMarker(t *testing.T) {
	_, repo, _, _ := fakeReapFixture(t, "worker")
	seedPendingCreate(t, repo.ID, reapInstanceID, "af_gone_session_a1b2c3d4", "claude")
	logPath := installFakeReapTmux(t, "af_other_b2c3d4e5", fakeReapTmux{home: "/unrelated"})

	sweepUnrecordedCreateSessions()

	if kills := killsIn(fakeTmuxLog(t, logPath)); len(kills) != 0 {
		t.Fatalf("a stale marker must not kill anything; tmux was sent %q", kills)
	}
	if ids := pendingCreateIDs(t); len(ids) != 0 {
		t.Fatalf("journal = %v, want the stale marker cleared", ids)
	}
}

// "Unrecorded" cannot be proven while the committed record cannot be read:
// both the session and the marker stay.
func TestSweepUnrecordedCreate_KeepsUnreadableRecords(t *testing.T) {
	_, repo, sanitized, home := fakeReapFixture(t, "worker")
	seedPendingCreate(t, repo.ID, reapInstanceID, sanitized, "claude")
	if err := os.MkdirAll(filepath.Join(home, "instances", repo.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "instances", repo.ID, "instances.json"), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := installFakeReapTmux(t, sanitized, fakeReapTmux{instance: reapInstanceID, home: home})

	sweepUnrecordedCreateSessions()

	if kills := killsIn(fakeTmuxLog(t, logPath)); len(kills) != 0 {
		t.Fatalf("nothing may be killed while records are unreadable; tmux was sent %q", kills)
	}
	if ids := pendingCreateIDs(t); len(ids) != 1 {
		t.Fatalf("journal = %v, want the marker kept for the next start", ids)
	}
}

// The kill is bound to the incarnation the environment was read from: a
// replacement server answering under the same id by the re-confirm is not it.
func TestSweepUnrecordedCreate_SkipsReplacedIncarnation(t *testing.T) {
	_, repo, sanitized, home := fakeReapFixture(t, "worker")
	seedPendingCreate(t, repo.ID, reapInstanceID, sanitized, "claude")
	logPath := installFakeReapTmux(t, sanitized, fakeReapTmux{
		instance: reapInstanceID, home: home,
		// The name resolved to $7 on a server whose pid/created were
		// 4242/1700000000; by the re-confirm a replacement server answers
		// $7 differently.
		flipIncarnation: "$7 4343 1700000100",
	})

	sweepUnrecordedCreateSessions()

	if kills := killsIn(fakeTmuxLog(t, logPath)); len(kills) != 0 {
		t.Fatalf("the replacement incarnation must survive; tmux was sent %q", kills)
	}
	if ids := pendingCreateIDs(t); len(ids) != 1 {
		t.Fatalf("journal = %v, want the marker kept", ids)
	}
}

// On a real tmux server the journaled leftover is found by `ls`, proven by its
// stamped environment, and killed — the whole crash-window story end to end.
func TestSweepUnrecordedCreate_RealTmuxReapsProvenLeftover(t *testing.T) {
	testguard.IsolateTmux(t)
	_, repo, sanitized, home := fakeReapFixture(t, "real-leftover")
	seedPendingCreate(t, repo.ID, reapInstanceID, sanitized, "claude")
	seedRealLeftoverSession(t, sanitized, home, "-e", tmux.EnvMarkerInstance+"="+reapInstanceID)

	sweepUnrecordedCreateSessions()

	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err == nil {
		t.Fatalf("the proven leftover must be reaped, still alive: %s", out)
	}
	if ids := pendingCreateIDs(t); len(ids) != 0 {
		t.Fatalf("journal after reap = %v, want empty", ids)
	}
}

// The never-by-name core on a real server: a session whose AF_INSTANCE_ID has
// no journal entry is unprovable and survives — af doctor reports it.
func TestSweepUnrecordedCreate_RealTmuxKeepsUnjournaledSession(t *testing.T) {
	testguard.IsolateTmux(t)
	_, _, sanitized, home := fakeReapFixture(t, "unjournaled")
	seedRealLeftoverSession(t, sanitized, home, "-e", tmux.EnvMarkerInstance+"="+reapInstanceID)

	sweepUnrecordedCreateSessions()

	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err != nil {
		t.Fatalf("an unjournaled session must never be reaped by name: %v: %s", err, out)
	}
}
