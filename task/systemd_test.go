//go:build linux

package task

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateServiceContentQuotesPaths(t *testing.T) {
	content := generateServiceContent(
		"agent-factory-task-abc1",
		"/opt/my apps/agent-factory",
		"abc1",
		"/home/user/my projects/repo",
		"/usr/bin:/usr/local/bin",
		"/home/user",
		"/bin/bash",
		"xterm-256color",
	)

	// ExecStart must quote the executable path so spaces are handled correctly.
	assert.Contains(t, content, `ExecStart="/opt/my apps/agent-factory" task run abc1`)

	// WorkingDirectory must quote the project path.
	assert.Contains(t, content, `WorkingDirectory="/home/user/my projects/repo"`)
}

func TestGenerateServiceContentNoSpaces(t *testing.T) {
	content := generateServiceContent(
		"agent-factory-task-def2",
		"/usr/local/bin/agent-factory",
		"def2",
		"/home/user/repo",
		"/usr/bin",
		"/home/user",
		"/bin/bash",
		"xterm-256color",
	)

	// Even without spaces, paths should be quoted (quoting is always safe).
	assert.Contains(t, content, `ExecStart="/usr/local/bin/agent-factory" task run def2`)
	assert.Contains(t, content, `WorkingDirectory="/home/user/repo"`)

	// Verify it's a valid unit structure.
	assert.True(t, strings.Contains(content, "[Unit]"))
	assert.True(t, strings.Contains(content, "[Service]"))
	assert.True(t, strings.Contains(content, "Type=oneshot"))
}
