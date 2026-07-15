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
	c.Flags().StringVar(&tabCreateNameFlag, "name", "", "Tab name (defaults to the command basename, or \"web\"/\"vscode\" for those kinds; auto-suffixed on collision)")
	c.Flags().StringVar(&tabCreateKindFlag, "kind", "", "Tab kind: empty for a process tab, \"web\" for a URL/iframe tab, or \"vscode\" for a VS Code editor on the session's worktree")
	c.Flags().StringVar(&tabCreateURLFlag, "url", "", "Web tab target URL (with --kind web): a localhost dev-server address or an external https URL")
	c.Flags().IntVar(&tabCreatePortFlag, "port", 0, "Web tab convenience for --url http://localhost:<port> (with --kind web)")
}

// bindTabDeleteFlags registers the tab-delete flag on c, bound to the shared
// global. Called for both the hyphen verb and the tabs-delete alias (#1192).
func bindTabDeleteFlags(c *cobra.Command) {
	c.Flags().StringVar(&tabDeleteNameFlag, "name", "", "Name of the tab to delete (required)")
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
The name is made unique within the session (auto-suffixed -2, -3, …). The resolved
tab name is printed on success so scripts/agents can address it. Not available for
remote sessions: they have no local worktree.`,
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
tab-create printed (also visible in the TUI tab bar).

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

// The sessions tabs {create,delete} group gives a noun-subcommand spelling of
// the tab-create/tab-delete verbs (#1192). Both spellings share the same RunE
// and flag globals; the hyphen verbs are kept for the scripts that already use
// them. tab-list has no equivalent — tabs are listed via `sessions get`.
var sessionsTabsCmd = &cobra.Command{
	Use:   "tabs",
	Short: "Manage a session's process tabs (create/delete)",
	Long: `Noun-subcommand aliases for the tab-create/tab-delete verbs.

"sessions tabs create" is identical to "sessions tab-create" and "sessions tabs
delete" is identical to "sessions tab-delete"; the hyphen verbs remain supported
for existing scripts. To list a session's tabs, use "sessions get <title>".`,
}

var sessionsTabsCreateCmd = &cobra.Command{
	Use:   "create <title>",
	Short: "Spawn a process tab (a command) or a web tab (a URL/iframe) in a session",
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
