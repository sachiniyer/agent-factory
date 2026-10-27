package proposal

import (
	"strings"
	"testing"
)

func TestRelativeRepoPathIsRefused(t *testing.T) {
	svc, _, _ := newTestService(t)
	for _, p := range []string{".", "sub/dir"} {
		_, err := svc.resolve(Target{RepoPath: p})
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("resolve(repo_path %q) = %v; want a must-be-absolute error", p, err)
		}
	}
}
