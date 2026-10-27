//go:build darwin

package daemon

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestKqueueExitResult: only a NOTE_EXIT event, or a registration that found
// the child already gone, counts as the exit. A registration failure reported
// as an EV_ERROR event is an error, not an exit — treating it as one would
// SIGKILL a leader that is still running.
func TestKqueueExitResult(t *testing.T) {
	exit := unix.Kevent_t{Filter: unix.EVFILT_PROC, Fflags: unix.NOTE_EXIT}
	evError := func(errno unix.Errno) unix.Kevent_t {
		return unix.Kevent_t{Filter: unix.EVFILT_PROC, Flags: unix.EV_ERROR, Data: int64(errno)}
	}
	cases := []struct {
		name       string
		n          int
		ev         unix.Kevent_t
		err        error
		wantExited bool
		wantErr    error
	}{
		{"note exit", 1, exit, nil, true, nil},
		{"already gone at registration", 0, unix.Kevent_t{}, unix.ESRCH, true, nil},
		{"interrupted", 0, unix.Kevent_t{}, unix.EINTR, false, nil},
		{"kevent failed", 0, unix.Kevent_t{}, unix.EBADF, false, unix.EBADF},
		{"registration failed as an event", 1, evError(unix.ENOMEM), nil, false, unix.ENOMEM},
		{"already gone, as an event", 1, evError(unix.ESRCH), nil, true, nil},
		{"interrupted, as an event", 1, evError(unix.EINTR), nil, false, nil},
		{"error flag without an errno", 1, evError(0), nil, false, nil},
		{"no event", 0, unix.Kevent_t{}, nil, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exited, err := kqueueExitResult(c.n, &c.ev, c.err)
			if exited != c.wantExited || err != c.wantErr {
				t.Fatalf("kqueueExitResult = (%v, %v), want (%v, %v)", exited, err, c.wantExited, c.wantErr)
			}
		})
	}
}
