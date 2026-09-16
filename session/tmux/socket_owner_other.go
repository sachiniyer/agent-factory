//go:build !linux

package tmux

// attestedSocketOwnerPIDs is nil everywhere but Linux: there is no
// /proc/net/unix to read socket ownership from, and the proc_pidfdinfo fd
// walk that would substitute on darwin is machinery this fix does not need —
// the unattested answer (every verified same-uid server) is the conservative
// set #2875 chose, now with clients excluded at construction and refused
// again at signal time (#4349).
func attestedSocketOwnerPIDs(string, []int) []int { return nil }
