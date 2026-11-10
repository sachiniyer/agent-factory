//go:build darwin

package execgroup

import "golang.org/x/sys/unix"

// szomb is SZOMB from <sys/proc.h>: the p_stat of a process that has exited
// and is waiting for its parent to collect it. x/sys/unix exposes the P_stat
// field but not the state constants.
const szomb = 5

// groupHasLiveMember reports whether any process in the group pgid leads is
// alive, which here means not a zombie. kern.proc.pgrp lists a group's zombies
// as well, and a zombie leader stays in its group until os/exec's Wait reaps
// it. XNU's fork is not atomic against a group signal, so killGroup re-signals
// until this reports false (see killGroup).
var groupHasLiveMember = func(pgid int) (bool, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, err
	}
	for i := range kps {
		if kps[i].Proc.P_stat != szomb {
			return true, nil
		}
	}
	return false, nil
}
