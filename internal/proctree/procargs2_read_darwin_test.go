//go:build darwin

package proctree

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarwinProcArgsReadersPreserveExitAndRefusal(t *testing.T) {
	previous := procArgsSysctl
	t.Cleanup(func() { procArgsSysctl = previous })
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ESRCH, syscall.EACCES, syscall.EPERM} {
		t.Run(errno.Error(), func(t *testing.T) {
			procArgsSysctl = func(name string, args ...int) ([]byte, error) {
				require.Equal(t, "kern.procargs2", name)
				require.Equal(t, []int{42}, args)
				return nil, errno
			}
			argv, err := readArgv(42)
			require.Nil(t, argv)
			require.ErrorIs(t, err, errno)
			env, envErr := Environ(42)
			require.Nil(t, env)
			require.ErrorIs(t, envErr, ErrEnvUnreadable)
			require.ErrorIs(t, envErr, errno)
			if errno == syscall.EIO || errno == syscall.ESRCH {
				require.ErrorIs(t, err, ErrProcessExited)
				require.ErrorIs(t, envErr, ErrProcessExited)
			} else {
				require.NotErrorIs(t, err, ErrProcessExited)
				require.NotErrorIs(t, envErr, ErrProcessExited)
			}
		})
	}
}
