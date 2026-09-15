package redactspan

import "testing"

func TestApplyCoversUnionOfPartiallyOverlappingSpans(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		spans []Span
		want  string
	}{
		{
			name:  "uncovered prefix",
			input: "secret /srv/reallylong/repo",
			spans: []Span{
				{Start: 0, End: len("secret /srv"), Replacement: "[title]"},
				{Start: len("secret "), End: len("secret /srv/reallylong/repo"), Replacement: "[repo]"},
			},
			want: "[redacted][repo]",
		},
		{
			name:  "uncovered suffix",
			input: "/srv/reallylong/repo secret",
			spans: []Span{
				{Start: 0, End: len("/srv/reallylong/repo"), Replacement: "[repo]"},
				{Start: len("/srv/reallylong/"), End: len("/srv/reallylong/repo secret"), Replacement: "[title]"},
			},
			want: "[repo][redacted]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Apply(tc.input, tc.spans, "[redacted]"); got != tc.want {
				t.Fatalf("Apply() = %q, want %q", got, tc.want)
			}
		})
	}
}
