package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/backlog"
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
func inItemSession() *session.InstanceData {
	s, err := backlogSelfSession()
	if err != nil || s == nil || s.BacklogItem == 0 {
		return nil
	}
	return s
}

const backlogSelfAskHint = "ask the user with \"af backlog ask --self\""

// refuseFromItemSession stops an agent in an item session from running a
// command that changes the backlog's plan. It stops a confused agent, not a
// hostile one.
func refuseFromItemSession(command string) error {
	if inItemSession() == nil {
		return nil
	}
	return fmt.Errorf("item sessions cannot run \"af backlog %s\"; %s", command, backlogSelfAskHint)
}

func refuseControlsFromItemSession() error {
	if inItemSession() == nil {
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
		"here. wait: the session stays live until the answer arrives. If someone works in the " +
		"session after you stop, af keeps it live and notes why on the item.\n\n" +
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
		"gone, the answer stays in the thread.",
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
