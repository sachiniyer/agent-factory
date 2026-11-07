package proctree

// OpenFilePaths returns the absolute filesystem paths of every regular-file
// descriptor pid currently holds open, keyed for membership tests. Sockets,
// pipes, and other non-path descriptors are absent, not empty strings.
//
// The contract is "the truth or an error", never a guess: a process that
// provably holds nothing returns an empty map and nil error, while a process
// that cannot be read — exited, or refusing — returns an error so callers
// can tell "seen, empty" from "could not see". ErrProcessExited (and a
// process-table miss) mean gone; anything else means refused.
func OpenFilePaths(pid int) (map[string]struct{}, error) {
	return openFilePaths(pid)
}
