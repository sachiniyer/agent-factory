package api

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/daemon"
)

// orchestratorSelf is the id of the session this command runs in, which must
// not be an item session.
func orchestratorSelf() (string, error) {
	if inItemSession() != nil {
		return "", backlog.ErrOrchestrateFromItemSession
	}
	s, err := backlogSelfSession()
	if err != nil {
		return "", fmt.Errorf("--self needs an af session; run it from inside the session that should orchestrate: %w", err)
	}
	return s.ID, nil
}

var (
	backlogOrchestrateSelf        bool
	backlogOrchestrateClear       bool
	backlogOrchestrateHandoffFile string
	backlogOrchestrateReplace     bool
)

var backlogOrchestrateCmd = &cobra.Command{
	Use:   "orchestrate <group> --self [--replace] | <group> --clear | --self --handoff-file <path|->",
	Short: "Bind this session as a group's orchestrator, or unbind it",
	Long: "With a group and --self, bind the session this runs in as the orchestrator of the " +
		"group, a top-level parent. A session orchestrates at most one group, and an item " +
		"session cannot orchestrate. A group another session orchestrates is refused unless " +
		"--replace; the new session keeps its handoff notes. With --clear, unbind the group.\n\n" +
		"--handoff-file stores notes for a session that replaces this one as orchestrator (at " +
		"most 8 KB), read from a file or from stdin with -. Without a group it updates the notes " +
		"of the group this session orchestrates.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			if backlogOrchestrateSelf == backlogOrchestrateClear {
				return nil, errors.New("af backlog orchestrate needs one of --self or --clear")
			}
			if backlogOrchestrateClear {
				if len(args) != 1 {
					return nil, errors.New("--clear needs the group's id")
				}
				if backlogOrchestrateHandoffFile != "" || backlogOrchestrateReplace {
					return nil, errors.New("--handoff-file and --replace go with --self")
				}
				if err := refuseFromItemSession("orchestrate"); err != nil {
					return nil, err
				}
			}
			if len(args) == 0 && backlogOrchestrateHandoffFile == "" {
				return nil, errors.New("af backlog orchestrate --self needs a group id, or --handoff-file to update this session's notes")
			}
			if len(args) == 0 && backlogOrchestrateReplace {
				return nil, errors.New("--replace needs the group's id")
			}
			id := 0
			if len(args) == 1 {
				var err error
				if id, err = backlog.ParseID(args[0]); err != nil {
					return nil, err
				}
			}
			handoff := ""
			if backlogOrchestrateHandoffFile != "" {
				var err error
				if handoff, err = readFileOrStdin(cmd, backlogOrchestrateHandoffFile); err != nil {
					return nil, fmt.Errorf("read --handoff-file: %w", err)
				}
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			if backlogOrchestrateClear {
				return setBacklogOrchestrator(daemon.SetBacklogOrchestratorRequest{RepoID: t.repoID, RepoPath: t.repoPath, ID: id, Clear: true})
			}
			sessionID, err := orchestratorSelf()
			if err != nil {
				return nil, err
			}
			if id != 0 {
				var notes *string
				if backlogOrchestrateHandoffFile != "" {
					notes = &handoff
				}
				return setBacklogOrchestrator(daemon.SetBacklogOrchestratorRequest{RepoID: t.repoID, RepoPath: t.repoPath, ID: id,
					SessionID: sessionID, Replace: backlogOrchestrateReplace, Handoff: notes})
			}
			var resp daemon.SetBacklogOrchestratorHandoffResponse
			if err := callBacklogRoute("SetBacklogOrchestratorHandoff", daemon.SetBacklogOrchestratorHandoffRequest{
				RepoID: t.repoID, RepoPath: t.repoPath, SessionID: sessionID, Handoff: handoff}, &resp); err != nil {
				return nil, err
			}
			return &resp, nil
		})
	},
}

func setBacklogOrchestrator(req daemon.SetBacklogOrchestratorRequest) (*backlog.OrchestratorResponse, error) {
	var resp daemon.SetBacklogOrchestratorResponse
	if err := callBacklogRoute("SetBacklogOrchestrator", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func init() {
	f := backlogOrchestrateCmd.Flags()
	f.BoolVar(&backlogOrchestrateSelf, "self", false, "Bind the session this runs in")
	f.BoolVar(&backlogOrchestrateClear, "clear", false, "Unbind the group's orchestrator")
	f.BoolVar(&backlogOrchestrateReplace, "replace", false, "With --self, take the group over from the session that orchestrates it")
	f.StringVar(&backlogOrchestrateHandoffFile, "handoff-file", "", "With --self, a `path` to notes for a session that replaces this one, or - for stdin")
	BacklogCmd.AddCommand(backlogOrchestrateCmd)
}
