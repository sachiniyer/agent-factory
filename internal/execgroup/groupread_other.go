//go:build !darwin

package execgroup

// readGroup is nil here: Linux's fork is atomic against a group signal, so
// killGroup's first kill reaches a child the group is forking, and there is
// nothing to sweep (see killGroup).
var readGroup groupReader
