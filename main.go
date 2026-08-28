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
	version     = "1.0.267"
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
		executable, err := os.Executable()
		if err != nil {
			// Not fatal: TrustedWrapper only widens what the command guard accepts,
			// so an unknown path means a bare `af` is still recognised and anything
			// else is refused. Failing closed is the correct direction.
			executable = ""
		}
		return agentaccount.Selected(home, agent, name, executable)
	}
	sessionenv.HandleInternalExec()
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
