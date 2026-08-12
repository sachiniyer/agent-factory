//go:build darwin

package git

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSetCleanupGenerationCreate_DoesNotReplaceDarwinValue(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open directory: %v", err)
	}
	defer directory.Close()

	first := []byte("0123456789abcdef0123456789abcdef")
	if err := setCleanupGenerationCreate(int(directory.Fd()), cleanupGenerationXattr, first); err != nil {
		t.Fatalf("create cleanup generation: %v", err)
	}
	if err := setCleanupGenerationCreate(
		int(directory.Fd()), cleanupGenerationXattr, []byte("fedcba9876543210fedcba9876543210"),
	); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("second create replaced the existing generation: %v", err)
	}
	stored, err := cleanupGenerationFromFile(directory)
	if err != nil {
		t.Fatalf("read cleanup generation: %v", err)
	}
	if stored != string(first) {
		t.Fatalf("create-only write replaced generation: got %q want %q", stored, first)
	}
}
