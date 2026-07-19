package api

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/spf13/cobra"
)

var (
	tabCreateCommandFlag string
	tabCreateNameFlag    string
	tabCreateKindFlag    string
	tabCreateURLFlag     string
	tabCreatePortFlag    int
)

// bindTabCreateFlags registers the tab-create flags on c, bound to the shared
// globals. Called for both the hyphen verb and the tabs-create alias (#1192).
func bindTabCreateFlags(c *cobra.Command) {
	c.Flags().StringVar(&tabCreateCommandFlag, "command", "", "Command to run in a process tab (required unless --kind web/vscode)")
	c.Flags().StringVar(&tabCreateNameFlag, "name", "", "Tab name — sanitized to [A-Za-z0-9_-] and auto-suffixed on collision (defaults to the command basename, or \"web\"/\"vscode\" for those kinds)")
	c.Flags().StringVar(&tabCreateKindFlag, "kind", "", "Tab kind: empty for a process tab, \"web\" for a URL/iframe tab, or \"vscode\" for a VS Code editor on the session's worktree")
	c.Flags().StringVar(&tabCreateURLFlag, "url", "", "Web tab target URL (with --kind web): a localhost dev-server address or an external https URL")
	c.Flags().IntVar(&tabCreatePortFlag, "port", 0, "Web tab convenience for --url http://localhost:<port> (with --kind web)")
}

// bindTabDeleteFlags registers the tab-delete flag on c, bound to the shared
// global. Called for both the hyphen verb and the tabs-delete alias (#1192).
func bindTabDeleteFlags(c *cobra.Command) {
	c.Flags().StringVar(&tabDeleteNameFlag, "name", "", "Name of the tab to delete (required; the tab's name, not the TUI's \"Agent\"/\"Terminal\" label)")
	c.MarkFlagRequired("name")
}

var sessionsTabCreateCmd = &cobra.Command{
	Use:   "tab-create <title>",
	Short: "Spawn a process tab (a command), a web tab (a URL/iframe), or a VS Code tab in a session",
	Long: `Create a new tab in an existing session.

Process tab (default): runs --command in the session's git worktree (e.g. a data
explorer TUI or a test watcher). If --name is omitted, a name is derived from the
command's basename.

Web tab (--kind web): a URL/iframe tab with NO process — an agent injects a live
browser view into the user's screen. Point it at a local dev server with --port
<n> (= http://localhost:<n>) or --url <addr>, or at an external site with --url
https://... A localhost/loopback target is reverse-proxied by the daemon so the
preview works even when the web UI is viewed remotely (Tailscale/SSH); an external
URL is iframed directly (best-effort — many sites block embedding). The web tab
renders as an iframe in the web UI and as a placeholder in the TUI.

The tab persists and reconnects across a daemon/af restart like every other tab.

--name sets the tab's name — the handle every other tab verb addresses it by,
not the label the TUI renders (agent and shell tabs always show "Agent" and
"Terminal"). The name is sanitized before use: characters outside [A-Za-z0-9_-]
become "-". It is then made unique within the session (auto-suffixed -2, -3, …).
So the name you pass is not always the name you get — the resolved tab name is
printed on success, and that is the one the other tab verbs address.

Not available for remote sessions: they have no local worktree.`,
	Args: cobra.ExactArgs(1),
	RunE: runTabCreate,
}

// runTabCreate is the shared RunE body behind both the legacy hyphen verb
// (sessions tab-create) and the noun-subcommand alias (sessions tabs create),
// so the two stay byte-identical (#1192). External users script the hyphen
// verb, so it stays first-class — the alias is purely additive.
func runTabCreate(cmd *cobra.Command, args []string) error {
	log.Initialize(false)
	defer log.Close()

	// The kind vocabulary lives in the session package (session.ParseTabKindName),
	// which the daemon dispatches on too — so this client-side check can never
	// accept a kind the daemon would reject. It stays a pre-check purely to fail
	// fast with a flag-shaped message; the daemon re-validates regardless.
	kind, explicitKind := session.ParseTabKindName(tabCreateKindFlag)
	switch {
	case tabCreateKindFlag != "" && !explicitKind:
		return jsonError(fmt.Errorf("--kind must be empty or one of %s, got %q",
			strings.Join(session.TabKindNameList(), ", "), tabCreateKindFlag))
	case explicitKind && kind == session.TabKindWeb:
		if strings.TrimSpace(tabCreateURLFlag) == "" && tabCreatePortFlag == 0 {
			return jsonError(fmt.Errorf("--kind web requires --url or --port"))
		}
		if strings.TrimSpace(tabCreateCommandFlag) != "" {
			return jsonError(fmt.Errorf("--command is not valid for a web tab (--kind web); use --url or --port"))
		}
	case explicitKind && kind == session.TabKindVSCode:
		// A vscode tab always opens the session's own worktree, so a target is
		// meaningless rather than optional.
		if strings.TrimSpace(tabCreateURLFlag) != "" || tabCreatePortFlag != 0 {
			return jsonError(fmt.Errorf("--url/--port are not valid for a vscode tab (--kind vscode): it always opens the session's worktree"))
		}
		if strings.TrimSpace(tabCreateCommandFlag) != "" {
			return jsonError(fmt.Errorf("--command is not valid for a vscode tab (--kind vscode)"))
		}
	default:
		if strings.TrimSpace(tabCreateCommandFlag) == "" {
			return jsonError(fmt.Errorf("--command is required (or pass --kind web with --url/--port, or --kind vscode)"))
		}
	}

	// Honor --repo scoping (#891, same class as kill/send-prompt/attach). An
	// empty repoID preserves the all-repo search; a non-empty one confines the
	// session lookup to that repo so a same-titled session in another repo
	// never receives the tab.
	repoID, err := resolveRepoID()
	if err != nil {
		return jsonError(err)
	}

	name, err := createTabViaDaemon(daemon.CreateTabRequest{
		Title:   args[0],
		RepoID:  repoID,
		Command: tabCreateCommandFlag,
		Name:    tabCreateNameFlag,
		Kind:    tabCreateKindFlag,
		URL:     tabCreateURLFlag,
		Port:    tabCreatePortFlag,
	})
	if err != nil {
		return jsonError(err)
	}
	return jsonOut(map[string]string{"name": name})
}

var tabDeleteNameFlag string

var sessionsTabDeleteCmd = &cobra.Command{
	Use:   "tab-delete <title>",
	Short: "Delete a single tab from a session",
	Long: `Delete the named tab from an existing session — the counterpart of tab-create.

The tab is removed from the daemon's session state and its tmux window is
killed. The removal is persistent: the daemon will not respawn the tab, and it
does not return on a daemon/af restart. The name to pass is the tab name
tab-create printed, as reported by "af sessions get" — not the label the TUI
tab bar shows, which is a fixed "Agent"/"Terminal" for agent and shell tabs. A
miss lists the tabs that exist, with those labels, so a wrong name is a next
step rather than a dead end.

The agent tab can't be deleted — use "af sessions kill" to tear down the whole
session. Deleting a tab or session that doesn't exist is an error, not a
silent success. Not available for remote sessions: their tabs are fixed by
remote_hooks config.`,
	Args: cobra.ExactArgs(1),
	RunE: runTabDelete,
}

// runTabDelete is the shared RunE body behind both sessions tab-delete and the
// sessions tabs delete alias (#1192); see runTabCreate.
func runTabDelete(cmd *cobra.Command, args []string) error {
	log.Initialize(false)
	defer log.Close()

	if strings.TrimSpace(tabDeleteNameFlag) == "" {
		return jsonError(fmt.Errorf("--name is required"))
	}

	// Honor --repo scoping (#891 class), mirroring tab-create: an empty
	// repoID preserves the all-repo search; a non-empty one confines the
	// session lookup to that repo so a same-titled session in another repo
	// never loses a tab.
	repoID, err := resolveRepoID()
	if err != nil {
		return jsonError(err)
	}

	name, err := closeTabViaDaemon(daemon.CloseTabRequest{
		Title:   args[0],
		RepoID:  repoID,
		TabName: tabDeleteNameFlag,
	})
	if err != nil {
		return jsonError(err)
	}
	return jsonOut(map[string]string{"name": name})
}

var (
	tabRenameNameFlag    string
	tabRenameNewNameFlag string
)

// bindTabRenameFlags registers the tab-rename flags on c, bound to the shared
// globals. Called for both the hyphen verb and the tabs-rename alias (#1192).
func bindTabRenameFlags(c *cobra.Command) {
	c.Flags().StringVar(&tabRenameNameFlag, "name", "", "Name of the tab to rename (required; the tab's name, not the TUI's \"Agent\"/\"Terminal\" label)")
	c.Flags().StringVar(&tabRenameNewNameFlag, "new-name", "", "New name for the tab (required; sanitized and auto-suffixed on collision)")
	c.MarkFlagRequired("name")
	c.MarkFlagRequired("new-name")
}

var sessionsTabRenameCmd = &cobra.Command{
	Use:   "tab-rename <title>",
	Short: "Rename a tab of a session",
	Long: `Rename an existing tab — the fix for a name you have to live with all day,
typically one an agent picked when it created the tab.

Only web, process and VS Code tabs can be renamed: those are the tabs that
display their name. The agent tab always shows "Agent" and shell tabs always show
"Terminal" on every surface, so renaming one would change the handle you address
it by without changing anything you can see — confusing rather than useful, so it
is refused.

--new-name follows the same rules as tab-create's --name: characters outside
[A-Za-z0-9_-] become "-", and the name is made unique within the session
(auto-suffixed -2, -3, …). A name that sanitizes away to nothing is an error
rather than a silent fall back to a default. The resolved name is printed on
success — that is what the tab is actually called, and what the other tab verbs
now address it by.

The rename persists across a daemon/af restart and does not disturb the tab's
running process. Not available for remote sessions (their tabs are fixed by
remote_hooks config) or archived sessions (restore them first).`,
	Args: cobra.ExactArgs(1),
	RunE: runTabRename,
}

// runTabRename is the shared RunE body behind both sessions tab-rename and the
// sessions tabs rename alias (#1192); see runTabCreate.
func runTabRename(cmd *cobra.Command, args []string) error {
	log.Initialize(false)
	defer log.Close()

	if strings.TrimSpace(tabRenameNameFlag) == "" {
		return jsonError(fmt.Errorf("--name is required"))
	}
	if strings.TrimSpace(tabRenameNewNameFlag) == "" {
		return jsonError(fmt.Errorf("--new-name is required"))
	}

	// Honor --repo scoping (#891 class), mirroring tab-create/tab-delete: an
	// empty repoID preserves the all-repo search; a non-empty one confines the
	// session lookup to that repo so a same-titled session in another repo never
	// has a tab renamed out from under it.
	repoID, err := resolveRepoID()
	if err != nil {
		return jsonError(err)
	}

	name, err := renameTabViaDaemon(daemon.RenameTabRequest{
		Title:   args[0],
		RepoID:  repoID,
		TabName: tabRenameNameFlag,
		NewName: tabRenameNewNameFlag,
	})
	if err != nil {
		return jsonError(err)
	}
	return jsonOut(map[string]string{"name": name})
}

var (
	tabReorderNameFlag  string
	tabReorderIndexFlag int
)

// bindTabReorderFlags registers the tab-reorder flags on c, bound to the shared
// globals. Called for both the hyphen verb and the tabs-reorder alias (#1192).
func bindTabReorderFlags(c *cobra.Command) {
	c.Flags().StringVar(&tabReorderNameFlag, "name", "", "Name of the tab to move (required; the tab's name, not the TUI's \"Agent\"/\"Terminal\" label)")
	c.Flags().IntVar(&tabReorderIndexFlag, "index", 0, "Destination slot, 0-based, as the tab bar reads left to right (required; slot 0 is the agent tab and can't be targeted)")
	c.MarkFlagRequired("name")
	c.MarkFlagRequired("index")
}

var sessionsTabReorderCmd = &cobra.Command{
	Use:   "tab-reorder <title>",
	Short: "Move a tab within a session's tab order",
	Long: `Move a tab to a different slot, so the tab order is yours rather than whatever
order the tabs happened to be created in.

--index is the destination slot, 0-based, counting the tab bar left to right,
and read as the tab's FINAL position: moving a tab to --index 3 of a 4-tab
session puts it last.

Slot 0 is reserved for the agent tab: the agent tab can't be moved, and no tab
can be moved in front of it. That is structural, not cosmetic — the agent tab is
identified by its position throughout a session's lifecycle.

The new order persists across a daemon/af restart and does not disturb any tab's
running process. Not available for remote sessions (their tabs are fixed by
remote_hooks config) or archived sessions (restore them first).`,
	Args: cobra.ExactArgs(1),
	RunE: runTabReorder,
}

// runTabReorder is the shared RunE body behind both sessions tab-reorder and
// the sessions tabs reorder alias (#1192); see runTabCreate.
func runTabReorder(cmd *cobra.Command, args []string) error {
	log.Initialize(false)
	defer log.Close()

	if strings.TrimSpace(tabReorderNameFlag) == "" {
		return jsonError(fmt.Errorf("--name is required"))
	}

	// Honor --repo scoping (#891 class), mirroring the other tab verbs.
	repoID, err := resolveRepoID()
	if err != nil {
		return jsonError(err)
	}

	name, index, err := reorderTabViaDaemon(daemon.ReorderTabRequest{
		Title:    args[0],
		RepoID:   repoID,
		TabName:  tabReorderNameFlag,
		NewIndex: tabReorderIndexFlag,
	})
	if err != nil {
		return jsonError(err)
	}
	return jsonOut(map[string]any{"name": name, "index": index})
}

// The sessions tabs {create,delete,rename,reorder} group gives a
// noun-subcommand spelling of the tab-* verbs (#1192). Both spellings share the
// same RunE and flag globals; the hyphen verbs are kept for the scripts that
// already use them. tab-list has no equivalent — tabs are listed via `sessions
// get`.
var sessionsTabsCmd = &cobra.Command{
	Use:   "tabs",
	Short: "Manage a session's tabs (create/delete/rename/reorder)",
	Long: `Noun-subcommand aliases for the tab-create/tab-delete/tab-rename/tab-reorder
verbs.

"sessions tabs create" is identical to "sessions tab-create", and the same holds
for delete, rename and reorder; the hyphen verbs remain supported for existing
scripts. To list a session's tabs, use "sessions get <title>".`,
}

var sessionsTabsRenameCmd = &cobra.Command{
	Use:   "rename <title>",
	Short: "Rename a tab of a session",
	Long:  `Alias for "sessions tab-rename". See "af sessions tab-rename --help" for details.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTabRename,
}

var sessionsTabsReorderCmd = &cobra.Command{
	Use:   "reorder <title>",
	Short: "Move a tab within a session's tab order",
	Long:  `Alias for "sessions tab-reorder". See "af sessions tab-reorder --help" for details.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTabReorder,
}

var sessionsTabsCreateCmd = &cobra.Command{
	Use:   "create <title>",
	Short: "Spawn a process tab (a command), a web tab (a URL/iframe), or a VS Code tab in a session",
	Long:  `Alias for "sessions tab-create". See "af sessions tab-create --help" for details.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTabCreate,
}

var sessionsTabsDeleteCmd = &cobra.Command{
	Use:   "delete <title>",
	Short: "Delete a single tab from a session",
	Long:  `Alias for "sessions tab-delete". See "af sessions tab-delete --help" for details.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runTabDelete,
}
