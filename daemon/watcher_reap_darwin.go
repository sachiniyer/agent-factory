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
		n, err := unix.Kevent(kq, []unix.Kevent_t{change}, events, nil)
		if exited, err := kqueueExitResult(n, &events[0], err); exited || err != nil {
			return err
		}
	}
}

// kqueueExitResult reads one kevent call of waitExitedUnreaped. exited means
// the child has exited; err means its exit cannot be awaited this way; neither
// means call again. A registration that fails can come back as an EV_ERROR
// event with the errno in Data while kevent itself succeeds, so the event is
// checked for that before it counts as the exit.
func kqueueExitResult(n int, ev *unix.Kevent_t, err error) (exited bool, _ error) {
	if err == nil && n > 0 && ev.Flags&unix.EV_ERROR != 0 {
		if ev.Data == 0 {
			return false, nil
		}
		err = unix.Errno(ev.Data)
	}
	switch {
	case err == unix.EINTR:
		return false, nil
	case err == unix.ESRCH:
		// The registration found no live process: our child has already
		// exited, and since only we reap it, it is still unreaped.
		return true, nil
	case err != nil:
		return false, err
	}
	return n > 0 && ev.Filter == unix.EVFILT_PROC && ev.Fflags&unix.NOTE_EXIT != 0, nil
}
