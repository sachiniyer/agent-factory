//go:build darwin

package execgroup

import "golang.org/x/sys/unix"

// szomb is SZOMB from <sys/proc.h>: the p_stat of a process that has exited
// and is waiting for its parent to collect it. x/sys/unix exposes the P_stat
// field but not the state constants.
const szomb = 5

// readGroup lists the group pgid leads through kern.proc.pgrp, zombies
// included: a zombie leader stays in its group until os/exec's Wait reaps it.
// XNU's fork is not atomic against a group signal, so killGroup sweeps the
// group with it (see killGroup). The read is not a snapshot, and it can
// block; resignal and sweep are built around both.
var readGroup groupReader = func(pgid int) ([]groupMember, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, err
	}
	members := make([]groupMember, 0, len(kps))
	for i := range kps {
		members = append(members, groupMember{
			pid:    int(kps[i].Proc.P_pid),
			zombie: kps[i].Proc.P_stat == szomb,
		})
	}
	return members, nil
}
