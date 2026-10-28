package session

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Write-back is a versioned compare-and-swap from a bounded stream (#5864, #5865).
// These tables run the interleavings two sessions on ONE registry account can
// produce, against a real filesystem.

type sharedAccount struct {
	t        *testing.T
	registry string // the registry's credential file
	a, b     *sandboxWorkspace
}

func newSharedAccount(t *testing.T) *sharedAccount {
	t.Helper()
	acct := writeAccount(t, "claude", "v0")
	s := &sharedAccount{t: t, registry: filepath.Join(acct.Dir, ".credentials.json"),
		a: workspaceOn(t, acct, "box1"), b: workspaceOn(t, acct, "box1")}
	require.NoError(t, s.a.syncAccountIn(time.Minute))
	require.NoError(t, s.b.syncAccountIn(time.Minute))
	return s
}

// refresh is the agent on the host rotating its token inside its own scope.
func (s *sharedAccount) refresh(w *sandboxWorkspace, token string) {
	s.t.Helper()
	require.NoError(s.t, os.WriteFile(filepath.Join(w.AccountDir(), ".credentials.json"), []byte(token), 0o600))
}

func (s *sharedAccount) reg() string {
	s.t.Helper()
	b, err := os.ReadFile(s.registry)
	require.NoError(s.t, err)
	return string(b)
}

func (s *sharedAccount) writeBack(w *sandboxWorkspace) accountWriteBack {
	s.t.Helper()
	res, err := w.writeBackAccount(time.Minute)
	require.NoError(s.t, err)
	return res
}

func TestWriteBackInterleavingsNeverLetAStaleSnapshotWin(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, s *sharedAccount)
	}{
		{"A refreshes, then B (unchanged, stale) tears down", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			assert.Equal(t, 1, s.writeBack(s.a).Written)
			res := s.writeBack(s.b) // B's host copy is still v0: not a refresh
			assert.Zero(t, res.Written)
			assert.Empty(t, res.Skipped, "an unchanged file is not news, so it is not even a conflict")
			assert.Equal(t, "A1", s.reg(), "B's stale snapshot must not roll A's refresh back (#5864)")
		}},
		{"B tears down first (unchanged), then A refreshes", func(t *testing.T, s *sharedAccount) {
			assert.Zero(t, s.writeBack(s.b).Written)
			assert.Equal(t, "v0", s.reg())
			s.refresh(s.a, "A1")
			assert.Equal(t, 1, s.writeBack(s.a).Written)
			assert.Equal(t, "A1", s.reg())
		}},
		{"both refreshed: the second writer loses, visibly", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			s.refresh(s.b, "B1")
			assert.Equal(t, 1, s.writeBack(s.a).Written)
			res := s.writeBack(s.b)
			assert.Zero(t, res.Written)
			assert.Equal(t, []string{".credentials.json"}, res.Skipped, "the skip is reported, not silent")
			assert.Equal(t, "A1", s.reg(), "a fresher registry is never overwritten by an older baseline")
		}},
		{"the same, in the other order", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			s.refresh(s.b, "B1")
			assert.Equal(t, 1, s.writeBack(s.b).Written)
			assert.Equal(t, []string{".credentials.json"}, s.writeBack(s.a).Skipped)
			assert.Equal(t, "B1", s.reg())
		}},
		{"a session keeps writing back from what it last wrote", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			assert.Equal(t, 1, s.writeBack(s.a).Written)
			s.refresh(s.a, "A2")
			assert.Equal(t, 1, s.writeBack(s.a).Written, "the baseline advanced with the write")
			assert.Equal(t, "A2", s.reg())
			assert.Zero(t, s.writeBack(s.a).Written, "and an unchanged file after that is not a refresh")
		}},
		{"the account was removed: never resurrected", func(t *testing.T, s *sharedAccount) {
			require.NoError(t, os.Remove(s.registry))
			s.refresh(s.a, "A1")
			res := s.writeBack(s.a)
			assert.Zero(t, res.Written)
			assert.Equal(t, []string{".credentials.json"}, res.Skipped)
			assert.NoFileExists(t, s.registry)
		}},
		{"a process with no sync-in baseline writes nothing back", func(t *testing.T, s *sharedAccount) {
			s.a.syncedVersion = nil
			s.refresh(s.a, "A1")
			res := s.writeBack(s.a)
			assert.Zero(t, res.Written)
			assert.Equal(t, []string{".credentials.json"}, res.Skipped)
			assert.Equal(t, "v0", s.reg())
		}},
		{"teardown goes through the same rule", func(t *testing.T, s *sharedAccount) {
			s.refresh(s.a, "A1")
			require.NoError(t, s.a.teardownAccount(time.Minute))
			require.NoError(t, s.b.teardownAccount(time.Minute))
			assert.Equal(t, "A1", s.reg())
			assert.NoDirExists(t, s.a.AccountDir())
			assert.NoDirExists(t, s.b.AccountDir())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newSharedAccount(t)) })
	}
}

// Two teardowns racing, both holding a refresh: exactly one wins, whole — the
// registry is never a mix and never lost.
func TestConcurrentTeardownsPickExactlyOneWinner(t *testing.T) {
	for i := 0; i < 25; i++ {
		s := newSharedAccount(t)
		s.refresh(s.a, "A1")
		s.refresh(s.b, "B1")
		var wg sync.WaitGroup
		results := make([]accountWriteBack, 2)
		for n, w := range []*sandboxWorkspace{s.a, s.b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := w.writeBackAccount(time.Minute)
				assert.NoError(t, err)
				results[n] = res
			}()
		}
		wg.Wait()
		assert.Equal(t, 1, results[0].Written+results[1].Written, "exactly one write-back lands")
		assert.Equal(t, 1, len(results[0].Skipped)+len(results[1].Skipped), "the other is a reported skip")
		assert.Contains(t, []string{"A1", "B1"}, s.reg())
	}
}

// --- the bound ---

// archiveOf builds a tar of the named files.
func archiveOf(t *testing.T, files map[string][]byte, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range order {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(files[name])), Typeflag: tar.TypeReg}))
		_, err := tw.Write(files[name])
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// A host that sends an oversized or truncated archive is refused whole, and the
// registry is left exactly as it was — nothing is written until every entry parsed.
func TestWriteBackRefusesAnOversizedOrTruncatedArchive(t *testing.T) {
	acct := writeAccount(t, "gemini", "g0")
	creds := map[string][]byte{}
	rels := accountCredentialRel["gemini"]
	for _, rel := range rels {
		creds[rel] = []byte("refreshed-" + filepath.Base(rel))
	}
	good := archiveOf(t, creds, rels)

	for _, tc := range []struct {
		name string
		out  []byte
		want string
	}{
		{"oversized: more bytes than the bound", bytes.Repeat([]byte("x"), int(accountArchiveLimit("gemini"))+10), "bound"},
		{"truncated mid-entry", good[:len(good)-700], "unreadable archive"},
		{"truncated after the first entry", good[:512+512+512], ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &sandboxWorkspace{shell: fixedOutputShell{out: tc.out}, SessionDir: "/s",
				spec:          ProvisionSpec{Account: acct, AccountSyncHost: "box1"},
				syncedVersion: map[string]string{}}
			for _, rel := range rels {
				b, _ := os.ReadFile(filepath.Join(acct.Dir, filepath.FromSlash(rel)))
				if b != nil {
					w.syncedVersion[rel] = accountVersion(b)
				}
			}
			before, _ := os.ReadFile(filepath.Join(acct.Dir, rels[0]))
			_, err := w.writeBackAccount(time.Second)
			require.Error(t, err)
			if tc.want != "" {
				assert.Contains(t, err.Error(), tc.want)
			}
			after, _ := os.ReadFile(filepath.Join(acct.Dir, rels[0]))
			assert.Equal(t, string(before), string(after), "a refused archive writes nothing, not even its valid entries")
		})
	}
}

// The bound holds WHILE STREAMING: past the limit the writer refuses and cancels,
// and never holds more than the limit.
func TestLimitWriterNeverHoldsPastItsLimit(t *testing.T) {
	cancelled := false
	lw := &limitWriter{limit: 100, cancel: func() { cancelled = true }}
	n, err := lw.Write(bytes.Repeat([]byte("a"), 60))
	require.NoError(t, err)
	assert.Equal(t, 60, n)
	n, err = lw.Write(bytes.Repeat([]byte("b"), 60))
	require.ErrorIs(t, err, ErrOutputOverLimit)
	assert.Equal(t, 40, n, "only the room that was left is accepted")
	assert.True(t, cancelled, "the command is cancelled the moment the limit is crossed")
	assert.LessOrEqual(t, lw.buf.Len(), 100)
	_, err = lw.Write([]byte("more"))
	require.ErrorIs(t, err, ErrOutputOverLimit)
	assert.LessOrEqual(t, lw.buf.Len(), 100)
}

// The production path: a command that would stream forever is killed at the bound,
// through the real command composition, and returns promptly with the bound error.
func TestRunLimitedKillsAnEndlessStreamAtTheBound(t *testing.T) {
	p := &sandboxProvisioner{sshCmd: "sh -c"} // runs the script locally, same composition as ssh
	start := time.Now()
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = p.RunLimited(30*time.Second, "yes x", 4096)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("RunLimited did not return: the endless stream was not killed at the bound")
	}
	require.ErrorIs(t, err, ErrOutputOverLimit)
	assert.Nil(t, out, "nothing past the bound is returned")
	assert.Less(t, time.Since(start), 15*time.Second)

	// Within the bound it behaves like Run.
	ok, err := p.RunLimited(30*time.Second, "printf hello", 4096)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(ok))
}

// Reaching the bound through the shell interface is the same refusal.
type overLimitShell struct{}

func (overLimitShell) Run(time.Duration, string, io.Reader, bool) ([]byte, error) {
	return nil, errors.New("unused")
}
func (overLimitShell) RunLimited(time.Duration, string, int64) ([]byte, error) {
	return nil, ErrOutputOverLimit
}

func TestWriteBackUsesTheBoundedStreamWhenTheShellOffersIt(t *testing.T) {
	acct := writeAccount(t, "claude", "v0")
	w := &sandboxWorkspace{shell: overLimitShell{}, SessionDir: "/s", spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1"}}
	_, err := w.writeBackAccount(time.Second)
	require.ErrorIs(t, err, ErrOutputOverLimit)
	_ = context.Background()
}
