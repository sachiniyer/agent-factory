package doctor

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

func checkCodexAccounts(home string, report *Report) {
	if home == "" {
		return
	}
	names, err := agentaccount.List(home, "codex")
	if err != nil {
		report.addActionableFinding(Finding{Check: "codex-account-settings", Section: sectionConfig, Detail: fmt.Sprintf("Could not inspect Codex accounts under %s: %v", home, err)})
		return
	}
	for _, name := range names {
		account, err := agentaccount.Selected(home, "codex", name, "")
		if err != nil {
			report.addActionableFinding(Finding{Check: "codex-account-settings", Section: sectionConfig, Detail: err.Error()})
			continue
		}
		if warning := agentaccount.CodexApprovalWarning(name, account.Dir); warning != "" {
			report.addActionableFinding(Finding{Check: "codex-account-settings", Section: sectionConfig, Detail: warning})
		}
	}
}
