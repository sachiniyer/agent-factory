package task

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// validateTaskHost refuses a task host that is not in the operator's global host
// list, with the same message every other pick surface gives (the shared
// config.ValidateHostPick). An empty host is "no pick" and always valid. The list
// is read from the GLOBAL config only — a repository can neither add a host nor
// satisfy this check.
func validateTaskHost(host string) error {
	if host == "" {
		return nil
	}
	if err := config.ValidateHostName(host); err != nil {
		return fmt.Errorf("task host: %w", err)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("task host %q cannot be checked against the host list: %w", host, err)
	}
	if err := config.ValidateHostPick(cfg.Hosts, host); err != nil {
		return fmt.Errorf("task host: %w", err)
	}
	return nil
}
