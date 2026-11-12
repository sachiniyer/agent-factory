package api

import (
	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// setConversationViaDaemon is the same daemon action the TUI and web would
// reach over HTTP: the daemon validates the id against the session's own
// provider store before writing, so the transport is the only difference.
var setConversationViaDaemon = routedSetConversation

var sessionsSetConversationCmd = &cobra.Command{
	Use:   "set-conversation <title> <id>",
	Short: "Record a session's provider conversation id",
	Long: `Record a provider conversation id on a session's Agent tab — the repair for
a conversation af never captured, or captured stale before the store had a
capture arm on it (#6769).

The daemon refuses unless the session's own conversation store proves the id
belongs to it: today that means a Codex rollout-…-<uuid>.jsonl whose
session_meta launches in the session's working directory and is not a
sub-agent thread. A remote session, a non-Codex agent, a malformed or absent
rollout, and a session mid-operation all refuse with the reason.

The running pane is not touched: the recorded id only matters the next time af
builds a resume command (respawn, restore, handoff). The recorded id stands
while the pane stays on the thread it was on when you set it, and it outranks
the other threads already in the session's worktree.

Once the pane moves to another thread, the id no longer names the thread you
were last in. af records the thread the pane moved to (#7120); if it could not
see the move, the next restore or handoff starts a fresh conversation rather
than resume the id (#7122). A move is a new thread (/new, or the first prompt
of a fresh Codex, which is on no thread until then) or an older thread the
pane resumes. A pane af starts after you set the id was on no thread when you
set it, so any thread it opens is a move, even the one the old pane was on
(#7129). If af could not read which thread the pane was on as you set the id,
the thread it is on the first time af can read it stands in for that, and
any other thread written to since you set the id then counts as a move;
until af has read it, an older thread the pane resumes does not. So set the
id after a fresh pane's first prompt, not before. Re-running with the same
id renews the repair as of now.

Example:
  af sessions set-conversation fix-auth 0195f8e2-7c3a-7b1d-9e4f-2a6c8d0b1e2f`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		scope, err := resolveLookupScope()
		if err != nil {
			return jsonError(err)
		}

		title, conversationID := args[0], args[1]
		err = setConversationViaDaemon(daemon.SetConversationRequest{
			Title:          title,
			RepoID:         scope.RepoID,
			ConversationID: conversationID,
		})
		warning := ""
		if err != nil && apiclient.IsMutationCommitted(err) {
			warning = err.Error()
		} else if err != nil {
			return jsonError(scope.nameMiss(title, err))
		}

		output := map[string]any{"ok": true, "title": title, "conversation_id": conversationID}
		if warning != "" {
			output["warning"] = warning
		}
		return jsonOut(output)
	},
}
