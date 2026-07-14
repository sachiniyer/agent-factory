package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// TestEnsureTokenConcurrent proves the #1720 TOCTOU is gone: many goroutines
// racing on a fresh (missing) token path must all agree on one token, and that
// token must be the one persisted. On the pre-fix code (LoadToken-miss →
// generate → os.WriteFile with no lock) concurrent callers each generated a
// different token and clobbered each other, so the goroutines disagreed and/or
// the persisted file differed from what a given caller returned.
func TestEnsureTokenConcurrent(t *testing.T) {
	const iterations = 100
	const goroutines = 12

	for i := 0; i < iterations; i++ {
		// Fresh path per iteration so every run starts from "no token".
		path := filepath.Join(t.TempDir(), "daemon-token")

		var start sync.WaitGroup
		start.Add(1)
		var done sync.WaitGroup
		results := make([]string, goroutines)
		errs := make([]error, goroutines)

		for g := 0; g < goroutines; g++ {
			done.Add(1)
			go func(idx int) {
				defer done.Done()
				start.Wait() // released together for maximum contention
				tok, err := EnsureToken(path)
				results[idx] = tok
				errs[idx] = err
			}(g)
		}

		start.Done() // release the barrier
		done.Wait()

		want := results[0]
		for g := 0; g < goroutines; g++ {
			if errs[g] != nil {
				t.Fatalf("iter %d goroutine %d: EnsureToken error: %v", i, g, errs[g])
			}
			if results[g] == "" {
				t.Fatalf("iter %d goroutine %d: EnsureToken returned empty token", i, g)
			}
			if results[g] != want {
				t.Fatalf("iter %d: goroutines disagree on token: %q != %q (TOCTOU race)", i, results[g], want)
			}
		}

		// The persisted file must match the token every caller agreed on.
		loaded, err := LoadToken(path)
		if err != nil {
			t.Fatalf("iter %d: LoadToken after concurrent EnsureToken: %v", i, err)
		}
		if loaded != want {
			t.Fatalf("iter %d: persisted token %q != agreed token %q", i, loaded, want)
		}

		// Perms must stay 0600 through the concurrent generate path.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("iter %d: stat token file: %v", i, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("iter %d: token file perms = %o, want 0600", i, perm)
		}
	}
}

// TestRotateTokenConcurrentReaderNeverEmpty proves the #1722 rotation-401
// window is gone: while one goroutine rotates the token many times, reader
// goroutines tight-loop LoadToken and must NEVER observe an error or a
// malformed token — every read returns a well-formed 43-char token (the whole
// old or whole new value). On the pre-fix code (os.WriteFile O_TRUNC) a reader
// could catch the file truncated-but-not-rewritten and LoadToken returned the
// "token file ... is empty" error, which is exactly the spurious 401 the auth
// gate produced.
func TestRotateTokenConcurrentReaderNeverEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-token")

	if _, err := EnsureToken(path); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}

	const rotations = 300
	const readers = 4

	stop := make(chan struct{})
	var wg sync.WaitGroup

	var readErr error
	var readErrOnce sync.Once
	recordErr := func(err error) {
		readErrOnce.Do(func() { readErr = err })
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tok, err := LoadToken(path)
				if err != nil {
					recordErr(err)
					return
				}
				if len(tok) != 43 {
					recordErr(fmt.Errorf("reader saw malformed token %q (len %d)", tok, len(tok)))
					return
				}
			}
		}()
	}

	for i := 0; i < rotations; i++ {
		if _, err := RotateToken(path); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("RotateToken iteration %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if readErr != nil {
		t.Fatalf("concurrent reader observed a bad token during rotation: %v", readErr)
	}

	// After all the churn the file must still be a valid 0600 token.
	loaded, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken after rotations: %v", err)
	}
	if len(loaded) != 43 {
		t.Fatalf("final token malformed: %q (len %d)", loaded, len(loaded))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file perms after rotation = %o, want 0600", perm)
	}
}

// TestEnsureTokenCreatesHome0700 is the Greptile-follow-up regression guard: on
// a fresh machine where token generation is the first thing to touch the AF
// home, the home dir must be created 0700 (owner-only) — it holds secrets
// (daemon-token, state.json). The serialize+atomic fix routes
// creation through config.WithFileLock, whose MkdirAll is 0755 (correct for its
// non-secret callers), so EnsureToken must pre-create the parent 0700 before the
// lock. On the pre-follow-up head the parent landed 0755.
func TestEnsureTokenCreatesHome0700(t *testing.T) {
	// Parent AF-home dir does NOT exist yet; EnsureToken must create it.
	home := filepath.Join(t.TempDir(), "af-home")
	path := filepath.Join(home, "daemon-token")

	if _, err := EnsureToken(path); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}

	info, err := os.Stat(home)
	if err != nil {
		t.Fatalf("stat AF home: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("AF home perms after first-run EnsureToken = %o, want 0700", perm)
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
