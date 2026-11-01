package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// BacklogCmd is the top-level command group for the backlog.
var BacklogCmd = &cobra.Command{
	Use:   "backlog",
	Short: "Keep an ordered backlog of work items for a project",
	Long: "The backlog is one project's work items in a strict order. Items can have children " +
		"and can wait on other items. New items are drafts; queue an item to make it eligible " +
		"for the runner, and mark it done after review.\n\n" +
		"Item ids are accepted as 12 or B-12. Commands follow the shared project contract: " +
		"--repo names a project, otherwise the current directory's project is used.",
}

// callBacklogRoute sends one backlog route to the targeted daemon: the local
// one over the control socket (started when needed), or the --daemon-url one
// over HTTP.
var callBacklogRoute = func(method string, req, resp any) error {
	if !apiclient.IsRemoteTarget() {
		return daemon.CallBacklog(method, req, resp)
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	return client.CallBacklog(method, req, resp)
}

func runBacklog(fn func() (any, error)) error {
	log.Initialize(false)
	defer log.Close()
	out, err := fn()
	if err != nil {
		return jsonError(err)
	}
	return jsonOut(out)
}

// backlogTarget is the project an af backlog command acts on: a repo id for
// the local daemon, or a path on the host of a remote one.
type backlogTarget struct {
	repoID   string
	repoPath string
	name     string
}

func backlogProject() (*backlogTarget, error) {
	if apiclient.IsRemoteTarget() {
		if repoFlag == "" {
			return nil, fmt.Errorf("--repo is required with --daemon-url: pass --repo <path on the daemon host at %s>", apiclient.RemoteTargetURL())
		}
		return &backlogTarget{repoPath: repoFlag, name: remoteProjectName(repoFlag)}, nil
	}
	if repoFlag != "" {
		repo, err := repoFromFlag()
		if err != nil {
			return nil, err
		}
		return &backlogTarget{repoID: repo.ID, name: filepath.Base(repo.Root)}, nil
	}
	repo, err := optionalCurrentRepo()
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, errors.New("--repo is required: the current directory is not a git repository — pass --repo <path> to target a project")
	}
	return &backlogTarget{repoID: repo.ID, name: filepath.Base(repo.Root)}, nil
}

// remoteProjectName names a project from a path on a remote host. A path
// with no last element names nothing, and errors then leave the project out.
func remoteProjectName(path string) string {
	base := filepath.Base(filepath.Clean(path))
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

func (t *backlogTarget) get() (*backlog.View, error) {
	var resp daemon.GetBacklogResponse
	if err := callBacklogRoute("GetBacklog", daemon.GetBacklogRequest{RepoID: t.repoID, RepoPath: t.repoPath}, &resp); err != nil {
		return nil, err
	}
	if resp.Backlog == nil {
		resp.Backlog = backlog.New()
	}
	return &resp, nil
}

// parseIDList parses "12,B-13". An empty string is an empty list.
func parseIDList(s string) ([]int, error) {
	ids := []int{}
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		id, err := backlog.ParseID(part)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseOptionalID(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return backlog.ParseID(s)
}

// readText returns v, or stdin when v is "-".
func readText(cmd *cobra.Command, v string) (string, error) {
	if v != "-" {
		return v, nil
	}
	b, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(b), nil
}

var (
	backlogListAll      bool
	backlogListState    string
	backlogListArchived bool
)

// backlogEntry is one item as list and show print it.
type backlogEntry struct {
	backlog.Item
	State    backlog.State `json:"state"`
	Reason   string        `json:"reason,omitempty"`
	NeededBy []int         `json:"needed_by"`
	Children []int         `json:"children"`
	Cost     float64       `json:"cost"`
}

func entryFor(v *backlog.View, it backlog.Item) backlogEntry {
	d := v.States[it.ID]
	e := backlogEntry{Item: it, State: d.State, Reason: d.Reason,
		NeededBy: v.Backlog.NeededBy(it.ID), Children: v.Backlog.Children(it.ID)}
	if e.NeededBy == nil {
		e.NeededBy = []int{}
	}
	if e.Children == nil {
		e.Children = []int{}
	}
	for _, r := range it.Runs {
		e.Cost += r.Cost()
	}
	return e
}

var backlogListCmd = &cobra.Command{
	Use:   "list",
	Short: "Print the backlog in order, as a tree, with each item's state",
	Long: "Print the backlog in order, children under their parent, with each item's state and " +
		"reason. Done and dropped items are hidden unless --all; archived groups and their items " +
		"are hidden unless --archived. With --json, print the items " +
		"as JSON in the {data,error} envelope.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()
		entries, err := backlogList()
		if err != nil {
			return jsonError(err)
		}
		if envelopeOutput {
			return jsonOut(entries)
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), formatBacklogTree(entries))
		return err
	},
}

func backlogList() ([]backlogEntry, error) {
	var want backlog.State
	if backlogListState != "" {
		st, err := backlog.ParseState(backlogListState)
		if err != nil {
			return nil, err
		}
		want = st
	}
	t, err := backlogProject()
	if err != nil {
		return nil, err
	}
	v, err := t.get()
	if err != nil {
		return nil, err
	}
	entries := []backlogEntry{}
	for _, it := range v.Backlog.Items {
		st := v.States[it.ID].State
		switch {
		case !backlogListArchived && v.Backlog.ArchivedBy(it.ID) != 0:
			continue
		case want != "" && st != want:
			continue
		case want == "" && !backlogListAll && (st == backlog.StateDone || st == backlog.StateDropped):
			continue
		}
		entries = append(entries, entryFor(v, it))
	}
	return entries, nil
}

// formatBacklogTree indents each entry under its nearest listed ancestor.
func formatBacklogTree(entries []backlogEntry) string {
	if len(entries) == 0 {
		return "No items yet · Add one with \"af backlog add <title>\".\n"
	}
	depth := map[int]int{}
	var sb strings.Builder
	for _, e := range entries {
		d := 0
		if pd, ok := depth[e.Parent]; ok && e.Parent != 0 {
			d = pd + 1
		}
		depth[e.ID] = d
		state := e.State.Label()
		if e.Reason != "" {
			if strings.HasPrefix(e.Reason, state) {
				state = e.Reason
			} else {
				state += " · " + e.Reason
			}
		}
		switch {
		case e.Archived:
			state += " · archived"
		case e.Paused && len(e.Children) > 0:
			state += " · paused"
		}
		fmt.Fprintf(&sb, "%s%s  %s  · %s\n", strings.Repeat("  ", d), backlog.FormatID(e.ID), e.Title, state)
	}
	return sb.String()
}

var backlogShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Print one item: body, state and reason, links, runs, and thread",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			id, err := backlog.ParseID(args[0])
			if err != nil {
				return nil, err
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			v, err := t.get()
			if err != nil {
				return nil, err
			}
			it := v.Backlog.Item(id)
			if it == nil {
				return nil, &backlog.NotFoundError{ID: id, Project: t.name}
			}
			return entryFor(v, *it), nil
		})
	},
}

var (
	backlogAddBody    string
	backlogAddParent  string
	backlogAddWaitsOn string
	backlogAddBefore  string
	backlogAddAfter   string
	backlogAddProgram string
	backlogAddFile    string
)

var backlogAddCmd = &cobra.Command{
	Use:   "add <title> | add --file <path or ->",
	Short: "Add a draft item, or add and update many items from JSON",
	Long: "Add one draft item, at the bottom of its parent by default. With --file, add or " +
		"update many items at once from JSON: {\"proposal\": \"<slug>\", \"items\": [{\"key\", " +
		"\"title\", \"body\", \"parent\", \"waits_on\", \"program\", \"sequential\"}]}. key is " +
		"local to the file; parent and waits_on take a key or an item id. With proposal set, an " +
		"item with the same proposal and key as an existing one is updated, not added again. The " +
		"batch is all or nothing and prints each key's id. From an item session, add takes only a " +
		"title and --parent: the agent files work it found as a draft, and af notes on the new " +
		"item's thread which item's agent added it.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			self, mErr := inItemSession()
			if mErr != nil {
				return nil, mErr
			}
			if self != nil {
				if err := refuseItemSessionAddFlags(cmd); err != nil {
					return nil, err
				}
				return backlogAddOne(cmd, args, self.ID)
			}
			if backlogAddFile != "" {
				return backlogAddBatch(cmd, args)
			}
			return backlogAddOne(cmd, args, "")
		})
	},
}

// itemSessionAddFlags are the flags of add an item session may pass: an agent
// files the work it found as a draft (decision (3) on #6284), and leaves its
// order, dependencies and program to the user.
var itemSessionAddFlags = map[string]bool{"parent": true}

// refuseItemSessionAddFlags refuses, from an item session, an add that passes
// any other of add's own flags, --file included.
func refuseItemSessionAddFlags(cmd *cobra.Command) error {
	var refused error
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if refused == nil && f.Changed && !itemSessionAddFlags[f.Name] {
			refused = refuseFromItemSession("add --" + f.Name)
		}
	})
	return refused
}

func backlogAddBatch(cmd *cobra.Command, args []string) (any, error) {
	for _, name := range []string{"body", "parent", "waits-on", "before", "after", "program"} {
		if cmd.Flags().Changed(name) {
			return nil, fmt.Errorf("--file takes the items from JSON; --%s cannot go with it", name)
		}
	}
	if len(args) > 0 {
		return nil, errors.New("--file takes the items from JSON; do not pass a title with it")
	}
	raw, err := readFileOrStdin(cmd, backlogAddFile)
	if err != nil {
		return nil, err
	}
	var batch backlog.Batch
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&batch); err != nil {
		return nil, fmt.Errorf("read %s: %w", backlogAddFile, err)
	}
	t, err := backlogProject()
	if err != nil {
		return nil, err
	}
	var resp daemon.AddBacklogItemsResponse
	if err := callBacklogRoute("AddBacklogItems", daemon.AddBacklogItemsRequest{
		RepoID: t.repoID, RepoPath: t.repoPath, Proposal: batch.Proposal, Items: batch.Items,
	}, &resp); err != nil {
		return nil, err
	}
	if resp.IDs == nil {
		resp.IDs = map[string]int{}
	}
	return map[string]any{"ids": resp.IDs}, nil
}

func readFileOrStdin(cmd *cobra.Command, path string) (string, error) {
	if path == "-" {
		return readText(cmd, "-")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// backlogAddOne adds one draft. sessionID is the item session adding it, or "".
func backlogAddOne(cmd *cobra.Command, args []string, sessionID string) (any, error) {
	if len(args) != 1 {
		return nil, errors.New("af backlog add needs a title, or --file <path or -> for many items")
	}
	body, err := readText(cmd, backlogAddBody)
	if err != nil {
		return nil, err
	}
	in := backlog.ItemInput{Key: "item", Title: args[0], Body: body, Program: backlogAddProgram}
	parent, err := parseOptionalID(backlogAddParent)
	if err != nil {
		return nil, err
	}
	in.Parent = backlog.Ref{ID: parent}
	waits, err := parseIDList(backlogAddWaitsOn)
	if err != nil {
		return nil, err
	}
	for _, w := range waits {
		in.WaitsOn = append(in.WaitsOn, backlog.Ref{ID: w})
	}
	before, err := parseOptionalID(backlogAddBefore)
	if err != nil {
		return nil, err
	}
	after, err := parseOptionalID(backlogAddAfter)
	if err != nil {
		return nil, err
	}
	t, err := backlogProject()
	if err != nil {
		return nil, err
	}
	var resp daemon.AddBacklogItemsResponse
	if err := callBacklogRoute("AddBacklogItems", daemon.AddBacklogItemsRequest{
		RepoID: t.repoID, RepoPath: t.repoPath, Items: []backlog.ItemInput{in}, Before: before, After: after,
		SessionID: sessionID,
	}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Items) != 1 {
		return nil, errors.New("the daemon added no item")
	}
	return resp.Items[0], nil
}

var (
	backlogEditTitle      string
	backlogEditBody       string
	backlogEditWaitsOn    string
	backlogEditProgram    string
	backlogEditSequential bool
)

var backlogEditCmd = &cobra.Command{
	Use:   "edit <id>",
	Short: "Change an item's title, body, waits-on, program, or sequential flag",
	Long: "Change the fields you pass. --body - reads the body from stdin. --waits-on replaces " +
		"the list; pass \"\" to clear it. --sequential=true makes a parent run its children in order.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			if err := refuseFromItemSession("edit"); err != nil {
				return nil, err
			}
			id, err := backlog.ParseID(args[0])
			if err != nil {
				return nil, err
			}
			f := cmd.Flags()
			var (
				title, body, program *string
				waits                *[]int
				sequential           *bool
			)
			if f.Changed("title") {
				title = &backlogEditTitle
			}
			if f.Changed("body") {
				text, err := readText(cmd, backlogEditBody)
				if err != nil {
					return nil, err
				}
				body = &text
			}
			if f.Changed("waits-on") {
				ids, err := parseIDList(backlogEditWaitsOn)
				if err != nil {
					return nil, err
				}
				waits = &ids
			}
			if f.Changed("program") {
				program = &backlogEditProgram
			}
			if f.Changed("sequential") {
				sequential = &backlogEditSequential
			}
			if title == nil && body == nil && waits == nil && program == nil && sequential == nil {
				return nil, errors.New("nothing to change — pass --title, --body, --waits-on, --program or --sequential")
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			req := daemon.UpdateBacklogItemRequest{RepoID: t.repoID, RepoPath: t.repoPath, ID: id,
				Title: title, Body: body, WaitsOn: waits, Program: program, Sequential: sequential}
			var resp daemon.UpdateBacklogItemResponse
			if err := callBacklogRoute("UpdateBacklogItem", req, &resp); err != nil {
				return nil, err
			}
			return resp.Item, nil
		})
	},
}

var (
	backlogMoveBefore string
	backlogMoveAfter  string
	backlogMoveInto   string
	backlogMoveTop    bool
	backlogMoveBottom bool
)

var backlogMoveCmd = &cobra.Command{
	Use:   "move <id>",
	Short: "Reorder an item; its children move with it",
	Long: "Move an item and its subtree with exactly one of --before, --after, --top, --bottom " +
		"or --into. --top and --bottom keep it under its current parent; " +
		"to take it out of a parent, use --after <parent>.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			if err := refuseFromItemSession("move"); err != nil {
				return nil, err
			}
			id, err := backlog.ParseID(args[0])
			if err != nil {
				return nil, err
			}
			var to [3]int
			for i, v := range []string{backlogMoveBefore, backlogMoveAfter, backlogMoveInto} {
				if to[i], err = parseOptionalID(v); err != nil {
					return nil, err
				}
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			req := daemon.MoveBacklogItemRequest{RepoID: t.repoID, RepoPath: t.repoPath, ID: id,
				Before: to[0], After: to[1], Into: to[2], Top: backlogMoveTop, Bottom: backlogMoveBottom}
			var resp daemon.MoveBacklogItemResponse
			if err := callBacklogRoute("MoveBacklogItem", req, &resp); err != nil {
				return nil, err
			}
			return map[string]any{"id": id, "moved": true}, nil
		})
	},
}

func init() {
	const repoFlagUsage = "Path to the project's git repository (default: the current directory's project)"
	BacklogCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", repoFlagUsage)
	BacklogCmd.PersistentFlags().BoolVar(&envelopeOutput, "json", false, "Wrap output in the {data,error} JSON envelope (default: bare payload; a text tree for list)")

	backlogListCmd.Flags().BoolVar(&backlogListAll, "all", false, "Include done and dropped items")
	backlogListCmd.Flags().BoolVar(&backlogListArchived, "archived", false, "Include archived groups and their items")
	backlogListCmd.Flags().StringVar(&backlogListState, "state", "", "Only items in this `state` (draft, queued, waiting, starting, working, stopped, in_review, needs_you, done, dropped)")

	f := backlogAddCmd.Flags()
	f.StringVar(&backlogAddBody, "body", "", "Item body in markdown; - reads stdin")
	f.StringVar(&backlogAddParent, "parent", "", "Add the item as a child of this `id`")
	f.StringVar(&backlogAddWaitsOn, "waits-on", "", "Comma-separated item `ids` this item waits on")
	f.StringVar(&backlogAddBefore, "before", "", "Place the item above this sibling `id`")
	f.StringVar(&backlogAddAfter, "after", "", "Place the item below this sibling `id`")
	f.StringVar(&backlogAddProgram, "program", "", "Program to run the item with (default: the project's default)")
	f.StringVar(&backlogAddFile, "file", "", "Add or update many items from this JSON `file`; - reads stdin")

	f = backlogEditCmd.Flags()
	f.StringVar(&backlogEditTitle, "title", "", "New title")
	f.StringVar(&backlogEditBody, "body", "", "New body in markdown; - reads stdin")
	f.StringVar(&backlogEditWaitsOn, "waits-on", "", "Comma-separated item `ids` this item waits on; \"\" clears")
	f.StringVar(&backlogEditProgram, "program", "", "Program to run the item with; \"\" uses the project's default")
	f.BoolVar(&backlogEditSequential, "sequential", false, "Run this parent's children in order")

	f = backlogMoveCmd.Flags()
	f.StringVar(&backlogMoveBefore, "before", "", "Place the item above this `id`")
	f.StringVar(&backlogMoveAfter, "after", "", "Place the item below this `id`")
	f.StringVar(&backlogMoveInto, "into", "", "Make the item the last child of this parent `id`")
	f.BoolVar(&backlogMoveTop, "top", false, "Place the item first under its parent")
	f.BoolVar(&backlogMoveBottom, "bottom", false, "Place the item last under its parent")

	BacklogCmd.AddCommand(backlogListCmd, backlogShowCmd, backlogAddCmd, backlogEditCmd, backlogMoveCmd)
}
