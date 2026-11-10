//go:build !darwin

package execgroup

// groupHasLiveMember is nil here: Linux's fork is atomic against a group
// signal, so killGroup's first kill reaches a child the group is forking, and
// there is nothing to re-signal (see killGroup).
var groupHasLiveMember func(pgid int) (bool, error)
