package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateTokenUniqueAndStrong(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := generateToken()
		if err != nil {
			t.Fatalf("generateToken: %v", err)
		}
		// base64url of 32 bytes, no padding => 43 chars.
		if len(tok) != 43 {
			t.Fatalf("token length = %d, want 43 (%q)", len(tok), tok)
		}
		if seen[tok] {
			t.Fatalf("duplicate token generated: %q", tok)
		}
		seen[tok] = true
	}
}

func TestEnsureTokenGeneratesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-token")

	tok, err := EnsureToken(path)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if tok == "" {
		t.Fatal("EnsureToken returned an empty token")
	}

	// The file must exist and be 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file perms = %o, want 0600", perm)
	}

	// Round-trip: LoadToken returns the same value.
	loaded, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if loaded != tok {
		t.Fatalf("LoadToken = %q, want %q", loaded, tok)
	}
}

func TestEnsureTokenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-token")

	first, err := EnsureToken(path)
	if err != nil {
		t.Fatalf("EnsureToken (first): %v", err)
	}
	second, err := EnsureToken(path)
	if err != nil {
		t.Fatalf("EnsureToken (second): %v", err)
	}
	if first != second {
		t.Fatalf("EnsureToken not idempotent: %q != %q", first, second)
	}
}

func TestLoadTokenAbsentAndEmpty(t *testing.T) {
	dir := t.TempDir()

	// Absent file: an error the auth gate treats as fail-closed.
	if _, err := LoadToken(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("LoadToken on an absent file: want error, got nil")
	} else if !os.IsNotExist(err) {
		t.Fatalf("LoadToken absent: want IsNotExist, got %v", err)
	}

	// Empty/whitespace file: rejected so an empty token never authenticates.
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(empty); err == nil {
		t.Fatal("LoadToken on an empty file: want error, got nil")
	}
}

func TestRotateTokenChangesValueAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-token")

	original, err := EnsureToken(path)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	rotated, err := RotateToken(path)
	if err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	if rotated == original {
		t.Fatal("RotateToken produced the same token")
	}

	// The new token is the one now persisted, and perms stay 0600.
	loaded, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken after rotate: %v", err)
	}
	if loaded != rotated {
		t.Fatalf("persisted token = %q, want rotated %q", loaded, rotated)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("rotated token file perms = %o, want 0600", perm)
	}
}

func TestConstantTimeEqual(t *testing.T) {
	cases := []struct {
		name      string
		got, want string
		expect    bool
	}{
		{"equal", "s3cr3t", "s3cr3t", true},
		{"different", "s3cr3t", "guess", false},
		{"prefix-of", "s3c", "s3cr3t", false},
		{"empty-want-denies", "s3cr3t", "", false},
		{"empty-want-empty-got-denies", "", "", false},
		{"empty-got", "", "s3cr3t", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConstantTimeEqual(tc.got, tc.want); got != tc.expect {
				t.Fatalf("ConstantTimeEqual(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.expect)
			}
		})
	}
}
