package tmux

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/stretchr/testify/require"
)

// Exercise both public consumers before changing the shared implementation.
// Assert the fallback interaction as well as the result: adoption intentionally
// rejects both absent and unknown generations and cannot expose this regression.
func TestSessionMarkerReaders(t *testing.T) {
	for _, reader := range []struct {
		marker string
		read   func(cmd.Executor, string) (string, bool, error)
	}{
		{EnvMarkerHome, SessionHomeMarker},
		{EnvMarkerGeneration, SessionGenerationMarker},
	} {
		testSessionMarkerRead(t, reader.marker, reader.read)
	}
}

func testSessionMarkerRead(t *testing.T, marker string, read func(cmd.Executor, string) (string, bool, error)) {
	t.Helper()
	type readCase struct {
		name, output                         string
		queryErr, fallbackErr                error
		wantPresent, wantError, wantFallback bool
	}
	cases := []readCase{
		{name: "present", output: marker + "=value\n", wantPresent: true},
		{name: "empty_present", output: marker + "=\n", wantPresent: true},
		{name: "unset", output: "-" + marker + "\n"},
		{name: "absent", queryErr: &exec.ExitError{Stderr: []byte("unknown variable: " + marker + "\n")}, wantFallback: true},
		{name: "absent_unanswered", queryErr: &exec.ExitError{Stderr: []byte("unknown variable: " + marker)}, fallbackErr: errors.New("server unavailable"), wantError: true, wantFallback: true},
		{name: "unknown", queryErr: errors.New("server unavailable"), wantError: true},
		{name: "malformed", output: "unexpected=value\n", wantError: true},
		{name: "multiline", output: marker + "=value\nforged=value\n", wantError: true},
	}
	for _, other := range []string{EnvMarkerSession, EnvMarkerHome, EnvMarkerGeneration} {
		if other != marker {
			cases = append(cases, readCase{name: "wrong_missing_" + other, queryErr: &exec.ExitError{Stderr: []byte("unknown variable: " + other)}, wantError: true})
		}
	}
	for _, tc := range cases {
		t.Run(marker+"/"+tc.name, func(t *testing.T) {
			var calls [][]string
			executor := cmd_test.MockCmdExec{OutputFunc: func(c *exec.Cmd) ([]byte, error) {
				calls = append(calls, append([]string(nil), c.Args[1:]...))
				if len(calls) == 1 {
					return []byte(tc.output), tc.queryErr
				}
				return []byte(marker + "=forged\n"), tc.fallbackErr
			}}
			value, present, err := read(executor, "af_marker_test")
			require.Equal(t, tc.wantPresent, present)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			wantValue := ""
			if tc.name == "present" {
				wantValue = "value"
			}
			require.Equal(t, wantValue, value)
			wantCalls := [][]string{{"show-environment", "-t", exactTarget("af_marker_test"), marker}}
			if tc.wantFallback {
				wantCalls = append(wantCalls, []string{"show-environment", "-t", exactTarget("af_marker_test")})
			}
			require.Equal(t, wantCalls, calls)
		})
	}
}

// All marker constants cross the same present/absent/error contract, including
// AF_SESSION (currently only consumed from process environments).
func TestSessionEnvMarkerCrossProduct(t *testing.T) {
	for _, marker := range []string{EnvMarkerSession, EnvMarkerHome, EnvMarkerGeneration} {
		testSessionMarkerRead(t, marker, func(executor cmd.Executor, name string) (string, bool, error) {
			return sessionEnvMarker(executor, name, marker)
		})
	}
}

func TestMissingSessionEnvMarkerCrossProduct(t *testing.T) {
	markers := []string{EnvMarkerSession, EnvMarkerHome, EnvMarkerGeneration, "AF_FUTURE_MARKER"}
	for _, marker := range markers {
		t.Run(marker, func(t *testing.T) {
			require.False(t, missingSessionEnvMarker(nil, marker))
			require.False(t, missingSessionEnvMarker(errors.New("unknown variable: "+marker), marker))
			require.False(t, missingSessionEnvMarker(&exec.ExitError{Stderr: []byte("server unavailable")}, marker))
			for _, reported := range markers {
				err := &exec.ExitError{Stderr: []byte("unknown variable: " + reported + "\n")}
				require.Equal(t, marker == reported, missingSessionEnvMarker(err, marker), "reported %s", reported)
			}
		})
	}
}
