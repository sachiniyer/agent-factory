//go:build !linux && !darwin

package git

import "os"

// No creation pin is available off linux/darwin: there is no portable inode
// birth time on the remaining platforms. Probes answer 0, so a retained-tree
// record captures CreationTime 0 and the verdict falls back to exactly what
// dev+ino+file-type prove — the documented accepted gap that "rm plus
// recreate at the same path reads as tracked" here (#7900).
func creationPinAt(_ *os.File, _ string) (int64, error) {
	return 0, nil
}

func creationPinFile(_ *os.File) (int64, error) {
	return 0, nil
}
