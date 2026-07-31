//go:build !linux && !darwin

package git

func renameAtNoReplacePlatform(_ int, _ string, _ int, _ string) error {
	return errAtomicNoReplaceUnsupported
}
