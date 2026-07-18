package api

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var (
	sessionsHandoffTo    string
	sessionsHandoffBrief string
)

// handoffSessionViaDaemon is the daemon seam, matching the other session verbs
// in api/sessions.go so tests can substitute it.
var handoffSessionViaDaemon = daemon.HandoffSession

var sessionsHandoffCmd = &cobra.Command{
	Use:   "handoff <title>",
	Short: "Continue a session under a different agent, in place",
	Long: `Hand a session's work over to a different agent without losing it.

The session keeps its identity, its git worktree, and its branch — only the
agent process changes. The incoming agent starts a fresh conversation and is
given a mission brief: the session's goal, and what is already on the branch.

This is the answer to an agent that has stopped and cannot continue — most often
one blocked at its provider's usage limit, where the alternative is waiting for
the window to reset (see 'af sessions list' for a [limit] badge, and
docs/usage-limits.md for the waiting path).

Agent conversations are not portable between providers: the incoming agent
cannot read what its predecessor was thinking, only the working tree and the git
history. The brief points it at both. Because of that, a handoff is recorded —
the swap and the branch tip at the moment it happened — so a reviewer reading
the resulting diff can tell which agent wrote which part.

Local-worktree sessions only: swapping the agent inside a remote/docker/ssh
sandbox is a different lifecycle and is not supported yet.

Examples:
  af sessions handoff fix-auth --to claude
  af sessions handoff fix-auth --to gemini --brief "finish the retry test, skip the docs"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		title := args[0]
		if sessionsHandoffTo == "" {
			return jsonError(fmt.Errorf("--to is required: name the agent to hand off to (one of %s)", tmux.SupportedProgramsString()))
		}
		if !tmux.IsSupportedProgram(sessionsHandoffTo) {
			return jsonError(fmt.Errorf("unknown agent %q: --to must be one of %s", sessionsHandoffTo, tmux.SupportedProgramsString()))
		}

		// Honor --repo scoping the way kill/archive do: an empty repoID keeps the
		// all-repo search, a non-empty one confines the swap to that repo so a
		// same-titled session elsewhere is never touched.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		resp, err := handoffSessionViaDaemon(daemon.HandoffSessionRequest{
			Title:  title,
			RepoID: repoID,
			To:     sessionsHandoffTo,
			Brief:  sessionsHandoffBrief,
		})
		if err != nil {
			return jsonError(err)
		}

		return jsonOut(map[string]any{
			"ok":       true,
			"title":    title,
			"from":     resp.From,
			"to":       resp.To,
			"head_sha": resp.HeadSHA,
		})
	},
}
