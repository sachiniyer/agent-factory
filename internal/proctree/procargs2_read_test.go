package proctree

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcArgs2ScanSkipsExitedAndRefusesUnreadable(t *testing.T) {
	const livePID, targetPID = 42, 43
	for _, tc := range []struct {
		name      string
		err       error
		lookupErr error
		gone      bool
	}{
		{"exits during read", syscall.EIO, nil, true},
		{"already absent", syscall.ESRCH, nil, true},
		{"wrapped exit", fmt.Errorf("sysctl: %w", syscall.EIO), nil, true},
		{"zombie EINVAL", syscall.EINVAL, ErrProcessExited, true},
		{"missing EINVAL", syscall.EINVAL, os.ErrNotExist, true},
		{"live EINVAL refusal", syscall.EINVAL, nil, false},
		{"unknown EINVAL identity", syscall.EINVAL, syscall.EACCES, false},
		{"permission denied", syscall.EACCES, nil, false},
		{"operation not permitted", syscall.EPERM, nil, false},
		{"other failure", syscall.ENOMEM, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(pid int) (Process, error) {
				require.Equal(t, targetPID, pid)
				return Process{PID: pid}, tc.lookupErr
			}
			sysctl := func(name string, args ...int) ([]byte, error) {
				require.Equal(t, "kern.procargs2", name)
				require.Len(t, args, 1)
				if args[0] == targetPID {
					return nil, tc.err
				}
				require.Equal(t, livePID, args[0])
				return buildProcArgs2("/usr/bin/sleep", []string{"sleep", "300"}, []string{"AF_SESSION=test"}, 1), nil
			}
			read := func(pid int) ([]string, error) {
				argv, _, err := readProcArgs2(pid, sysctl, lookup)
				return argv, err
			}
			_, readErr := read(targetPID)
			require.Equal(t, tc.gone, errors.Is(readErr, ErrProcessExited), "sysctl error must classify at the Darwin boundary")
			stubScan(t, fixedSnapshot(livePID, targetPID), read)
			stubScanUID(t, map[int]int{livePID: os.Getuid(), targetPID: os.Getuid()})
			matched, err := ProcessesMatchingArgv(func(argv []string) bool { return argv[0] == "sleep" })
			if tc.gone {
				require.NoError(t, err)
				require.Len(t, matched, 1)
				require.Equal(t, livePID, matched[0].Process.PID)
			} else {
				var refused *ArgvUnreadableError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, targetPID, refused.PID)
				require.ErrorIs(t, err, tc.err)
				require.Empty(t, matched, "a refused read must never return a partial scan")
			}
		})
	}
}
