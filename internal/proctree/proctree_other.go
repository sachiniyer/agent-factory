//go:build !linux && !darwin

package proctree

import "time"

// This file is the deliberate dead end for any platform with no process-table
// backend, and it exists to be LOUD.
//
// The bug this package was built out of (#1939) was not that darwin lacked
// /proc — it was that nothing said so. proctree was Linux-only with no build
// tag, so on darwin every read failed, every caller swallowed the failure, and
// `af doctor` reported a clean bill of health from a process table it had
// never managed to read. The code compiled everywhere and worked in one place.
//
// So: a new GOOS gets a compile-time fact, not a silent no-op. Either add a
// real backend for it, or let it fail here where the error names the platform.
// Do not "fix" a build failure in this file by returning empty values — an
// empty process table is a claim that nothing is running, and this platform is
// in no position to make that claim.

func snapshot() (map[int]Process, error) {
	return nil, ErrUnsupportedPlatform
}

func readProc(int) (Process, error) {
	return Process{}, ErrUnsupportedPlatform
}

func readEnviron(int) ([]string, error) {
	return nil, ErrUnsupportedPlatform
}

func readArgv(int) ([]string, error) {
	return nil, ErrUnsupportedPlatform
}

func readCPUTime(int) (time.Duration, error) {
	return 0, ErrUnsupportedPlatform
}

func readUID(int) (int, bool) {
	return 0, false
}

func readWorkingDir(int) (string, bool) {
	return "", false
}
