//go:build linux

package task

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateServiceContentPathsWithSpaces(t *testing.T) {
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

	// ExecStart uses shell-style quoting in systemd, so quoting the
	// executable path is correct and required to survive spaces.
	assert.Contains(t, content, `ExecStart="/opt/my apps/agent-factory" task run abc1`)

	// WorkingDirectory does NOT use shell-style quote parsing — quotes
	// would be kept literally. The raw path (spaces and all) is valid.
	assert.Contains(t, content, "\nWorkingDirectory=/home/user/my projects/repo\n")
	assert.NotContains(t, content, `WorkingDirectory="/home/user/my projects/repo"`)

	// Environment= values are likewise assignment-style; no surrounding quotes.
	assert.Contains(t, content, "\nEnvironment=PATH=/usr/bin:/usr/local/bin\n")
	assert.Contains(t, content, "\nEnvironment=HOME=/home/user\n")
	assert.Contains(t, content, "\nEnvironment=SHELL=/bin/bash\n")
	assert.Contains(t, content, "\nEnvironment=TERM=xterm-256color\n")
	assert.NotContains(t, content, `Environment="PATH=`)
	assert.NotContains(t, content, `Environment="HOME=`)
	assert.NotContains(t, content, `Environment="SHELL=`)
	assert.NotContains(t, content, `Environment="TERM=`)
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

	// ExecStart keeps its shell-style quoting (safe for any path).
	assert.Contains(t, content, `ExecStart="/usr/local/bin/agent-factory" task run def2`)

	// WorkingDirectory and Environment lines must not be wrapped in
	// literal double-quotes, since systemd would treat them as value
	// characters and reject the resulting unit file.
	assert.Contains(t, content, "\nWorkingDirectory=/home/user/repo\n")
	assert.NotContains(t, content, `WorkingDirectory="`)
	assert.NotContains(t, content, `Environment="`)

	// Verify it's a valid unit structure.
	assert.True(t, strings.Contains(content, "[Unit]"))
	assert.True(t, strings.Contains(content, "[Service]"))
	assert.True(t, strings.Contains(content, "Type=oneshot"))
}

func TestGenerateServiceContentEnvNewlineSanitized(t *testing.T) {
	// Newlines in Environment= values would produce an invalid unit file;
	// sanitizeEnvValue replaces them so the unit remains parseable.
	content := generateServiceContent(
		"agent-factory-task-xyz9",
		"/usr/local/bin/agent-factory",
		"xyz9",
		"/home/user/repo",
		"/usr/bin\n/evil",
		"/home/user",
		"/bin/bash",
		"xterm-256color",
	)

	// The PATH line must not contain an embedded newline that would
	// prematurely terminate the Environment= assignment.
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "Environment=PATH=") {
			assert.NotContains(t, line, "\n")
		}
	}
	assert.Contains(t, content, "Environment=PATH=/usr/bin /evil")
}

func TestGenerateServiceContentWorkingDirectoryNewlineSanitized(t *testing.T) {
	// A newline inside WorkingDirectory= would terminate the value and cause
	// systemd to run the task in a truncated directory. The unit must have
	// the newline replaced so the full path is preserved on a single line.
	projectPath := "/tmp/test\ninjected"
	content := generateServiceContent(
		"agent-factory-task-nl1",
		"/usr/local/bin/agent-factory",
		"nl1",
		projectPath,
		"/usr/bin",
		"/home/user",
		"/bin/bash",
		"xterm-256color",
	)

	// The raw newline must not appear mid-value.
	assert.NotContains(t, content, "WorkingDirectory=/tmp/test\n")

	// The sanitized single-line WorkingDirectory= should contain the full
	// path with the newline replaced (spaces, matching Environment= handling).
	assert.Contains(t, content, "\nWorkingDirectory=/tmp/test injected\n")

	// Defensive: whichever line holds WorkingDirectory= must be free of any
	// embedded newline in its value.
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "WorkingDirectory=") {
			assert.NotContains(t, line, "\n")
		}
	}
}

func TestGenerateServiceContentExecStartEscapesQuotes(t *testing.T) {
	// An executable path containing a literal double-quote must not
	// break out of the ExecStart= quoted string.
	content := generateServiceContent(
		"agent-factory-task-q1",
		`/opt/weird"path/agent-factory`,
		"q1",
		"/home/user/repo",
		"/usr/bin",
		"/home/user",
		"/bin/bash",
		"xterm-256color",
	)

	assert.Contains(t, content, `ExecStart="/opt/weird\"path/agent-factory" task run q1`)
}
