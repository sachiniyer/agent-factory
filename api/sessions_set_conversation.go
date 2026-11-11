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

The running pane is NOT touched: the recorded id only matters the next time af
builds a resume command (respawn, restore, handoff). Re-running with the same
id is a no-op success.

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
