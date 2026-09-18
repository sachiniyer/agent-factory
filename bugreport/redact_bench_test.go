package bugreport

import "testing"

func BenchmarkScrubLogLine(b *testing.B) {
	r := &redactor{}
	line := "ERROR:2026/08/05 11:15:52 worktree_ops.go:529: failed to remove worktree /home/u/.agent-factory/worktrees/af_0f8fc14c_fix-login: exit status 128"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.scrubLog(line)
	}
}

func BenchmarkScrubLogLineEncoded(b *testing.B) {
	r := &redactor{}
	line := `ERROR:2026/08/05 11:15:52 hooks.go:88: post-worktree hook "fetch https://user@example.com/path?q=1" exited 1`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.scrubLog(line)
	}
}
