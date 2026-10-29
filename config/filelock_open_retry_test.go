package config

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Acquiring the lock never fails because of another writer: a transient ENOENT
// or EINTR from the O_CREAT open is retried, while any other error, and an ENOENT
// that never clears, still surface.
func TestOpenLockWithRetry(t *testing.T) {
	prevN, prevB := lockOpenAttempts, lockOpenBackoff
	lockOpenAttempts, lockOpenBackoff = 5, time.Microsecond
	t.Cleanup(func() { lockOpenAttempts, lockOpenBackoff = prevN, prevB })

	for _, tc := range []struct {
		name      string
		failures  []error
		wantCalls int
		wantErr   error
	}{
		{name: "first try", wantCalls: 1},
		{name: "transient ENOENT then success", failures: []error{syscall.ENOENT, syscall.ENOENT}, wantCalls: 3},
		{name: "transient EINTR then success", failures: []error{syscall.EINTR}, wantCalls: 2},
		{name: "permission error is not retried", failures: []error{syscall.EACCES}, wantCalls: 1, wantErr: syscall.EACCES},
		{name: "ENOENT that never clears fails after the budget", failures: []error{syscall.ENOENT, syscall.ENOENT, syscall.ENOENT, syscall.ENOENT, syscall.ENOENT, syscall.ENOENT}, wantCalls: 5, wantErr: syscall.ENOENT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := openLockWithRetry(func() (*os.File, error) {
				calls++
				if calls <= len(tc.failures) {
					return nil, &os.PathError{Op: "open", Path: "x.lock", Err: tc.failures[calls-1]}
				}
				return nil, nil
			})
			require.Equal(t, tc.wantCalls, calls)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.True(t, errors.Is(err, tc.wantErr), "got %v", err)
		})
	}
}
