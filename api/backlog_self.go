package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// backlogSelfSession resolves the af session this command runs in.
var backlogSelfSession = resolveSelfSession

// selfItem is the item of the session this command runs in, resolved from the
// backlog by session id.
func (t *backlogTarget) selfItem() (string, *backlog.Item, error) {
	s, err := backlogSelfSession()
	if err != nil {
		return "", nil, fmt.Errorf("--self needs an item session; run it from inside the item's session: %w", err)
	}
	v, err := t.get()
	if err != nil {
		return "", nil, err
	}
	it := v.Backlog.ItemForSession(s.ID)
	if it == nil && s.BacklogItem != 0 {
		// The start record links the session after its first prompt was delivered,
		// so a quick report can arrive before it exists. The session's own item id
		// stands for it while its run is still starting; the daemon links the run
		// when the report arrives.
		if cand := v.Backlog.Item(s.BacklogItem); cand != nil {
			if r := v.Backlog.LatestRun(cand.ID); r != nil && r.State == backlog.RunStarting && r.SessionID == "" {
				return s.ID, cand, nil
			}
		}
	}
	if it == nil {
		return "", nil, fmt.Errorf("--self needs an item session; this session (%s) was not started from the backlog", s.Title)
	}
	if r := v.Backlog.LatestRun(it.ID); r == nil || r.SessionID != s.ID {
		return "", nil, fmt.Errorf("this session (%s) is an older run of %s; only its latest session can report on it", s.Title, backlog.FormatID(it.ID))
	}
	return s.ID, it, nil
}

// inItemSession reports the item session this command runs in, or nil. A
// caller outside any af session is not an item session.
//
// The session record's own BacklogItem is a client-side chip set at creation;
// when it is set, the session was started for a backlog item, so the guard fires
// without a backlog read. It is omitempty, so an older daemon binary whose
// InstanceData lacks the field rewrites the record and silently drops the key —
// and once dropped it is never re-derived. Resolving the refusal rails solely
// from that field lets a confused agent run plan mutations (done <id>, start,
// run, queue/drop, runner on, note <id>, ...) past the guard after the drop: the
// CLI sends them with no SessionID and the daemon attributes them to the human
// user.
//
// So when the chip is absent the guard falls back to the authoritative link, a
// run record in backlog.json (ItemForSession), mirroring selfItem below. That
// fallback is resolved from the session's own project, not the command target
// (itemSessionFromAuthoritativeLink), so a confused agent that points --repo at
// another project cannot escape the guard. Failing closed on the chip — firing
// when the record says item session even though the link is unresolved — is the
// safe direction: a confused agent is refused rather than allowed to run past.
//
// When the chip is absent and the authoritative read itself fails, the second
// return is that error: a guarded mutation cannot proceed when af cannot
// determine whether the session runs a backlog item, so the caller refuses with
// the real cause rather than treating it as a non-item session and letting it
// through. (A confused agent cannot cause the read to fail; the failure is
// transient, e.g. a backlog lock timeout, and naming it costs the caller one
// retry in exactly the window where the mutation would itself contend for the
// same backlog.) The same fail-closed stance holds when the session's own
// project cannot be resolved (its recorded worktree was deleted, moved, or is
// unreadable): the second return is that error, so a confused agent that points
// --repo at another project cannot make the guard consult that unrelated
// backlog and let the mutation through. It holds too when the session has no
// recorded path and the command target's project — the fallback in that case —
// cannot be resolved: the second return is that error, so a guarded mutation
// that does not re-resolve a project before reaching the daemon (the global
// budget command) cannot pass the guard as a non-item session.
func inItemSession() (*session.InstanceData, error) {
	s, err := backlogSelfSession()
	if err != nil || s == nil {
		return nil, nil
	}
	if s.BacklogItem != 0 {
		return s, nil
	}
	in, readErr := itemSessionFromAuthoritativeLink(s)
	if readErr != nil {
		return nil, readErr
	}
	if in {
		return s, nil
	}
	return nil, nil
}

// itemSessionFromAuthoritativeLink reports whether the authoritative backlog
// link — a run record in backlog.json (ItemForSession) — confirms s is an item
// session. The link is resolved from the session's own project, the project its
// worktree path belongs to, independently of the command target: a confused
// agent that points --repo (or its cwd) at another project must not escape the
// guard, so membership is checked against the session's own backlog, not the
// command's. When the session's own project cannot be resolved — no recorded
// path, or a --daemon-url target whose path is on the daemon host — it falls
// back to the command target's project (the prior behavior), so a record that
// predates the path or a remote session is still guarded when the command
// targets its own project.
//
// When the backlog read itself fails (a transient lock timeout, etc.) the second
// return is that error, so inItemSession can fail closed on it instead of
// silently treating an unreadable link as a non-item session. A failure to
// resolve the session's own project (a recorded worktree that is deleted, moved,
// or unreadable) is also a read-class failure for this guard: the second return
// is that error so a confused agent that points --repo at another project cannot
// make the guard consult that unrelated backlog (which has no link for this
// session) and let the mutation through. A session with no recorded path, or a
// --daemon-url target whose path is on the daemon host, has nothing to resolve,
// so it returns false with no error and the guard falls back to the command
// target's project. When that fallback itself cannot be resolved (no --repo and
// the cwd is not a git repository), the second return is that error rather than
// nil: a guarded mutation that does not re-resolve the project before reaching
// the daemon (the global budget command, SetBudget) would otherwise pass the
// guard as a non-item session and let the mutation through, so the guard fails
// closed and names the fallback resolution failure rather than claiming an item
// session.
func itemSessionFromAuthoritativeLink(s *session.InstanceData) (bool, error) {
	t, resolveErr := sessionOwnBacklogTarget(s)
	if resolveErr != nil {
		return false, backlogUnresolvableSessionProjectErr(resolveErr)
	}
	if t != nil {
		v, err := t.get()
		if err != nil {
			return false, backlogUnreadableItemSessionErr(err)
		}
		return v.Backlog.ItemForSession(s.ID) != nil, nil
	}
	t, err := backlogProject()
	if err != nil {
		return false, backlogUnresolvableFallbackProjectErr(err)
	}
	v, err := t.get()
	if err != nil {
		return false, backlogUnreadableItemSessionErr(err)
	}
	return v.Backlog.ItemForSession(s.ID) != nil, nil
}

// sessionOwnBacklogTarget resolves the session's own project — the project its
// worktree path belongs to — as a backlog target, independent of the command
// target. It returns (nil, nil) when the session has no recorded path or the
// client is talking to a remote daemon (a --daemon-url target's session path is
// on the daemon host and unreadable on this client); callers then fall back to
// the command target's project. When the session has a recorded path but it
// cannot be resolved to a git repository (the worktree was deleted, moved, or is
// unreadable), it returns (nil, err) so the guard fails closed rather than
// consulting an unrelated project a confused agent pointed --repo at.
//
// The resolution runs under backlogGuardRepoProbeTimeout: RepoFromPath's own
// entry point uses an unbounded background context, so a worktree on a stale or
// unreachable mount would block the guard forever before it returns — putting
// every guarded backlog mutation from a dropped-chip or ordinary session on
// that unbounded path. The bounded RepoFromPathContext lookup caps that wait,
// and a timeout or other unanswered probe propagates through the caller's
// fail-closed error path rather than falling back to an unrelated project.
func sessionOwnBacklogTarget(s *session.InstanceData) (*backlogTarget, error) {
	if apiclient.IsRemoteTarget() || s.Path == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), backlogGuardRepoProbeTimeout)
	defer cancel()
	repo, err := config.RepoFromPathContext(ctx, s.Path)
	if err != nil {
		return nil, err
	}
	return &backlogTarget{repoID: repo.ID, name: filepath.Base(repo.Root)}, nil
}

// backlogGuardRepoProbeTimeout bounds the session's own-project resolution the
// guard runs on every guarded backlog mutation. It is a var, not a const, only
// so tests can drive it below git's response time; production never changes it.
// A generous interactive budget (matching the claude login-shell probe) gives a
// healthy resolution room under load while capping a wedged mount that the
// unbounded RepoFromPath entry point would wait on forever.
var backlogGuardRepoProbeTimeout = 5 * time.Second

const backlogSelfAskHint = "ask the user with \"af backlog ask --self\""

// backlogUnreadableItemSessionErr is the fail-closed refusal a guarded mutation
// gets when af cannot read this project's backlog to determine whether the
// session runs a backlog item. It names the real cause (the wrapped read error)
// rather than claiming the session is an item session, so a plain session that
// hits a transient read failure retries rather than sees a misleading
// "item sessions cannot run" error.
func backlogUnreadableItemSessionErr(readErr error) error {
	return fmt.Errorf("cannot read this project's backlog to check for an item session: %w; retry", readErr)
}

// backlogUnresolvableSessionProjectErr is the fail-closed refusal a guarded
// mutation gets when af cannot resolve the session's own project to determine
// whether it runs a backlog item (its recorded worktree was deleted, moved, or
// is unreadable). Like the unreadable-backlog refusal it names the real cause,
// not "item sessions cannot run", so a plain session retries; and it fails
// closed rather than falling back to an unrelated project a confused agent
// pointed --repo at.
func backlogUnresolvableSessionProjectErr(resolveErr error) error {
	return fmt.Errorf("cannot resolve this session's own project to check for an item session: %w; retry", resolveErr)
}

// backlogUnresolvableFallbackProjectErr is the fail-closed refusal a guarded
// mutation gets when the session has no recorded path (or the client is talking
// to a remote daemon) and the command target's project — the fallback the guard
// consults in that case — cannot be resolved either (no --repo and the cwd is
// not a git repository). The global budget command does not re-resolve a
// project before reaching SetBudget, so letting this fall through as a non-item
// session would pass the guard; the guard fails closed and names the fallback
// resolution failure rather than claiming an item session.
func backlogUnresolvableFallbackProjectErr(resolveErr error) error {
	return fmt.Errorf("cannot resolve the command target's project to check for an item session: %w; retry", resolveErr)
}

// refuseFromItemSession stops an agent in an item session from running a
// command that changes the backlog's plan. It stops a confused agent, not a
// hostile one. When the item-session membership cannot be determined because
// the authoritative backlog read or the session's own project resolution
// failed, it refuses with the real cause (already wrapped by
// itemSessionFromAuthoritativeLink) rather than letting the mutation through.
func refuseFromItemSession(command string) error {
	s, err := inItemSession()
	if err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	return fmt.Errorf("item sessions cannot run \"af backlog %s\"; %s", command, backlogSelfAskHint)
}

func refuseControlsFromItemSession() error {
	s, err := inItemSession()
	if err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	return errors.New("item sessions cannot change the runner or budget; " + backlogSelfAskHint)
}

var backlogStartCmd = &cobra.Command{
	Use:   "start <id>",
	Short: "Start an item now",
	Long: "Start the item now, ignoring the runner switch and the budget. An item with a live " +
		"session gets \"continue\" in that session; any other item gets a new session on a new " +
		"branch from the default branch. The command returns at once; the run shows starting " +
		"until the session exists.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			if err := refuseFromItemSession("start"); err != nil {
				return nil, err
			}
			id, err := backlog.ParseID(args[0])
			if err != nil {
				return nil, err
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			var resp daemon.StartBacklogItemResponse
			if err := callBacklogRoute("StartBacklogItem", daemon.StartBacklogItemRequest{
				RepoID: t.repoID, RepoPath: t.repoPath, ID: id,
			}, &resp); err != nil {
				return nil, err
			}
			return resp.Run, nil
		})
	},
}

var backlogRunCount int

var backlogRunCmd = &cobra.Command{
	Use:   "run [--count N]",
	Short: "Run one runner pass now",
	Long: "Start up to N queued items now (default 1), top first, in the project's free slots. " +
		"It ignores the runner switch and the budget, so an af cron task can drive the runner " +
		"with it. Prints the ids it started, and why when it started none.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			if err := refuseFromItemSession("run"); err != nil {
				return nil, err
			}
			if backlogRunCount < 1 {
				return nil, errors.New("--count must be 1 or more")
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			var resp daemon.RunBacklogResponse
			if err := callBacklogRoute("RunBacklog", daemon.RunBacklogRequest{
				RepoID: t.repoID, RepoPath: t.repoPath, Count: backlogRunCount,
			}, &resp); err != nil {
				return nil, err
			}
			return resp, nil
		})
	},
}

var (
	backlogDoneSelf bool
	backlogDonePR   string
)

var backlogDoneCmd = &cobra.Command{
	Use:   "done <id> | done --self [--pr <url>]",
	Short: "Mark an item done after review, or, from an item session, send it to review",
	Long: "With an id, mark the item done. Items that wait on it can then start. Open questions " +
		"on it close; its session stays.\n\n" +
		"With --self, run from inside an item session: the agent's work is finished and the " +
		"item goes to review for the user. --pr stores the pull request link.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			if backlogDoneSelf {
				if len(args) > 0 {
					return nil, errors.New("--self names the item of this session; do not pass an id with it")
				}
				sessionID, it, err := t.selfItem()
				if err != nil {
					return nil, err
				}
				return setBacklogStatus(daemon.SetBacklogItemStatusRequest{RepoID: t.repoID, RepoPath: t.repoPath,
					ID: it.ID, Status: backlog.StatusReview, SessionID: sessionID, PR: backlogDonePR})
			}
			if len(args) != 1 {
				return nil, errors.New("af backlog done needs an id, or --self from inside an item session")
			}
			if backlogDonePR != "" {
				return nil, errors.New("--pr goes with --self")
			}
			if err := refuseFromItemSession("done <id>"); err != nil {
				return nil, err
			}
			id, err := backlog.ParseID(args[0])
			if err != nil {
				return nil, err
			}
			return setBacklogStatus(daemon.SetBacklogItemStatusRequest{RepoID: t.repoID, RepoPath: t.repoPath,
				ID: id, Status: backlog.StatusDone})
		})
	},
}

func setBacklogStatus(req daemon.SetBacklogItemStatusRequest) (any, error) {
	var resp daemon.SetBacklogItemStatusResponse
	if err := callBacklogRoute("SetBacklogItemStatus", req, &resp); err != nil {
		return nil, err
	}
	return resp.Item, nil
}

var backlogNoteSelf bool

var backlogNoteCmd = &cobra.Command{
	Use:   "note <id> <text> | note --self <text>",
	Short: "Add a note to an item's thread",
	Long:  "Add a note to the item's thread. A note is not delivered to the item's session. With --self, the item is the one of the session this runs in.",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			if backlogNoteSelf {
				sessionID, it, err := t.selfItem()
				if err != nil {
					return nil, err
				}
				return postBacklogMessage(daemon.PostBacklogMessageRequest{RepoID: t.repoID, RepoPath: t.repoPath,
					ID: it.ID, Kind: backlog.KindNote, Text: strings.Join(args, " "), SessionID: sessionID})
			}
			if len(args) < 2 {
				return nil, errors.New("af backlog note needs an id and text, or --self and text")
			}
			if err := refuseFromItemSession("note <id>"); err != nil {
				return nil, err
			}
			id, err := backlog.ParseID(args[0])
			if err != nil {
				return nil, err
			}
			return postBacklogMessage(daemon.PostBacklogMessageRequest{RepoID: t.repoID, RepoPath: t.repoPath,
				ID: id, Kind: backlog.KindNote, Text: strings.Join(args[1:], " ")})
		})
	},
}

func postBacklogMessage(req daemon.PostBacklogMessageRequest) (any, error) {
	var resp daemon.PostBacklogMessageResponse
	if err := callBacklogRoute("PostBacklogMessage", req, &resp); err != nil {
		return nil, err
	}
	return resp.Message, nil
}

var (
	backlogAskSelf        bool
	backlogAskThen        string
	backlogAskHandoffFile string
)

var backlogAskCmd = &cobra.Command{
	Use:   "ask --self <text> [--then wait|archive] [--handoff-file <path|->]",
	Short: "From an item session, ask the user a question",
	Long: "Run from inside an item session: add a question to the item's thread and set the " +
		"session's attention to it. The item shows Needs you until the user answers. Stop " +
		"working after you ask.\n\n" +
		"--then says what happens to this session once you stop. archive, the default: af " +
		"archives it, and the user's answer restores it, with this conversation, and arrives " +
		"here. wait: the session stays live until the answer arrives. af archives only a session " +
		"it can restore into its own conversation (today a Claude, Codex or Amp session with a " +
		"recorded conversation id); any other stays live, as with wait. If it keeps the session " +
		"live, or someone works in the session after you stop, af notes why on the item.\n\n" +
		"--handoff-file stores notes for a later run of the item (at most 8 KB) with the " +
		"question, read from a file or from stdin with -. The user sees them in \"af backlog inbox\".",
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			if !backlogAskSelf {
				return nil, errors.New("af backlog ask needs --self; an agent asks from inside its item session")
			}
			if backlogAskThen != backlog.ThenWait && backlogAskThen != backlog.ThenArchive {
				return nil, fmt.Errorf("--then is %q; use %s or %s", backlogAskThen, backlog.ThenWait, backlog.ThenArchive)
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			sessionID, it, err := t.selfItem()
			if err != nil {
				return nil, err
			}
			handoff := ""
			if backlogAskHandoffFile != "" {
				if handoff, err = readFileOrStdin(cmd, backlogAskHandoffFile); err != nil {
					return nil, fmt.Errorf("read --handoff-file: %w", err)
				}
			}
			return postBacklogMessage(daemon.PostBacklogMessageRequest{RepoID: t.repoID, RepoPath: t.repoPath,
				ID: it.ID, Kind: backlog.KindQuestion, Text: strings.Join(args, " "), SessionID: sessionID,
				Then: backlogAskThen, Handoff: handoff})
		})
	},
}

var backlogAnswerQuestion int

var backlogAnswerCmd = &cobra.Command{
	Use:   "answer <id> [--question <n>] <text>",
	Short: "Answer an item's open question",
	Long: "Answer the item's oldest open question, or the one --question names. af adds the " +
		"answer to the thread and sends it to the item's session, restoring the session first " +
		"if it is archived, and waiting while a client is attached to it. If the session is " +
		"gone, af starts a new session for the item whose first prompt carries the previous " +
		"agent's notes, the question and the answer; the question closes when that session " +
		"starts. A done or dropped item must be reopened first.",
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			if err := refuseFromItemSession("answer"); err != nil {
				return nil, err
			}
			id, err := backlog.ParseID(args[0])
			if err != nil {
				return nil, err
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			return postBacklogMessage(daemon.PostBacklogMessageRequest{RepoID: t.repoID, RepoPath: t.repoPath,
				ID: id, Kind: backlog.KindAnswer, Text: strings.Join(args[1:], " "), QuestionID: backlogAnswerQuestion})
		})
	},
}

func init() {
	backlogAnswerCmd.Flags().IntVar(&backlogAnswerQuestion, "question", 0, "The `id` of the question to answer, from \"af backlog show\"; default the oldest open one")
	backlogAskCmd.Flags().BoolVar(&backlogAskSelf, "self", false, "Ask on the item of the session this runs in")
	backlogAskCmd.Flags().StringVar(&backlogAskThen, "then", backlog.ThenArchive, "What happens to this session once you stop: archive (the answer restores it) or wait (it stays live)")
	backlogAskCmd.Flags().StringVar(&backlogAskHandoffFile, "handoff-file", "", "A `path` to notes for a later run of the item, or - for stdin (at most 8 KB)")
	backlogDoneCmd.Flags().BoolVar(&backlogDoneSelf, "self", false, "From inside an item session: the work is finished; send the item to review")
	backlogDoneCmd.Flags().StringVar(&backlogDonePR, "pr", "", "With --self, the `url` of the pull request for the work")
	backlogNoteCmd.Flags().BoolVar(&backlogNoteSelf, "self", false, "Note on the item of the session this runs in")
	backlogRunCmd.Flags().IntVar(&backlogRunCount, "count", 1, "How many items to start at most")
	BacklogCmd.AddCommand(backlogStartCmd, backlogRunCmd, backlogDoneCmd, backlogNoteCmd, backlogAskCmd, backlogAnswerCmd)
}
