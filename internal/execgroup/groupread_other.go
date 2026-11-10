//go:build !darwin

package execgroup

// readGroup is nil on every platform but darwin, and af ships only for
// linux and darwin. On Linux it has nothing to do: fork is atomic against a
// group signal, so killGroup's first kill reaches a child the group is
// forking, and a kill of a group of zombies succeeds, so EPERM is always a
// refusal (see killGroup). Another platform would need its own answer to
// both questions, not this nil.
var readGroup groupReader
