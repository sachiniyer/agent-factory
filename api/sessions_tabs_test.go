package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"

	"github.com/spf13/cobra"
)

// findSub returns the direct subcommand of parent whose first Use word matches
// name, or nil.
func findSub(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// TestSessionsTabsAliasesRegistered pins the #1192 additive aliases: the
// noun-subcommand `sessions tabs {create,delete,rename,reorder}` group exists
// alongside the original hyphen verbs (which must stay for scripts), and each
// alias carries the same required flags as the verb it mirrors.
func TestSessionsTabsAliasesRegistered(t *testing.T) {
	// Hyphen verbs must still be present — removing them would break scripts.
	if findSub(SessionsCmd, "tab-create") == nil {
		t.Error("sessions tab-create must remain registered")
	}
	if findSub(SessionsCmd, "tab-delete") == nil {
		t.Error("sessions tab-delete must remain registered")
	}
	if findSub(SessionsCmd, "tab-rename") == nil {
		t.Error("sessions tab-rename must be registered")
	}
	if findSub(SessionsCmd, "tab-reorder") == nil {
		t.Error("sessions tab-reorder must be registered")
	}

	tabs := findSub(SessionsCmd, "tabs")
	if tabs == nil {
		t.Fatal("sessions tabs group not registered")
	}

	create := findSub(tabs, "create")
	if create == nil {
		t.Fatal("sessions tabs create not registered")
	}
	if create.RunE == nil {
		t.Error("sessions tabs create has no RunE")
	}
	if create.Flag("command") == nil {
		t.Error("sessions tabs create missing --command flag")
	}
	if create.Flag("name") == nil {
		t.Error("sessions tabs create missing --name flag")
	}

	del := findSub(tabs, "delete")
	if del == nil {
		t.Fatal("sessions tabs delete not registered")
	}
	if del.RunE == nil {
		t.Error("sessions tabs delete has no RunE")
	}
	if del.Flag("name") == nil {
		t.Error("sessions tabs delete missing --name flag")
	}

	rename := findSub(tabs, "rename")
	if rename == nil {
		t.Fatal("sessions tabs rename not registered")
	}
	if rename.RunE == nil {
		t.Error("sessions tabs rename has no RunE")
	}
	for _, f := range []string{"name", "new-name"} {
		if rename.Flag(f) == nil {
			t.Errorf("sessions tabs rename missing --%s flag", f)
		}
	}

	reorder := findSub(tabs, "reorder")
	if reorder == nil {
		t.Fatal("sessions tabs reorder not registered")
	}
	if reorder.RunE == nil {
		t.Error("sessions tabs reorder has no RunE")
	}
	for _, f := range []string{"name", "index"} {
		if reorder.Flag(f) == nil {
			t.Errorf("sessions tabs reorder missing --%s flag", f)
		}
	}
}

func setTabCreateFlagsForTest(t *testing.T, command, name, kind, url string, port int) {
	t.Helper()
	previousCommand, previousName, previousKind := tabCreateCommandFlag, tabCreateNameFlag, tabCreateKindFlag
	previousURL, previousPort := tabCreateURLFlag, tabCreatePortFlag
	t.Cleanup(func() {
		tabCreateCommandFlag, tabCreateNameFlag, tabCreateKindFlag = previousCommand, previousName, previousKind
		tabCreateURLFlag, tabCreatePortFlag = previousURL, previousPort
	})
	tabCreateCommandFlag, tabCreateNameFlag, tabCreateKindFlag = command, name, kind
	tabCreateURLFlag, tabCreatePortFlag = url, port
}

// TestSessionsTabCreateShellUsesTheUIShellPath closes
// session.tab.create.shell: --kind shell is the canonical CLI spelling, but the
// request is normalized onto Shell=true — the same daemon path both UIs use —
// instead of growing a second CLI-only shell implementation.
func TestSessionsTabCreateShellUsesTheUIShellPath(t *testing.T) {
	repoID := setupRepoForCmd(t)
	setTabCreateFlagsForTest(t, "", "", "shell", "", 0)

	var gotReq daemon.CreateTabRequest
	prevCreate := createTabViaDaemon
	createTabViaDaemon = func(req daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		gotReq = req
		return daemon.CreateTabResponse{Name: "shell"}, nil
	}
	defer func() { createTabViaDaemon = prevCreate }()

	out, err := runCmdCaptureStdout(t, sessionsTabCreateCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("tab-create --kind shell returned error: %v", err)
	}
	if gotReq.Title != "worker" || gotReq.RepoID != repoID || !gotReq.Shell {
		t.Fatalf("shell request = %+v, want title=worker repo_id=%q Shell=true", gotReq, repoID)
	}
	if gotReq.Kind != "" || gotReq.Command != "" {
		t.Fatalf("shell request grew a parallel kind/command path: %+v", gotReq)
	}

	var parsed map[string]string
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not JSON (%q): %v", string(out), err)
	}
	if parsed["name"] != "shell" {
		t.Fatalf("JSON name = %q, want shell", parsed["name"])
	}
}

// TestSessionsTabCreateTerminalLabelIsNotAKind keeps the canonical vocabulary
// honest: Terminal is a UI label, just as it is for tab addressing (#1986).
// The rejection must lead the user to the working shell spelling.
func TestSessionsTabCreateTerminalLabelIsNotAKind(t *testing.T) {
	setTabCreateFlagsForTest(t, "", "", "terminal", "", 0)

	called := false
	prevCreate := createTabViaDaemon
	createTabViaDaemon = func(daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		called = true
		return daemon.CreateTabResponse{}, nil
	}
	defer func() { createTabViaDaemon = prevCreate }()

	err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"worker"})
	if err == nil || !strings.Contains(err.Error(), "shell, vscode, web") {
		t.Fatalf("Terminal label rejection = %v, want an error naming canonical kind shell", err)
	}
	if called {
		t.Fatal("an invalid presentation label reached the daemon")
	}
}

// TestSessionsTabCreateShellRejectsIgnoredOptions makes the new spelling
// fail closed: AddShellTab owns the name and command, so accepting one of these
// options would claim a customization the shared shell path silently ignores.
func TestSessionsTabCreateShellRejectsIgnoredOptions(t *testing.T) {
	for _, tc := range []struct {
		name, command, tabName, url string
		port                        int
		want                        string
	}{
		{name: "command", command: "bash", want: "--command"},
		{name: "name", tabName: "console", want: "--name"},
		{name: "url", url: "https://example.com", want: "--url/--port"},
		{name: "port", port: 3000, want: "--url/--port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTabCreateFlagsForTest(t, tc.command, tc.tabName, "shell", tc.url, tc.port)

			called := false
			previousCreate := createTabViaDaemon
			createTabViaDaemon = func(daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
				called = true
				return daemon.CreateTabResponse{}, nil
			}
			defer func() { createTabViaDaemon = previousCreate }()

			err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"worker"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("shell with %s error = %v, want an error naming %s", tc.name, err, tc.want)
			}
			if called {
				t.Fatalf("shell with invalid %s reached the daemon", tc.name)
			}
		})
	}
}

// TestSessionsTabCreateMissingCommandNamesShellAlternative covers the original
// #2619 dead end: an omitted process command must point at the working explicit
// shell spelling rather than naming only web and VS Code.
func TestSessionsTabCreateMissingCommandNamesShellAlternative(t *testing.T) {
	setTabCreateFlagsForTest(t, "", "", "", "", 0)
	err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"worker"})
	if err == nil || !strings.Contains(err.Error(), "--kind shell") {
		t.Fatalf("missing-command error = %v, want it to name --kind shell", err)
	}
}

// CLI seam tests for tab-rename/tab-reorder (#1813). They pin the two things the
// cobra layer owns and the daemon cannot check on its behalf: that --repo
// scoping reaches the request (#891 class — otherwise a same-titled session in
// another repo is the one that gets renamed), and that the JSON contract
// scripts and agents parse is what it claims to be.

// TestSessionsTabRename_HonorsRepoScopingAndReturnsResolvedName: tab-rename
// passes the resolved RepoID, title, tab and new name to the daemon, and prints
// the RESOLVED name — the daemon sanitizes and de-duplicates, so what a script
// must record is what came back, not what it asked for.
func TestSessionsTabRename_HonorsRepoScopingAndReturnsResolvedName(t *testing.T) {
	repoID := setupRepoForCmd(t)

	prevName, prevNew := tabRenameNameFlag, tabRenameNewNameFlag
	tabRenameNameFlag = "preview"
	tabRenameNewNameFlag = "storefront"
	defer func() { tabRenameNameFlag, tabRenameNewNameFlag = prevName, prevNew }()

	var gotReq daemon.RenameTabRequest
	prev := renameTabViaDaemon
	renameTabViaDaemon = func(req daemon.RenameTabRequest) (string, error) {
		gotReq = req
		if req.RepoID == "" {
			return "", errors.New("RepoID empty: --repo scoping was dropped")
		}
		return "storefront-2", nil // the resolved (collision-suffixed) name
	}
	defer func() { renameTabViaDaemon = prev }()

	out, err := runCmdCaptureStdout(t, sessionsTabRenameCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("tab-rename returned error: %v", err)
	}
	if gotReq.RepoID != repoID {
		t.Fatalf("tab-rename RepoID = %q, want %q", gotReq.RepoID, repoID)
	}
	if gotReq.Title != "worker" || gotReq.TabName != "preview" || gotReq.NewName != "storefront" {
		t.Fatalf("tab-rename request = %+v, want title=worker tab_name=preview new_name=storefront", gotReq)
	}

	var parsed map[string]string
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not JSON (%q): %v", string(out), err)
	}
	if parsed["name"] != "storefront-2" {
		t.Fatalf("JSON name = %q, want the resolved storefront-2", parsed["name"])
	}
}

// TestSessionsTabRename_RequiresFlags: both flags are validated before the
// daemon is reached, so an incomplete invocation fails fast and actionably
// rather than round-tripping.
func TestSessionsTabRename_RequiresFlags(t *testing.T) {
	setupRepoForCmd(t)

	called := false
	prev := renameTabViaDaemon
	renameTabViaDaemon = func(daemon.RenameTabRequest) (string, error) {
		called = true
		return "", nil
	}
	defer func() { renameTabViaDaemon = prev }()

	prevName, prevNew := tabRenameNameFlag, tabRenameNewNameFlag
	defer func() { tabRenameNameFlag, tabRenameNewNameFlag = prevName, prevNew }()

	for _, tc := range []struct{ name, newName, want string }{
		{"", "storefront", "--name is required"},
		{"preview", "   ", "--new-name is required"},
	} {
		tabRenameNameFlag, tabRenameNewNameFlag = tc.name, tc.newName
		err := sessionsTabRenameCmd.RunE(sessionsTabRenameCmd, []string{"worker"})
		if err == nil {
			t.Fatalf("expected an error for (--name %q, --new-name %q), got nil", tc.name, tc.newName)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %q, got: %v", tc.want, err)
		}
	}
	if called {
		t.Fatal("an incomplete tab-rename must not reach the daemon")
	}
}

// TestSessionsTabReorder_HonorsRepoScopingAndReturnsIndex: tab-reorder passes
// the resolved RepoID, title, tab and destination to the daemon and emits the
// {name, index} JSON contract.
func TestSessionsTabReorder_HonorsRepoScopingAndReturnsIndex(t *testing.T) {
	repoID := setupRepoForCmd(t)

	prevName, prevIndex := tabReorderNameFlag, tabReorderIndexFlag
	tabReorderNameFlag = "preview"
	tabReorderIndexFlag = 3
	defer func() { tabReorderNameFlag, tabReorderIndexFlag = prevName, prevIndex }()

	var gotReq daemon.ReorderTabRequest
	prev := reorderTabViaDaemon
	reorderTabViaDaemon = func(req daemon.ReorderTabRequest) (string, int, error) {
		gotReq = req
		if req.RepoID == "" {
			return "", 0, errors.New("RepoID empty: --repo scoping was dropped")
		}
		return "preview", 3, nil
	}
	defer func() { reorderTabViaDaemon = prev }()

	out, err := runCmdCaptureStdout(t, sessionsTabReorderCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("tab-reorder returned error: %v", err)
	}
	if gotReq.RepoID != repoID {
		t.Fatalf("tab-reorder RepoID = %q, want %q", gotReq.RepoID, repoID)
	}
	if gotReq.Title != "worker" || gotReq.TabName != "preview" || gotReq.NewIndex != 3 {
		t.Fatalf("tab-reorder request = %+v, want title=worker tab_name=preview new_index=3", gotReq)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not JSON (%q): %v", string(out), err)
	}
	if parsed["name"] != "preview" || parsed["index"] != float64(3) {
		t.Fatalf("JSON = %v, want name=preview index=3", parsed)
	}
}

// TestSessionsTabReorder_RequiresName: --name is validated before the daemon is
// reached. --index is deliberately NOT range-checked here: the daemon owns the
// "slot 0 is the agent tab" rule, and only it holds the roster the upper bound
// depends on. Duplicating a partial rule in the CLI would be the drift the
// shared resolver exists to avoid.
func TestSessionsTabReorder_RequiresName(t *testing.T) {
	setupRepoForCmd(t)

	called := false
	prev := reorderTabViaDaemon
	reorderTabViaDaemon = func(daemon.ReorderTabRequest) (string, int, error) {
		called = true
		return "", 0, nil
	}
	defer func() { reorderTabViaDaemon = prev }()

	prevName := tabReorderNameFlag
	tabReorderNameFlag = "  "
	defer func() { tabReorderNameFlag = prevName }()

	err := sessionsTabReorderCmd.RunE(sessionsTabReorderCmd, []string{"worker"})
	if err == nil {
		t.Fatal("expected an error for an empty --name, got nil")
	}
	if !strings.Contains(err.Error(), "--name is required") {
		t.Fatalf("expected a --name-required error, got: %v", err)
	}
	if called {
		t.Fatal("an incomplete tab-reorder must not reach the daemon")
	}
}
