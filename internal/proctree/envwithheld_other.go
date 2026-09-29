//go:build !darwin

package proctree

// withheldEnvCause is always false off darwin. Linux has no same-uid
// redaction: /proc/<pid>/environ is either readable or an error, so an empty
// read there really is an empty environment.
func withheldEnvCause(int) (string, bool) { return "", false }
