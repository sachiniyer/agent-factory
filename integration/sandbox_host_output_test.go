package integration_test

import "testing"

// TestHostStdoutKeepsPorcelainColumns pins the property the workspace fingerprints
// rely on: output is compared exactly as git prints it, so an unstaged edit
// (' M f') never reads as a staged one ('M  f').
func TestHostStdoutKeepsPorcelainColumns(t *testing.T) {
	unstaged := hostStdout([]byte(" M e2e-work.txt\nA  staged.txt\n"))
	staged := hostStdout([]byte("M  e2e-work.txt\nA  staged.txt\n"))
	if unstaged == staged {
		t.Fatalf("a fingerprint whose unstaged edit became staged compares equal:\n%q", unstaged)
	}
	if want := " M e2e-work.txt\nA  staged.txt"; unstaged != want {
		t.Fatalf("hostStdout = %q, want %q (leading column kept, trailing newline dropped)", unstaged, want)
	}
}
