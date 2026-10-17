//go:build darwin

package daemon

import "golang.org/x/sys/unix"

// waitExitedUnreaped blocks until pid, a child of this process, has exited,
// and leaves it unreaped: kqueue's NOTE_EXIT reports the exit without the
// wait(2) that would release the pid, and with it the process group's number.
func waitExitedUnreaped(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	change := unix.Kevent_t{}
	unix.SetKevent(&change, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	events := make([]unix.Kevent_t, 1)
	for {
		_, err := unix.Kevent(kq, []unix.Kevent_t{change}, events, nil)
		switch err {
		case nil:
			return nil
		case unix.EINTR:
			continue
		case unix.ESRCH:
			// The registration found no live process: our child has already
			// exited, and since only we reap it, it is still unreaped.
			return nil
		default:
			return err
		}
	}
}
