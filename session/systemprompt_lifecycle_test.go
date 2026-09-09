package session

import (
	"strings"
	"testing"
)

// Pin the lifecycle promises adopted by the CLI in #4031 in both agent audiences.
func TestAfUsageReferenceLifecycleCopy(t *testing.T) {
	for name, reference := range map[string]string{
		"inside": afUsageReference,
		"plugin": AfPluginUsageReference,
	} {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{
				"agent idle (awaiting input, not work completion)",
				"exits 0 on idle, non-zero on lost/dead/archived or --timeout (default 30m)",
				"Delete a session; work in af-owned workspaces can be lost",
			} {
				if !strings.Contains(reference, want) {
					t.Errorf("lifecycle guidance missing %q", want)
				}
			}
			for _, stale := range []string{"agent done", "ready for review", "clean up its worktree", "which deletes the worktree"} {
				if strings.Contains(reference, stale) {
					t.Errorf("stale lifecycle guidance %q", stale)
				}
			}
		})
	}
}

func TestAfUsageReferenceLifecycleColumns(t *testing.T) {
	for _, command := range []string{"watch", "kill"} {
		prefix := "  af sessions " + command + " <title>"
		for _, line := range strings.Split(afUsageBody, "\n") {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			// Preserve the existing description column and watch row's width budget.
			description := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if strings.Index(line, description) != 55 || len(line) > 198 {
				t.Errorf("%s row exceeds its layout budget: %q", command, line)
			}
		}
	}
}

func TestAfUsageReferenceRootAgentConfigCopy(t *testing.T) {
	for name, reference := range map[string]string{
		"inside": afUsageReference,
		"plugin": AfPluginUsageReference,
	} {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{
				"af config set root_agent.enabled <bool>",
				"af config set root_agent.program <command>",
				"kill the root, then restart the daemon",
			} {
				if !strings.Contains(reference, want) {
					t.Errorf("root-agent config guidance missing %q", want)
				}
			}
		})
	}
}
