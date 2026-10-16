package main

import (
	"os"

	"github.com/sachiniyer/agent-factory/commands"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

var (
	// version is the dev-build fallback. Released binaries are stamped at
	// build time via -ldflags "-X main.version=..." (see .github/workflows);
	// stable releases also commit the new number here so dev builds report
	// the latest stable base. Preview releases (vX.Y.Z-preview-N, #1041)
	// never rewrite this value.
	version     = "1.0.315"
	rootCommand = commands.NewRootCommand
)

func main() {
	// Installed BEFORE HandleInternalExec, which is the only consumer: the pane
	// shim resolves an account name against this machine's AF home, and
	// internal/agentaccount cannot be reached from internal/sessionenv because the
	// dependency already runs the other way (#3051).
	sessionenv.AccountLookup = func(agent, name string) (sessionenv.Account, error) {
		home, err := config.GetConfigDir()
		if err != nil {
			return sessionenv.Account{}, err
		}
		return agentaccount.Selected(home, agent, name)
	}
	sessionenv.HandleInternalExec()
	// Strip the watch-generation marker (AF_WATCH_GEN only) before anything
	// below can spawn a child (#5329). Defense in depth: tmux servers and panes
	// already get an allowlisted environment that never carries it; this makes
	// sure af's own direct children — an ad-hoc daemon launch, the dedicated
	// tmux-server wrapper, upgrade recovery — cannot forward it either. AF_HOME
	// stays: it is the #1104 pane ancestry marker, and alone it never makes a
	// process reapable.
	sessionenv.StripOwnershipMarkers()
	sessiontmux.HandleDedicatedServerExec()
	// Consume the internal __upgrade-recovery invocation (the persistent recovery
	// job execs the preserved previous binary this way) before Cobra, exactly as
	// the session exec protocol above. An ordinary invocation returns immediately.
	daemon.HandleUpgradeRecoveryExec()
	rootCmd := rootCommand(commands.Options{Version: version})
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
