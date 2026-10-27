package linger

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func setup(t *testing.T, goosVal string) (calls *[][]string) {
	t.Helper()
	oldG, oldU, oldR, oldD, oldS := goos, currentUser, runCommand, dir, statFile
	t.Cleanup(func() { goos, currentUser, runCommand, dir, statFile = oldG, oldU, oldR, oldD, oldS })
	goos = func() string { return goosVal }
	currentUser = func() (*user.User, error) { return &user.User{Username: "alice"}, nil }
	dir = t.TempDir()
	c := [][]string{}
	runCommand = func(name string, args ...string) ([]byte, error) {
		c = append(c, append([]string{name}, args...))
		return nil, nil
	}
	return &c
}

func TestEnsureNoopOffLinux(t *testing.T) {
	calls := setup(t, "darwin")
	require.NoError(t, Ensure())
	require.Empty(t, *calls)
	require.False(t, Applies())
}

func TestEnsureAlreadyLingering(t *testing.T) {
	calls := setup(t, "linux")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "alice"), nil, 0o644))
	require.NoError(t, Ensure())
	require.Empty(t, *calls)
}

func TestEnsureEnablesLinger(t *testing.T) {
	calls := setup(t, "linux")
	runCommand = func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		return nil, os.WriteFile(filepath.Join(dir, "alice"), nil, 0o644)
	}
	require.NoError(t, Ensure())
	require.Equal(t, [][]string{{"loginctl", "enable-linger", "alice"}}, *calls)
}

func TestEnsureRefusesWithRemedy(t *testing.T) {
	setup(t, "linux")
	runCommand = func(string, ...string) ([]byte, error) {
		return []byte("Interactive authentication required."), errors.New("exit status 1")
	}
	err := Ensure()
	require.Error(t, err)
	require.Contains(t, err.Error(), "sudo loginctl enable-linger alice")
	require.Contains(t, err.Error(), "Interactive authentication required.")
}

func TestEnsureRefusesWhenSuccessDidNotTake(t *testing.T) {
	setup(t, "linux")
	err := Ensure()
	require.ErrorContains(t, err, "still absent")
}
