package api

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/proposal"
)

// ProposalCmd is the top-level command group for proposals.
var ProposalCmd = &cobra.Command{
	Use:   "proposal",
	Short: "Write proposals together with agents",
	Long: "A proposal is a folder of markdown docs that belongs to the project. The user and " +
		"agent sessions edit the docs with their normal tools; af records each edit, and " +
		"\"af proposal changes\" shows a session what others changed since it last looked.\n\n" +
		"Commands that need a session use the calling af session, or --session <title>. The " +
		"others follow the shared project contract: --repo names a project, otherwise the " +
		"current directory's project is used.",
}

func runProposal(fn func() (any, error)) error {
	log.Initialize(false)
	defer log.Close()
	out, err := fn()
	if err != nil {
		return jsonError(err)
	}
	return jsonOut(out)
}

var proposalStartDocsFlag []string

var proposalStartCmd = &cobra.Command{
	Use:   "start <slug>",
	Short: "Create a proposal from templates, attach this session, and print the rules",
	Long: "Create a proposal from templates, attach the session to it, and print the rules the " +
		"agent follows. Fails when the slug exists. A slug is 1-63 lowercase letters, digits and " +
		"dashes. A doc with no template starts blank; user templates in " +
		"<AF_HOME>/templates/proposal/<name>.md override the built-in ones. The session gets a " +
		"Proposal tab, and \"tab\" names it.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			if err := proposal.ValidateSlug(args[0]); err != nil {
				return nil, err
			}
			c, err := requireProposalSession("start")
			if err != nil {
				return nil, err
			}
			var resp daemon.StartProposalResponse
			err = callProposalRoute("StartProposal", daemon.StartProposalRequest{
				SessionID: c.sessionID, Slug: args[0], Docs: proposalStartDocsFlag,
			}, &resp)
			return resp, err
		})
	},
}

var proposalAttachCmd = &cobra.Command{
	Use:   "attach [<slug>]",
	Short: "Attach this session to a proposal and print the rules",
	Long: "Attach the session to an existing proposal and print the rules. A session holds one " +
		"proposal at a time, so this detaches it from any other. With no slug, print the current " +
		"proposal and the rules again. The session gets a Proposal tab when it has " +
		"none, and \"tab\" names it.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := requireProposalSession("attach")
			if err != nil {
				return nil, err
			}
			slug := ""
			if len(args) == 1 {
				slug = args[0]
			}
			req := daemon.AttachProposalRequest{SessionID: c.sessionID, Slug: slug}
			var resp daemon.AttachProposalResponse
			err = callProposalRoute("AttachProposal", req, &resp)
			return resp, err
		})
	},
}

var proposalDetachCmd = &cobra.Command{
	Use:   "detach",
	Short: "Remove this session's proposal attachment and cursor",
	Long:  "Remove the session's proposal attachment and cursor, and close its Proposal tab.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := requireProposalSession("detach")
			if err != nil {
				return nil, err
			}
			var resp daemon.DetachProposalResponse
			if err := callProposalRoute("DetachProposal", daemon.DetachProposalRequest{SessionID: c.sessionID}, &resp); err != nil {
				return nil, err
			}
			return map[string]any{"slug": resp.Slug, "detached": resp.Detached}, nil
		})
	},
}

var proposalListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the project's proposals, newest edit first",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := proposalProject()
			if err != nil {
				return nil, err
			}
			var resp daemon.ListProposalsResponse
			if err := callProposalRoute("ListProposals", daemon.ListProposalsRequest{
				SessionID: c.sessionID, RepoID: c.repoID, RepoPath: c.repoPath,
			}, &resp); err != nil {
				return nil, err
			}
			if resp.Proposals == nil {
				resp.Proposals = []proposal.Summary{}
			}
			return resp.Proposals, nil
		})
	},
}

var proposalPathCmd = &cobra.Command{
	Use:   "path <slug>",
	Short: "Print the absolute folder path of a proposal",
	Long:  "Print the absolute folder path of a proposal. With --daemon-url, the path is on the daemon host.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := proposalProject()
			if err != nil {
				return nil, err
			}
			var resp daemon.GetProposalResponse
			if err := callProposalRoute("GetProposal", daemon.GetProposalRequest{
				SessionID: c.sessionID, RepoID: c.repoID, RepoPath: c.repoPath, Slug: args[0],
			}, &resp); err != nil {
				return nil, err
			}
			return map[string]string{"slug": resp.Slug, "path": resp.Path}, nil
		})
	},
}

var proposalAddDocCmd = &cobra.Command{
	Use:   "add-doc <slug> <name>",
	Short: "Add a doc to a proposal from a template, or blank",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := optionalProposalSession()
			if err != nil {
				return nil, err
			}
			var resp daemon.AddProposalDocResponse
			if err := callProposalRoute("AddProposalDoc", daemon.AddProposalDocRequest{
				SessionID: c.sessionID, RepoID: c.repoID, RepoPath: c.repoPath,
				Slug: args[0], Name: args[1], Author: string(c.author()),
			}, &resp); err != nil {
				return nil, err
			}
			return map[string]string{"slug": resp.Slug, "doc": resp.Doc, "path": resp.Path}, nil
		})
	},
}

var proposalRemoveDocCmd = &cobra.Command{
	Use:   "remove-doc <slug> <doc>",
	Short: "Remove a doc from a proposal; it stays in the proposal's history",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := optionalProposalSession()
			if err != nil {
				return nil, err
			}
			var resp daemon.RemoveProposalDocResponse
			if err := callProposalRoute("RemoveProposalDoc", daemon.RemoveProposalDocRequest{
				SessionID: c.sessionID, RepoID: c.repoID, RepoPath: c.repoPath,
				Slug: args[0], Doc: args[1], Author: string(c.author()),
			}, &resp); err != nil {
				return nil, err
			}
			return map[string]string{"slug": resp.Slug, "doc": resp.Doc, "path": resp.Path}, nil
		})
	},
}

var proposalChangesCmd = &cobra.Command{
	Use:   "changes",
	Short: "Print what others changed since this session's cursor, then move the cursor",
	Long: "Print every edit to this session's proposal since its cursor that the session did " +
		"not checkpoint itself — edits by the user, by other editors, and by other sessions — " +
		"then move the cursor. An empty edits array means nobody else changed the proposal.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := requireProposalSession("changes")
			if err != nil {
				return nil, err
			}
			var resp daemon.PullProposalChangesResponse
			if err := callProposalRoute("PullProposalChanges", daemon.PullProposalChangesRequest{SessionID: c.sessionID}, &resp); err != nil {
				return nil, err
			}
			if resp.Edits == nil {
				resp.Edits = []proposal.Edit{}
			}
			return resp, nil
		})
	},
}

var proposalCheckpointCmd = &cobra.Command{
	Use:   "checkpoint",
	Short: "Commit this session's edits to its proposal and move its cursor",
	Long: "Commit the uncommitted edits in this session's proposal as the session's own, so " +
		"\"af proposal changes\" does not show them back to it. The cursor does not move past " +
		"edits by others that changes has not shown yet; unseen counts them.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := requireProposalSession("checkpoint")
			if err != nil {
				return nil, err
			}
			var resp daemon.CheckpointProposalResponse
			err = callProposalRoute("CheckpointProposal", daemon.CheckpointProposalRequest{SessionID: c.sessionID}, &resp)
			return resp, err
		})
	},
}

var proposalCatCmd = &cobra.Command{
	Use:   "cat <slug> <doc>",
	Short: "Print a doc of a proposal",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := proposalProject()
			if err != nil {
				return nil, err
			}
			var resp daemon.ReadProposalDocResponse
			if err := callProposalRoute("ReadProposalDoc", daemon.ReadProposalDocRequest{
				SessionID: c.sessionID, RepoID: c.repoID, RepoPath: c.repoPath, Slug: args[0], Doc: args[1],
			}, &resp); err != nil {
				return nil, err
			}
			return map[string]string{"slug": resp.Slug, "doc": resp.Doc, "text": resp.Text}, nil
		})
	},
}

var proposalWriteCmd = &cobra.Command{
	Use:   "write <slug> <doc>",
	Short: "Replace a doc with stdin and commit it",
	Long: "Replace an existing doc with stdin and commit it: as this session's own edit inside " +
		"an af session or with --session, otherwise as an external edit.",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := optionalProposalSession()
			if err != nil {
				return nil, err
			}
			text, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return nil, fmt.Errorf("read stdin: %w", err)
			}
			var resp daemon.WriteProposalDocResponse
			if err := callProposalRoute("WriteProposalDoc", daemon.WriteProposalDocRequest{
				SessionID: c.sessionID, RepoID: c.repoID, RepoPath: c.repoPath,
				Slug: args[0], Doc: args[1], Text: string(text), Author: string(c.author()),
			}, &resp); err != nil {
				return nil, err
			}
			return map[string]string{
				"slug": resp.Slug, "doc": resp.Doc, "path": resp.Path,
				"commit": resp.Commit, "author": string(resp.Author),
			}, nil
		})
	},
}

func init() {
	const repoFlagUsage = "Path to the project's git repository (default: the current directory's project)"
	ProposalCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", repoFlagUsage)
	ProposalCmd.PersistentFlags().BoolVar(&envelopeOutput, "json", false, "Wrap output in the {data,error} JSON envelope (default: bare payload)")

	proposalStartCmd.Flags().StringSliceVar(&proposalStartDocsFlag, "docs", append([]string(nil), proposal.BuiltinTemplates...), "Docs to create, comma-separated")
	for _, c := range []*cobra.Command{proposalStartCmd, proposalAttachCmd, proposalDetachCmd, proposalChangesCmd, proposalCheckpointCmd, proposalAddDocCmd, proposalRemoveDocCmd, proposalWriteCmd} {
		c.Flags().StringVar(&proposalSessionFlag, "session", "", "Act for the session with this `title` instead of the calling session")
	}
	ProposalCmd.AddCommand(proposalStartCmd, proposalAttachCmd, proposalDetachCmd, proposalListCmd,
		proposalPathCmd, proposalAddDocCmd, proposalRemoveDocCmd, proposalChangesCmd, proposalCheckpointCmd, proposalCatCmd, proposalWriteCmd)
}
