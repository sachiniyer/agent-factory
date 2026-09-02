package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// exitErrorWithStderr produces a real *exec.ExitError carrying stderr, the way
// cmd.Output() does. Constructing it by running a genuine failing process is
// deliberate: a hand-built ExitError with a nil ProcessState would not exercise
// the same value the production path unwraps.
func exitErrorWithStderr(t *testing.T, stderr string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", "printf '%s' \"$0\" >&2; exit 1", stderr)
	_, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an *exec.ExitError from a failing command, got %T (%v)", err, err)
	}
	return err
}

// TestFetchPRInfo_SurfacesGhStderr is the #3392 regression: a non-zero `gh`
// exit must report what gh actually said, not just "exit status 1". Before the
// fix the message was exactly "failed to fetch PR info: exit status 1", from
// which a maintainer could not distinguish an auth failure from a rate limit,
// an unreachable network, or a repo with no GitHub remote.
func TestFetchPRInfo_SurfacesGhStderr(t *testing.T) {
	dir := t.TempDir()
	const ghMessage = "gh: Bad credentials (HTTP 401)"
	stub := "#!/bin/sh\nprintf '%s\\n' '" + ghMessage + "' >&2\nexit 1\n"
	if err := os.WriteFile(dir+"/gh", []byte(stub), 0o755); err != nil {
		t.Fatalf("failed to write gh stub: %v", err)
	}
	t.Setenv("PATH", dir)

	_, err := FetchPRInfo(t.TempDir(), "some-branch")
	if err == nil {
		t.Fatal("a non-zero gh exit must be an error")
	}
	if !strings.Contains(err.Error(), ghMessage) {
		t.Errorf("the error must carry gh's own explanation.\n got: %q\nwant it to contain: %q", err.Error(), ghMessage)
	}
	// The wrap must stay a wrap: callers and errors.As still need the exit code.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("the *exec.ExitError must remain unwrappable, got %T", err)
	}
}

// TestCommandFailureDetail_NonExitErrorsAddNothing pins the negative half. A
// start failure or a cancellation carries no captured stderr, and a
// CombinedOutput failure puts stderr in the output slice rather than in
// ExitError.Stderr — in all of those the caller's plain %w wrap is already the
// whole truth, and inventing a detail would be worse than adding none.
func TestCommandFailureDetail_NonExitErrorsAddNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain error", errors.New("boom")},
		{"wrapped plain error", fmt.Errorf("context: %w", errors.New("boom"))},
		{"exit error with empty stderr", exitErrorWithStderr(t, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandFailureDetail(tc.err); got != "" {
				t.Errorf("expected no detail, got %q", got)
			}
		})
	}
}

// TestCommandFailureDetail_ReadsThroughAWrap guards the errors.As contract: a
// call site that has already wrapped the ExitError once must still get the
// detail, or the helper would silently regress to "exit status 1" the first
// time someone adds context above it.
func TestCommandFailureDetail_ReadsThroughAWrap(t *testing.T) {
	wrapped := fmt.Errorf("outer context: %w", exitErrorWithStderr(t, "inner explanation"))
	if got := commandFailureDetail(wrapped); got != "inner explanation" {
		t.Errorf("detail through a wrap = %q, want %q", got, "inner explanation")
	}
}

// TestCollapseDetail_FlattensToOneLine pins the log-line contract. These errors
// are printed as single daemon log lines, so an embedded newline would split
// one warning into several — the continuations arriving with no timestamp, no
// level, and no link back to the failure they explain.
func TestCollapseDetail_FlattensToOneLine(t *testing.T) {
	got := collapseDetail("error: could not read Username\n\nhint: run gh auth login\r\n")
	want := "error: could not read Username; hint: run gh auth login"
	if got != want {
		t.Errorf("collapseDetail = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("collapsed detail must contain no newlines, got %q", got)
	}
}

// TestTruncateDetail_BoundsAndKeepsRunesIntact covers both halves of the bound:
// a pathological stderr must not flood the log, and the cut must land on a rune
// boundary so a multi-byte character is never sliced into replacement bytes.
func TestTruncateDetail_BoundsAndKeepsRunesIntact(t *testing.T) {
	if got := truncateDetail("short"); got != "short" {
		t.Errorf("under the limit must pass through unchanged, got %q", got)
	}

	flood := strings.Repeat("x", maxStderrDetail*3)
	got := truncateDetail(flood)
	if len(got) > maxStderrDetail+len("… (truncated) …") {
		t.Errorf("truncated length %d exceeds the bound", len(got))
	}
	if !strings.Contains(got, "… (truncated) …") {
		t.Errorf("a truncated detail must say so, got %q", got)
	}

	// A 3-byte rune straddling the limit must be dropped whole, not split.
	straddling := strings.Repeat("a", maxStderrDetail-1) + "☃" + strings.Repeat("b", 100)
	if cut := truncateDetail(straddling); strings.Contains(cut, "�") {
		t.Errorf("truncation split a multi-byte rune: %q", cut[max(0, len(cut)-20):])
	}
}

// TestTruncateDetail_KeepsTheTail is the review regression (#3392 review): a
// head-only bound discards exactly the line worth reading. git prints whatever
// the remote hooks emitted and only THEN its rejection reason, so truncating
// from the front keeps the noise and drops the diagnosis — and for
// runGitCommandContextWithEnvironment, which surfaced full stderr before this
// helper existed, that would have been a straight regression.
func TestTruncateDetail_KeepsTheTail(t *testing.T) {
	const diagnosis = "error: failed to push some refs — remote rejected (pre-receive hook declined)"
	noise := strings.Repeat("remote: building... ", 400)
	got := truncateDetail(noise + diagnosis)

	if !strings.HasSuffix(got, diagnosis) {
		t.Errorf("the final diagnosis must survive truncation.\n got tail: %q\nwant suffix: %q",
			got[max(0, len(got)-120):], diagnosis)
	}
	if !strings.HasPrefix(got, "remote: building") {
		t.Errorf("the head must survive too, got prefix %q", got[:min(40, len(got))])
	}
	if len(got) > maxStderrDetail+len("… (truncated) …") {
		t.Errorf("truncated length %d exceeds the bound", len(got))
	}
}
