package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/rpc"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// #7092: a play-test read `[]` and `instance "alpha" not found` as "the daemon
// lost my sessions". Both answers were scoped to the project the CLI resolved
// from the current directory, and neither said so. These pin that a cwd-scoped
// answer names its scope. An empty list prints one stderr line naming the
// project, and a title miss names the project and how to widen the search.
// Stdout's JSON, the exit status, and the not-found classification stay as they
// were, and an explicit --repo or an unscoped lookup keeps its own wording.

// cwdScopedRepo makes a project, moves into it, and starts from clean flags,
// so the CLI scopes every lookup to that project the way the play-test's did.
func cwdScopedRepo(t *testing.T) string {
	t.Helper()
	useTempConfig(t)
	resetScopeFlags(t)
	silenceCobraOutput()
	repo := mkRepo(t, "alpha")
	t.Chdir(repo)
	return repo
}

// daemonScopedMiss is the daemon's scoped not-found as the CLI receives it:
// net/rpc flattens the error to its text (a rpc.ServerError), so no sentinel
// survives the hop. The text is findInstanceDataByTitle's scoped miss, verbatim
// from the #7092 report.
func daemonScopedMiss(title string) error {
	return rpc.ServerError(fmt.Sprintf("instance %q not found", title))
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestSessionsList_CwdScopedEmptyNamesTheProject is the list half of #7092:
// inside a repository the list is that project's, and an empty one must say
// which project it searched and how to widen it. Stdout stays the bare `[]` and
// the command still succeeds.
func TestSessionsList_CwdScopedEmptyNamesTheProject(t *testing.T) {
	repo := cwdScopedRepo(t)
	warn := captureWarnWriter(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, nil })

	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	if got := strings.TrimSpace(string(out)); got != "[]" {
		t.Fatalf("stdout must stay the bare empty JSON array, got %q", out)
	}

	lines := nonEmptyLines(warn.String())
	if len(lines) != 1 {
		t.Fatalf("an empty cwd-scoped list must print exactly one stderr line, got %q", warn.String())
	}
	for _, want := range []string{"af sessions list:", "project " + repo, "resolved from the current directory", "--repo <path>", "--all"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the empty-scope notice must contain %q, got: %q", want, lines[0])
		}
	}
	if strings.Contains(lines[0], "not inside a project repo") {
		t.Errorf("a scoped list must not claim it had no project context, got: %q", lines[0])
	}
}

// TestSessionsList_CwdScopedFilteredEmptySaysMatching: with a filter set, an
// empty answer does not mean the project holds no sessions, so the notice must
// not claim that.
func TestSessionsList_CwdScopedFilteredEmptySaysMatching(t *testing.T) {
	repo := cwdScopedRepo(t)
	warn := captureWarnWriter(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, nil })
	sessionsListLiveFlag = true

	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	if !json.Valid(out) {
		t.Fatalf("stdout must stay valid JSON, got %q", out)
	}
	lines := nonEmptyLines(warn.String())
	if len(lines) != 1 {
		t.Fatalf("a filtered empty cwd-scoped list must print exactly one stderr line, got %q", warn.String())
	}
	if !strings.Contains(lines[0], "no matching sessions in project "+repo) {
		t.Errorf("a filtered empty list must say no session matched, not that the project is empty, got: %q", lines[0])
	}
}

// TestSessionsList_CwdScopedNonEmptyPrintsNoNotice: an answer with rows needs no
// caveat. The rows themselves show which project was listed.
func TestSessionsList_CwdScopedNonEmptyPrintsNoNotice(t *testing.T) {
	repo := cwdScopedRepo(t)
	warn := captureWarnWriter(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "foo", Path: repo}}, nil
	})

	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	if !json.Valid(out) {
		t.Fatalf("stdout must stay valid JSON, got %q", out)
	}
	if warn.String() != "" {
		t.Errorf("a non-empty scoped list must print nothing on stderr, got %q", warn.String())
	}
}

// TestSessionsList_ExplicitRepoEmptyPrintsNoNotice: the user named the project
// with --repo, so an empty answer is unambiguous and the notice stays out of
// the way.
func TestSessionsList_ExplicitRepoEmptyPrintsNoNotice(t *testing.T) {
	repo := cwdScopedRepo(t)
	warn := captureWarnWriter(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, nil })
	repoFlag = repo

	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	if !json.Valid(out) {
		t.Fatalf("stdout must stay valid JSON, got %q", out)
	}
	if warn.String() != "" {
		t.Errorf("an explicit --repo must not print the empty-scope notice, got %q", warn.String())
	}
}

// TestSessionsList_AllFromInsideRepoPrintsNoNotice: --all spans every project on
// purpose, so an empty answer means an empty fleet and needs no caveat.
func TestSessionsList_AllFromInsideRepoPrintsNoNotice(t *testing.T) {
	cwdScopedRepo(t)
	warn := captureWarnWriter(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, nil })
	sessionsListAllFlag = true

	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	if !json.Valid(out) {
		t.Fatalf("stdout must stay valid JSON, got %q", out)
	}
	if warn.String() != "" {
		t.Errorf("--all must not print the empty-scope notice, got %q", warn.String())
	}
}

// TestSessionsList_CwdScopedEmptyNoticeSuppressedUnderJSON: under --json, stderr
// carries the {data,error} envelope (#3169), so this caveat waits for #3511's
// structural channel like the #6882 notice does.
func TestSessionsList_CwdScopedEmptyNoticeSuppressedUnderJSON(t *testing.T) {
	cwdScopedRepo(t)
	warn := captureWarnWriter(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, nil })

	withEnvelope(t, func() {
		out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
		if !json.Valid(out) {
			t.Fatalf("stdout must stay the envelope, got %q", out)
		}
	})
	if warn.String() != "" {
		t.Errorf("under --json the notice must not corrupt stderr, got %q", warn.String())
	}
}

// requireCwdScopedMiss asserts the #7092 shape: the original miss text first,
// so anything matching on it still matches, and then the project it searched,
// where that scope came from, and how to widen it.
func requireCwdScopedMiss(t *testing.T, err error, head, repo string) {
	t.Helper()
	if err == nil {
		t.Fatal("a title the project does not hold must not resolve")
	}
	want := head + " in project " + repo + " (resolved from the current directory)"
	if !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("the miss must start %q, got: %v", want, err)
	}
	for _, remedy := range []string{"--repo <path>", "run outside the repo to search every project"} {
		if !strings.Contains(err.Error(), remedy) {
			t.Errorf("the miss must say how to widen the search (%q), got: %v", remedy, err)
		}
	}
}

// TestSessionsGet_CwdScopedMissNamesTheProject: the CLI-side lookup's miss
// names the project and stays errTitleNotFound for the callers that classify on
// it (#861).
func TestSessionsGet_CwdScopedMissNamesTheProject(t *testing.T) {
	repo := cwdScopedRepo(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: repo}}, nil
	})

	err := sessionsGetCmd.RunE(sessionsGetCmd, []string{"alpha"})
	requireCwdScopedMiss(t, err, `session "alpha" not found`, repo)
	if !errors.Is(err, errTitleNotFound) {
		t.Errorf("the decorated miss must stay errTitleNotFound, got: %v", err)
	}
}

// TestSessionsGet_ExplicitRepoMissNamesTheProjectOnly: --repo named the
// project, so the miss names it without claiming the current directory chose it.
func TestSessionsGet_ExplicitRepoMissNamesTheProjectOnly(t *testing.T) {
	repo := cwdScopedRepo(t)
	t.Chdir(t.TempDir()) // not a git repository: the scope is --repo's alone
	repoFlag = repo
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: repo}}, nil
	})

	err := sessionsGetCmd.RunE(sessionsGetCmd, []string{"alpha"})
	if err == nil {
		t.Fatal("a title the project does not hold must not resolve")
	}
	if want := `session "alpha" not found in project ` + repo; err.Error() != want {
		t.Errorf("an explicit --repo miss must read %q, got: %v", want, err)
	}
	if !errors.Is(err, errTitleNotFound) {
		t.Errorf("the decorated miss must stay errTitleNotFound, got: %v", err)
	}
}

// TestSessionsAttach_CwdScopedMissNamesTheProject: attach resolves the title
// from this project's records before dialing anything, and that miss is the
// same cwd-scoped answer as get's.
func TestSessionsAttach_CwdScopedMissNamesTheProject(t *testing.T) {
	repo := cwdScopedRepo(t)
	ctx, err := config.RepoFromPath(repo)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	saveOneRepoInstances(t, ctx.ID, session.InstanceData{Title: "someone-else", Path: repo})

	err = sessionsAttachCmd.RunE(sessionsAttachCmd, []string{"alpha"})
	requireCwdScopedMiss(t, err, `session "alpha" not found`, repo)
}

// TestSessionsWatch_CwdScopedMissNamesTheProject: a title that never resolved
// is the scoped miss; one that resolved and then vanished keeps its own
// "disappeared" wording (watchForReady), which this decoration must not touch.
func TestSessionsWatch_CwdScopedMissNamesTheProject(t *testing.T) {
	repo := cwdScopedRepo(t)
	prevAll, prevCurrent := watchAllFlag, watchIncludeCurrentFlag
	watchAllFlag, watchIncludeCurrentFlag = false, false
	t.Cleanup(func() { watchAllFlag, watchIncludeCurrentFlag = prevAll, prevCurrent })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: repo}}, nil
	})

	err := sessionsWatchCmd.RunE(sessionsWatchCmd, []string{"alpha"})
	requireCwdScopedMiss(t, err, `session "alpha" not found`, repo)
}

// TestSendPrompt_CwdScopedMissNamesTheProject: the send-prompt exists check
// read only this project's records, so its miss says so and keeps the --create
// affordance.
func TestSendPrompt_CwdScopedMissNamesTheProject(t *testing.T) {
	resetSendPromptState(t)
	repo := cwdScopedRepo(t)
	stubDaemon(t)

	err := sessionsSendPromptCmd.RunE(sessionsSendPromptCmd, []string{"alpha", "hi"})
	requireCwdScopedMiss(t, err, `session "alpha" not found`, repo)
	if !strings.Contains(err.Error(), "--create") {
		t.Errorf("the miss must keep the --create affordance, got: %v", err)
	}
}

// TestSessionsTabCreate_ExplicitRepoMissNamesTheProjectOnly is the daemon-side
// twin of the --repo get case.
func TestSessionsTabCreate_ExplicitRepoMissNamesTheProjectOnly(t *testing.T) {
	repo := cwdScopedRepo(t)
	t.Chdir(t.TempDir())
	repoFlag = repo
	setTabCreateFlagsForTest(t, "btop", "", "", "", 0)
	prev := createTabViaDaemon
	createTabViaDaemon = func(req daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		return daemon.CreateTabResponse{}, daemonScopedMiss(req.Title)
	}
	t.Cleanup(func() { createTabViaDaemon = prev })

	err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"alpha"})
	if err == nil {
		t.Fatal("a title the project does not hold must not resolve")
	}
	if want := `instance "alpha" not found in project ` + repo; err.Error() != want {
		t.Errorf("an explicit --repo miss must read %q, got: %v", want, err)
	}
}

// TestSessionsTabCreate_OtherDaemonErrorsPassThrough: only the clean scoped miss
// is renamed. A refusal that is not a miss must keep its own text. Neither must
// a "session %q not found" from a row the daemon found but could not restore,
// since naming the project there would send the user looking in the wrong place.
func TestSessionsTabCreate_OtherDaemonErrorsPassThrough(t *testing.T) {
	cwdScopedRepo(t)
	setTabCreateFlagsForTest(t, "btop", "", "", "", 0)
	for _, msg := range []string{
		"agent-factory daemon is starting (restoring sessions); retry shortly",
		`session "alpha" not found`,
		`instance "beta" not found`,
	} {
		prev := createTabViaDaemon
		createTabViaDaemon = func(daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
			return daemon.CreateTabResponse{}, rpc.ServerError(msg)
		}
		err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"alpha"})
		createTabViaDaemon = prev
		if err == nil || err.Error() != msg {
			t.Errorf("daemon error %q must pass through unchanged, got: %v", msg, err)
		}
	}
}

// TestDaemonRoutedTitleVerbs_CwdScopedMissNamesTheProject covers every verb that
// sends a title and the cwd's repo ID to the daemon: each one surfaces the
// daemon's scoped miss with the project named. A verb added later without the
// scope decoration fails here once it is listed.
func TestDaemonRoutedTitleVerbs_CwdScopedMissNamesTheProject(t *testing.T) {
	type missFn func(title, repoID string) error
	cases := []struct {
		name    string
		install func(t *testing.T, miss missFn)
		run     func() error
	}{
		{
			name: "kill",
			install: func(t *testing.T, miss missFn) {
				prev := killSessionViaDaemon
				killSessionViaDaemon = func(req daemon.KillSessionRequest) (session.KeptBranch, error) {
					return session.KeptBranch{}, miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { killSessionViaDaemon = prev })
			},
			run: func() error { return sessionsKillCmd.RunE(sessionsKillCmd, []string{"alpha"}) },
		},
		{
			name: "archive",
			install: func(t *testing.T, miss missFn) {
				prev := archiveSessionViaDaemon
				archiveSessionViaDaemon = func(req daemon.ArchiveSessionRequest) (string, *session.ArchivedWorkspaceData, error) {
					return "", nil, miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { archiveSessionViaDaemon = prev })
			},
			run: func() error { return sessionsArchiveCmd.RunE(sessionsArchiveCmd, []string{"alpha"}) },
		},
		{
			name: "restore",
			install: func(t *testing.T, miss missFn) {
				prev := restoreSessionViaDaemon
				restoreSessionViaDaemon = func(req daemon.RestoreSessionRequest) (string, error) {
					return "", miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { restoreSessionViaDaemon = prev })
			},
			run: func() error { return sessionsRestoreCmd.RunE(sessionsRestoreCmd, []string{"alpha"}) },
		},
		{
			name: "preview",
			install: func(t *testing.T, miss missFn) {
				prev := previewSessionViaDaemon
				previewSessionViaDaemon = func(req daemon.PreviewRequest) (daemon.PreviewResponse, error) {
					return daemon.PreviewResponse{}, miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { previewSessionViaDaemon = prev })
			},
			run: func() error { return sessionsPreviewCmd.RunE(sessionsPreviewCmd, []string{"alpha"}) },
		},
		{
			name: "tab-create",
			install: func(t *testing.T, miss missFn) {
				setTabCreateFlagsForTest(t, "btop", "", "", "", 0)
				prev := createTabViaDaemon
				createTabViaDaemon = func(req daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
					return daemon.CreateTabResponse{}, miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { createTabViaDaemon = prev })
			},
			run: func() error { return sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"alpha"}) },
		},
		{
			name: "tab-delete",
			install: func(t *testing.T, miss missFn) {
				prevName := tabDeleteNameFlag
				tabDeleteNameFlag = "worker"
				prev := closeTabViaDaemon
				closeTabViaDaemon = func(req daemon.CloseTabRequest) (string, error) {
					return "", miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { closeTabViaDaemon, tabDeleteNameFlag = prev, prevName })
			},
			run: func() error { return sessionsTabDeleteCmd.RunE(sessionsTabDeleteCmd, []string{"alpha"}) },
		},
		{
			name: "tab-rename",
			install: func(t *testing.T, miss missFn) {
				prevName, prevNew := tabRenameNameFlag, tabRenameNewNameFlag
				tabRenameNameFlag, tabRenameNewNameFlag = "worker", "builder"
				prev := renameTabViaDaemon
				renameTabViaDaemon = func(req daemon.RenameTabRequest) (string, error) {
					return "", miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() {
					renameTabViaDaemon = prev
					tabRenameNameFlag, tabRenameNewNameFlag = prevName, prevNew
				})
			},
			run: func() error { return sessionsTabRenameCmd.RunE(sessionsTabRenameCmd, []string{"alpha"}) },
		},
		{
			name: "tab-reorder",
			install: func(t *testing.T, miss missFn) {
				prevName := tabReorderNameFlag
				tabReorderNameFlag = "worker"
				prev := reorderTabViaDaemon
				reorderTabViaDaemon = func(req daemon.ReorderTabRequest) (string, int, error) {
					return "", 0, miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { reorderTabViaDaemon, tabReorderNameFlag = prev, prevName })
			},
			run: func() error { return sessionsTabReorderCmd.RunE(sessionsTabReorderCmd, []string{"alpha"}) },
		},
		{
			name: "attention",
			install: func(t *testing.T, miss missFn) {
				prev := setAttentionViaDaemon
				setAttentionViaDaemon = func(req daemon.SetAttentionRequest) (daemon.SetAttentionResponse, error) {
					return daemon.SetAttentionResponse{}, miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { setAttentionViaDaemon = prev })
			},
			run: func() error {
				cmd := newSessionsAttentionCmd()
				return cmd.RunE(cmd, []string{"alpha"})
			},
		},
		{
			name: "handoff",
			install: func(t *testing.T, miss missFn) {
				prevTo := sessionsHandoffTo
				sessionsHandoffTo = "claude"
				prev := handoffSessionViaDaemon
				handoffSessionViaDaemon = func(req daemon.HandoffSessionRequest) (daemon.HandoffSessionResponse, error) {
					return daemon.HandoffSessionResponse{}, miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { handoffSessionViaDaemon, sessionsHandoffTo = prev, prevTo })
			},
			run: func() error { return sessionsHandoffCmd.RunE(sessionsHandoffCmd, []string{"alpha"}) },
		},
		{
			name: "retry-limit",
			install: func(t *testing.T, miss missFn) {
				prev := resumeFromLimitViaDaemon
				resumeFromLimitViaDaemon = func(req daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
					return daemon.ResumeFromLimitResponse{}, miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { resumeFromLimitViaDaemon = prev })
			},
			run: func() error { return sessionsRetryLimitCmd.RunE(sessionsRetryLimitCmd, []string{"alpha"}) },
		},
		{
			name: "set-conversation",
			install: func(t *testing.T, miss missFn) {
				prev := setConversationViaDaemon
				setConversationViaDaemon = func(req daemon.SetConversationRequest) error {
					return miss(req.Title, req.RepoID)
				}
				t.Cleanup(func() { setConversationViaDaemon = prev })
			},
			run: func() error {
				return sessionsSetConversationCmd.RunE(sessionsSetConversationCmd, []string{"alpha", "0195f8e2-7c3a-7b1d-9e4f-2a6c8d0b1e2f"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := cwdScopedRepo(t)
			var gotRepoID string
			tc.install(t, func(title, repoID string) error {
				gotRepoID = repoID
				return daemonScopedMiss(title)
			})

			err := tc.run()
			if gotRepoID == "" {
				t.Fatalf("%s never reached the daemon with a repo scope (err: %v)", tc.name, err)
			}
			requireCwdScopedMiss(t, err, `instance "alpha" not found`, repo)
		})
	}
}

// TestNameMiss_KeepsTheResolversClausesInPlace pins the matcher's edges. A
// clause the daemon appended after the miss (SendPrompt's "; prompt not
// delivered") stays last, with the project inserted after the miss itself.
// Text that only resembles the miss is left alone, and an unscoped lookup is
// never renamed, because #6882 already explains it.
func TestNameMiss_KeepsTheResolversClausesInPlace(t *testing.T) {
	scope := lookupScope{RepoID: "repo-alpha", Project: "/repos/alpha", FromCwd: true}

	got := scope.nameMiss("alpha", rpc.ServerError(`instance "alpha" not found; prompt not delivered`))
	want := `instance "alpha" not found in project /repos/alpha (resolved from the current directory); ` +
		`pass --repo <path> to search another project, or run outside the repo to search every project; prompt not delivered`
	if got == nil || got.Error() != want {
		t.Errorf("a trailing clause must stay last:\n got: %v\nwant: %s", got, want)
	}

	for _, msg := range []string{
		`instance "alpha" not found: corrupted instances.json`,
		`instance "alpha" not foundling`,
		`instance "alphabet" not found`,
	} {
		in := rpc.ServerError(msg)
		if got := scope.nameMiss("alpha", in); got != in {
			t.Errorf("%q is not the clean miss and must pass through unchanged, got: %v", msg, got)
		}
	}

	// A CLI-side "session %q not found" counts only when it IS errTitleNotFound.
	bare := errors.New(`session "alpha" not found`)
	if got := scope.nameMiss("alpha", bare); got != bare {
		t.Errorf("a look-alike that is not errTitleNotFound must pass through, got: %v", got)
	}

	clean := fmt.Errorf("session %q %w", "alpha", errTitleNotFound)
	if got := (lookupScope{}).nameMiss("alpha", clean); got != clean {
		t.Errorf("an unscoped lookup must keep its own miss, got: %v", got)
	}
	if got := scope.nameMiss("alpha", nil); got != nil {
		t.Errorf("no error must stay no error, got: %v", got)
	}
}
