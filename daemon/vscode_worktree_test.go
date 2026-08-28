package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVSCodeStartOne_WorktreeValidation pins the two distinct failure modes of
// the pre-exec worktree check in startOne. The old implementation collapsed
// them into one branch (err != nil || !fi.IsDir()) and unconditionally wrapped
// err with %w. When the path existed as a non-directory, err was nil and %w
// rendered the Go formatting artifact "%!w(<nil>)" onto the message — and the
// "has it been moved or removed?" hint was wrong for a path that plainly
// exists. The split form wraps the stat error only when os.Stat failed (so
// errors.Is against os.ErrNotExist survives), and emits a plain, honest
// "is not a directory" message with nothing wrapped when the path is a file.
func TestVSCodeStartOne_WorktreeValidation(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)

	socketDir, err := vscodeSocketDir()
	if err != nil {
		t.Fatalf("vscodeSocketDir: %v", err)
	}
	socketPath := filepath.Join(socketDir, "validation"+vscodeSocketExt)

	t.Run("file_is_not_a_directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}

		_, err := v.startOne("repo/session", "instance-id", binary, flavorCodeServer, socketPath, file)
		if err == nil {
			t.Fatal("startOne accepted a file as the worktree; want an error")
		}
		msg := err.Error()
		if strings.Contains(msg, "%!w(") {
			t.Fatalf("error wraps a nil err and emits a formatting artifact: %q", msg)
		}
		if !strings.Contains(msg, "is not a directory") {
			t.Fatalf("error does not name the non-directory worktree: %q", msg)
		}
		if strings.Contains(msg, "moved or removed") {
			t.Fatalf("error hints the worktree was moved/removed, but the path exists: %q", msg)
		}
		if errors.Unwrap(err) != nil {
			t.Fatalf("non-directory error should not wrap a cause (nothing failed): %q (unwrap=%v)", msg, errors.Unwrap(err))
		}
	})

	t.Run("missing_cannot_be_accessed", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")

		_, err := v.startOne("repo/session", "instance-id", binary, flavorCodeServer, socketPath, missing)
		if err == nil {
			t.Fatal("startOne accepted a missing worktree; want an error")
		}
		msg := err.Error()
		if strings.Contains(msg, "%!w(") {
			t.Fatalf("error emits a formatting artifact: %q", msg)
		}
		if !strings.Contains(msg, "cannot be accessed") {
			t.Fatalf("error does not say the worktree cannot be accessed: %q", msg)
		}
		if !strings.Contains(msg, "moved or removed") {
			t.Fatalf("error drops the moved/removed hint for a missing worktree: %q", msg)
		}
		if errors.Unwrap(err) == nil {
			t.Fatalf("missing-worktree error should wrap the stat cause: %q", msg)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing-worktree error is not errors.Is os.ErrNotExist: %q", msg)
		}
	})
}

// TestVSCodeTab_WorktreeIsNotADirectorySurfacesCleanError drives the FULL
// product HTTP path — Manager -> webtab_serve -> WebTabTarget ->
// ensureServerForInstance -> startOne -> the worktree check -> writeHTTPError —
// and asserts the user-facing response body is well-formed. This is the exact
// surface that carried the malformed message in the bug report:
// writeHTTPError folds err.Error() into the JSON envelope, so a nil wrapped with
// %w rendered "%!w(<nil>)" into the iframe's response. The fixture hands the tab
// a real worktree dir; we replace it with a regular file to hit the !IsDir case.
func TestVSCodeTab_WorktreeIsNotADirectorySurfacesCleanError(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, id, tabID, worktree := newVSCodeFixture(t, binary)

	// The path exists but is not a directory — the case whose nil err the old
	// code wrapped with %w.
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("removing the fixture worktree: %v", err)
	}
	if err := os.WriteFile(worktree, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("writing a file in place of the worktree: %v", err)
	}

	mux := newHTTPMux(&controlServer{manager: manager})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, tabID, ""), nil))

	body := rec.Body.String()
	if !strings.Contains(body, "is not a directory") {
		t.Fatalf("the user-facing error body does not name the non-directory worktree: status=%d body=%q", rec.Code, body)
	}
	if strings.Contains(body, "%!w(") {
		t.Fatalf("the user-facing error body carries the Go formatting artifact: %q", body)
	}
	if strings.Contains(body, "moved or removed") {
		t.Fatalf("the error body hints the worktree was moved/removed, but the path exists: %q", body)
	}
}
