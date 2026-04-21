package git

import (
	"strings"
	"testing"
)

func TestParsePRInfo(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantInfo   *PRInfo
		wantErr    bool
		wantErrSub string
	}{
		{
			name:  "valid PR JSON",
			input: `{"number":42,"title":"Add feature","url":"https://example.com/pr/42","state":"OPEN"}`,
			wantInfo: &PRInfo{
				Number: 42,
				Title:  "Add feature",
				URL:    "https://example.com/pr/42",
				State:  "OPEN",
			},
		},
		{
			name:     "zero PR number is treated as no PR",
			input:    `{"number":0,"title":"","url":"","state":""}`,
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
			input:      `{"number":42,"title":"Add feat`,
			wantErr:    true,
			wantErrSub: "failed to parse PR info JSON",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePRInfo([]byte(tc.input))
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
