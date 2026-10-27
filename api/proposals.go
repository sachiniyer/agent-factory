package api

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

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

type proposalInfo struct {
	Slug             string   `json:"slug"`
	Path             string   `json:"path"`
	Docs             []string `json:"docs"`
	AttachedSessions []string `json:"attached_sessions"`
	LastEdit         string   `json:"last_edit,omitempty"`
	Rules            string   `json:"rules,omitempty"`
}

func describeProposal(c *proposalCaller, slug string, withRules bool) (*proposalInfo, error) {
	docs, err := c.store.Docs(slug)
	if err != nil {
		return nil, err
	}
	attachments, err := c.store.Attachments()
	if err != nil {
		return nil, err
	}
	var ids []string
	for id, s := range attachments {
		if s == slug {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	info := &proposalInfo{
		Slug:             slug,
		Path:             c.store.SlugPath(slug),
		Docs:             docs,
		AttachedSessions: sessionTitles(c.repoID, ids),
	}
	if withRules {
		info.Rules = proposal.Rules(slug, info.Path)
	}
	return info, nil
}

func runProposal(fn func() (any, error)) error {
	log.Initialize(false)
	defer log.Close()
	if err := proposalPreflight(); err != nil {
		return jsonError(err)
	}
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
		"<AF_HOME>/templates/proposal/<name>.md override the built-in ones.",
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
			if err := c.store.Create(args[0], proposalStartDocsFlag, c.committer()); err != nil {
				return nil, err
			}
			if err := c.store.Attach(args[0], c.sessionID); err != nil {
				return nil, err
			}
			return describeProposal(c, args[0], true)
		})
	},
}

var proposalAttachCmd = &cobra.Command{
	Use:   "attach [<slug>]",
	Short: "Attach this session to a proposal and print the rules",
	Long: "Attach the session to an existing proposal and print the rules. A session holds one " +
		"proposal at a time, so this detaches it from any other. With no slug, print the current " +
		"proposal and the rules again.",
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
				if err := c.store.Attach(slug, c.sessionID); err != nil {
					return nil, err
				}
			} else {
				if slug, err = c.store.Attached(c.sessionID); err != nil {
					return nil, err
				}
				if slug == "" {
					return nil, notAttachedError(c)
				}
			}
			return describeProposal(c, slug, true)
		})
	},
}

func notAttachedError(c *proposalCaller) error {
	slugs, _ := c.store.Slugs()
	if len(slugs) == 0 {
		return proposal.ErrNotAttached
	}
	return fmt.Errorf("%w; existing proposals: %s", proposal.ErrNotAttached, strings.Join(slugs, ", "))
}

var proposalDetachCmd = &cobra.Command{
	Use:   "detach",
	Short: "Remove this session's proposal attachment and cursor",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := requireProposalSession("detach")
			if err != nil {
				return nil, err
			}
			slug, err := c.store.Detach(c.sessionID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"slug": slug, "detached": slug != ""}, nil
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
			slugs, err := c.store.Slugs()
			if err != nil {
				return nil, err
			}
			out := []proposalInfo{}
			edits := map[string]time.Time{}
			for _, slug := range slugs {
				info, err := describeProposal(c, slug, false)
				if err != nil {
					return nil, err
				}
				at, err := c.store.LastEdit(slug)
				if err != nil {
					return nil, err
				}
				edits[slug] = at
				info.LastEdit = at.Format(time.RFC3339)
				out = append(out, *info)
			}
			sort.SliceStable(out, func(i, j int) bool { return edits[out[i].Slug].After(edits[out[j].Slug]) })
			return out, nil
		})
	},
}

var proposalPathCmd = &cobra.Command{
	Use:   "path <slug>",
	Short: "Print the absolute folder path of a proposal",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := proposalProject()
			if err != nil {
				return nil, err
			}
			if err := c.store.RequireSlug(args[0]); err != nil {
				return nil, err
			}
			return map[string]string{"slug": args[0], "path": c.store.SlugPath(args[0])}, nil
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
			file, err := c.store.AddDoc(args[0], args[1], c.committer())
			if err != nil {
				return nil, err
			}
			return describeProposalDoc(c, args[0], file), nil
		})
	},
}

func describeProposalDoc(c *proposalCaller, slug, file string) map[string]string {
	return map[string]string{"slug": slug, "doc": file, "path": filepath.Join(c.store.SlugPath(slug), file)}
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
			return c.store.Changes(c.sessionID)
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
			return c.store.Checkpoint(c.sessionID)
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
			text, file, err := c.store.Read(args[0], args[1])
			if err != nil {
				return nil, err
			}
			return map[string]string{"slug": args[0], "doc": file, "text": text}, nil
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
			by := c.committer()
			file, commit, err := c.store.Write(args[0], args[1], string(text), by)
			if err != nil {
				return nil, err
			}
			out := describeProposalDoc(c, args[0], file)
			out["commit"] = commit
			out["author"] = string(by.Author)
			return out, nil
		})
	},
}

func init() {
	const repoFlagUsage = "Path to the project's git repository (default: the current directory's project)"
	ProposalCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", repoFlagUsage)
	ProposalCmd.PersistentFlags().BoolVar(&envelopeOutput, "json", false, "Wrap output in the {data,error} JSON envelope (default: bare payload)")

	proposalStartCmd.Flags().StringSliceVar(&proposalStartDocsFlag, "docs", append([]string(nil), proposal.BuiltinTemplates...), "Docs to create, comma-separated")
	for _, c := range []*cobra.Command{proposalStartCmd, proposalAttachCmd, proposalDetachCmd, proposalChangesCmd, proposalCheckpointCmd, proposalAddDocCmd, proposalWriteCmd} {
		c.Flags().StringVar(&proposalSessionFlag, "session", "", "Act for the session with this `title` instead of the calling session")
	}
	ProposalCmd.AddCommand(proposalStartCmd, proposalAttachCmd, proposalDetachCmd, proposalListCmd,
		proposalPathCmd, proposalAddDocCmd, proposalChangesCmd, proposalCheckpointCmd, proposalCatCmd, proposalWriteCmd)
}
