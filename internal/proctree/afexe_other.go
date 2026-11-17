//go:build !linux && !darwin

package proctree

import "os"

// exeOpen has no kernel source on platforms without a backend; "cannot
// resolve" is the honest answer, and the attestation built on it reports
// unverified — the fail-closed direction for a cleanup-authorizing verdict.
func exeOpen(pid int) (*os.File, bool) {
	return nil, false
}

// exeIdentity reports no identity on platforms without an attestation
// backend: there is no kernel exe source to read, so a cached positive can
// never be confirmed — the memo re-attests, which itself reports
// unverified, the fail-closed direction.
func exeIdentity(pid int) (exeID, bool) {
	return exeID{}, false
}
