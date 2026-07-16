package main

import (
	"os"

	"github.com/sachiniyer/agent-factory/commands"
)

var (
	// version is the dev-build fallback. Released binaries are stamped at
	// build time via -ldflags "-X main.version=..." (see .github/workflows);
	// stable releases also commit the new number here so dev builds report
	// the latest stable base. Preview releases (vX.Y.Z-preview-N, #1041)
	// never rewrite this value.
	version     = "1.0.192"
	rootCommand = commands.NewRootCommand
)

func main() {
	rootCmd := rootCommand(commands.Options{Version: version})
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
