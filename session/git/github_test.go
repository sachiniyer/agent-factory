package git

import (
	"os"
	"strings"
	"testing"
)

// TestFetchPRInfo_EmptyBranch verifies the (nil, nil) contract for a
// detached-HEAD worktree (#694): an empty branch name has no PR to look up,
// so FetchPRInfo must return (nil, nil) without shelling out to `gh`. This
// holds whether or not `gh` is installed because the empty-branch guard runs
// before the LookPath/exec path.
func TestFetchPRInfo_EmptyBranch(t *testing.T) {
	info, err := FetchPRInfo("/some/repo/path", "")
	if err != nil {
		t.Fatalf("empty branch should not be an error, got %v", err)
	}
	if info != nil {
		t.Errorf("empty branch should yield nil PRInfo, got %+v", info)
	}
}

// TestFetchPRInfo_NumericBranch_UsesHeadFlag guards the #740 fix: the lookup
// must run `gh pr list --head <branch>` (which always treats its argument as
// a branch name) rather than `gh pr view <branch>` (which parses an
// all-numeric argument as a PR NUMBER, so branch "123" resolved to PR #123).
// A stub `gh` on PATH records its argv so the test asserts the exact command
// shape without any network access.
func TestFetchPRInfo_NumericBranch_UsesHeadFlag(t *testing.T) {
	dir := t.TempDir()
	argvFile := dir + "/argv"
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\necho '[]'\n"
	if err := os.WriteFile(dir+"/gh", []byte(stub), 0o755); err != nil {
		t.Fatalf("failed to write gh stub: %v", err)
	}
	t.Setenv("PATH", dir)

	info, err := FetchPRInfo(dir, "123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info != nil {
		t.Errorf("empty pr list should yield nil PRInfo, got %+v", info)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("gh stub was not invoked: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argv)), "\n")
	joined := strings.Join(args, " ")
	if len(args) < 2 || args[0] != "pr" || args[1] != "list" {
		t.Errorf("expected `gh pr list ...`, got `gh %s`", joined)
	}
	if !strings.Contains(joined, "--head 123") {
		t.Errorf("expected `--head 123` in argv, got `gh %s`", joined)
	}
	if !strings.Contains(joined, "--state all") {
		t.Errorf("expected `--state all` so merged/closed PRs stay visible, got `gh %s`", joined)
	}
}

func TestParsePRList(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantInfo   *PRInfo
		wantErr    bool
		wantErrSub string
	}{
		{
			name:  "single open PR",
			input: `[{"number":42,"title":"Add feature","url":"https://example.com/pr/42","state":"OPEN"}]`,
			wantInfo: &PRInfo{
				Number: 42,
				Title:  "Add feature",
				URL:    "https://example.com/pr/42",
				State:  "OPEN",
			},
		},
		{
			name:     "empty array is treated as no PR",
			input:    `[]`,
			wantInfo: nil,
		},
		{
			name: "open PR preferred over newer merged PR",
			input: `[{"number":50,"title":"Reopened work","url":"https://example.com/pr/50","state":"MERGED"},` +
				`{"number":42,"title":"Add feature","url":"https://example.com/pr/42","state":"OPEN"}]`,
			wantInfo: &PRInfo{
				Number: 42,
				Title:  "Add feature",
				URL:    "https://example.com/pr/42",
				State:  "OPEN",
			},
		},
		{
			name: "no open PR falls back to first (newest) entry",
			input: `[{"number":50,"title":"Shipped","url":"https://example.com/pr/50","state":"MERGED"},` +
				`{"number":42,"title":"Abandoned","url":"https://example.com/pr/42","state":"CLOSED"}]`,
			wantInfo: &PRInfo{
				Number: 50,
				Title:  "Shipped",
				URL:    "https://example.com/pr/50",
				State:  "MERGED",
			},
		},
		{
			name:     "zero PR number is treated as no PR",
			input:    `[{"number":0,"title":"","url":"","state":""}]`,
			wantInfo: nil,
		},
		{
			name:       "malformed JSON returns error and preserves cache",
			input:      `not-json-at-all`,
			wantErr:    true,
			wantErrSub: "failed to parse PR info JSON",
		},
		{
			name:       "empty output returns error",
			input:      ``,
			wantErr:    true,
			wantErrSub: "failed to parse PR info JSON",
		},
		{
			name:       "truncated JSON returns error",
			input:      `[{"number":42,"title":"Add feat`,
			wantErr:    true,
			wantErrSub: "failed to parse PR info JSON",
		},
		{
			name:       "single object (gh pr view shape) returns error",
			input:      `{"number":42,"title":"Add feature","url":"https://example.com/pr/42","state":"OPEN"}`,
			wantErr:    true,
			wantErrSub: "failed to parse PR info JSON",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePRList([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (info=%v)", got)
				}
				if got != nil {
					t.Errorf("expected nil PRInfo on error, got %+v", got)
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Errorf("expected error to contain %q, got %q", tc.wantErrSub, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantInfo == nil {
				if got != nil {
					t.Errorf("expected nil PRInfo, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected PRInfo %+v, got nil", tc.wantInfo)
			}
			if *got != *tc.wantInfo {
				t.Errorf("expected %+v, got %+v", tc.wantInfo, got)
			}
		})
	}
}
