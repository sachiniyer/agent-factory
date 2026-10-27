package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This is the lint half of #4125. The late-ghost finalizer is detached:
// KillSession and reconcileLateGhostCleanup return while it is still retrying,
// and it reads package-level seams on every attempt. A test that swaps one of
// those seams and restores it in a bare t.Cleanup races the worker whenever the
// test ends first — which is exactly what a failed wait does — and -race then
// fails the NEXT test in the package, pointing its author at the wrong code.
//
// #4063 fixed that at one of the four sites. This keeps the other shape from
// coming back: the seams below may be swapped only through stubLateGhostSeam in
// daemon/repo_gone_ghost_cleanup_test.go, which stops and joins the manager's
// background mutations before restoring. Its restore is written through a
// pointer, so a direct assignment to one of these names in a daemon test is
// always the shape this lint exists to refuse.
//
// Like the log-capture lint beside it, this is a drift lint, not a sandbox.

// lateGhostFinalizerSeams are the package-level variables the finalizer reads
// on its retry path (daemon/manager_persist.go).
var lateGhostFinalizerSeams = regexp.MustCompile(`\blateGhost(?:DeleteSessionRecord|CleanupRetryInterval)\s*=[^=]`)

func lintDaemonFinalizerSeams(files map[string]string) []string {
	var findings []string
	for name, content := range files {
		for i, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if lateGhostFinalizerSeams.MatchString(line) {
				findings = append(findings, name+":"+strconv.Itoa(i+1)+": "+
					"assigns a seam the detached late-ghost finalizer reads. Restoring it in a"+
					" bare t.Cleanup races that worker whenever the test ends first, and -race"+
					" then fails the next test instead of this one (#4125). Use"+
					" stubLateGhostSeam(t, manager, &seam, stub).")
			}
		}
	}
	return findings
}

func TestDaemonFinalizerSeamLintFlagsBareSwaps(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "the swap that raced in #4125",
			content: "\tpreviousRetryInterval := lateGhostCleanupRetryInterval\n\tlateGhostCleanupRetryInterval = 5 * time.Millisecond\n",
			want:    true,
		},
		{
			name:    "the restore half on its own",
			content: "\tt.Cleanup(func() { lateGhostDeleteSessionRecord = previousDelete })\n",
			want:    true,
		},
		{
			name:    "a swap of the delete seam to a closure",
			content: "\tlateGhostDeleteSessionRecord = func(*Manager, string, string, string, error) (bool, error) {\n",
			want:    true,
		},
		{
			name:    "reading a seam to save it is not a swap",
			content: "\tprevious := lateGhostDeleteSessionRecord\n\tif lateGhostCleanupRetryInterval == 0 {\n",
			want:    false,
		},
		{
			name:    "the helper",
			content: "\tstubLateGhostSeam(t, manager, &lateGhostCleanupRetryInterval, 5*time.Millisecond)\n",
			want:    false,
		},
		{
			name:    "prose about the old shape is inert",
			content: "\t// Was: lateGhostCleanupRetryInterval = 5 * time.Millisecond (#4125).\n",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := lintDaemonFinalizerSeams(map[string]string{"daemon/x_test.go": tc.content})
			if got := len(findings) > 0; got != tc.want {
				t.Fatalf("flagged = %v, want %v (findings: %v)", got, tc.want, findings)
			}
		})
	}
}

func TestDaemonFinalizerSeamLintOverTheRealTree(t *testing.T) {
	files := readDaemonTestFiles(t)
	const ghostTests = "daemon/repo_gone_ghost_cleanup_test.go"
	if files[ghostTests] == "" {
		t.Fatalf("scan did not read %s — the lint is not looking where it thinks it is", ghostTests)
	}

	if findings := lintDaemonFinalizerSeams(files); len(findings) > 0 {
		t.Fatalf("daemon tests swap late-ghost finalizer seams without joining the finalizer:\n  %s",
			strings.Join(findings, "\n  "))
	}

	// Satisfied vacuously if every swap were deleted, so count the helper's uses:
	// the four tests that stub finalizer seams today make seven calls between them.
	if uses := strings.Count(files[ghostTests], "stubLateGhostSeam(t, "); uses < 7 {
		t.Errorf("found %d stubLateGhostSeam calls in %s, want at least 7. If a test legitimately "+
			"stopped stubbing a finalizer seam, lower this number in the same commit and say why",
			uses, ghostTests)
	}
}
