//go:build !linux && !darwin

package proctree

import "os"

// exeOpen has no kernel source on platforms without a backend; "cannot
// resolve" is the honest answer, and the attestation built on it reports
// unverified — the fail-closed direction for a cleanup-authorizing verdict.
func exeOpen(pid int) (*os.File, bool) {
	return nil, false
}
