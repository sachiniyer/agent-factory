package microclaw

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var microclawDirFlag string

// MicroClawCmd is the cobra command for the interactive microclaw TUI.
var MicroClawCmd = &cobra.Command{
	Use:   "microclaw",
	Short: "Interactive TUI for MicroClaw messaging",
	Long:  "Opens an interactive chat interface for sending and receiving messages through the MicroClaw bridge.",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := microclawDirFlag
		if dir == "" {
			dir = os.Getenv("MICROCLAW_DIR")
		}
		bridge := NewBridge(dir)
		if !bridge.Available() {
			return fmt.Errorf("MicroClaw not available — set MICROCLAW_DIR or install microclaw")
		}

		chats, err := bridge.ListChats()
		if err != nil {
			return fmt.Errorf("failed to list chats: %w", err)
		}
		if len(chats) == 0 {
			return fmt.Errorf("no MicroClaw chats found")
		}

		// Pick the main (most recent) chat
		chat := chats[0]

		chatName := chat.ChatTitle
		if chatName == "" {
			chatName = fmt.Sprintf("chat-%d", chat.ChatID)
		}

		// Build metadata from environment
		meta := &MessageMeta{}
		if cwd, err := os.Getwd(); err == nil {
			meta.RepoPath = cwd
		}

		return RunTUI(bridge, chat.ChatID, chatName, meta)
	},
}

func init() {
	MicroClawCmd.Flags().StringVar(&microclawDirFlag, "dir", "", "MicroClaw directory (defaults to MICROCLAW_DIR or ~/.microclaw)")
}
