//go:build !windows

package sessionenv

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExecInvocationFailsClosedOnDollarQuotedCloudModeSelector is the
// end-to-end guard at the actual launch boundary. The pane exec shim
// (execInvocation in exec_unix.go) hands /bin/sh -c the operator's
// command after FilterForCommand has built the environment, so the
// environment argument processExec receives IS the boundary's output.
// Mock processExec and inspect it: a dollar-quoted falsy selector value
// must hand /bin/sh -c an environment that carries no AWS / GCP / Azure
// credential, on any host — bash-as-/bin/sh would otherwise run the
// session with Bedrock OFF while the AWS credential group is present.
//
// processExec and execInvocation live in exec_unix.go, so this test is
// Unix-only: the package does not build the exec shim on Windows
// (exec_windows.go has neither symbol).
func TestExecInvocationFailsClosedOnDollarQuotedCloudModeSelector(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AWS_SESSION_TOKEN", "fixture")
	t.Setenv("AZURE_CLIENT_SECRET", "fixture")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "fixture")
	// Bare claude on PATH so the bare-name grant predicate attributes
	// it to the operator's PATH (operator's PATH = a tempdir af owns).
	pathDir := t.TempDir()
	writeExecutableFixture(t, pathDir, "claude")
	t.Setenv("PATH", pathDir)

	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	err := execInvocation([]string{"claude", "0", "CLAUDE_CODE_USE_BEDROCK=$'0' claude"})
	require.ErrorIs(t, err, wantErr)

	for _, denied := range []string{
		"AWS_ACCESS_KEY_ID=fixture",
		"AWS_SECRET_ACCESS_KEY=fixture",
		"AWS_SESSION_TOKEN=fixture",
		"AZURE_CLIENT_SECRET=fixture",
		"GOOGLE_APPLICATION_CREDENTIALS=fixture",
	} {
		require.Falsef(t, slices.Contains(gotEnvironment, denied),
			"exec shim admitted %s into the environment handed to /bin/sh -c for a dollar-quoted falsy selector value — "+
				"the session would run with Bedrock off (bash-as-/bin/sh expands $'0' to 0) while carrying the AWS credential group",
			denied)
	}
}
