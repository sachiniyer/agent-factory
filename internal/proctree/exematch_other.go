//go:build !linux && !darwin

package proctree

// exeImageOfFile and exeImageForMatch have no kernel source on platforms
// without a backend: "cannot establish" is the honest answer, so a caller
// that resolves its reference through ExeImageOf fails here first and the
// exe route never engages — the platform keeps its title-only answer.
func exeImageOfFile(string) (ExeImage, error) { return ExeImage{}, errExeImageUnreadable }
func exeImageForMatch(int) (ExeImage, error)  { return ExeImage{}, errExeImageUnreadable }
