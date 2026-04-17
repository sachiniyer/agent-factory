package task

import "testing"

func TestIsReadyContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "empty",
			content: "",
			want:    false,
		},
		{
			name:    "claude input prompt",
			content: "some output\n\n❯ ",
			want:    true,
		},
		{
			name:    "claude trust prompt",
			content: "Do you trust the files in this folder?\n1. Yes\n2. No",
			want:    true,
		},
		{
			name: "aider trust prompt",
			content: "Aider v0.1\nOpen documentation url for more info: https://aider.chat/docs/\n" +
				"(Y)es/(N)o/(D)on't ask again [Yes]:",
			want: true,
		},
		{
			name: "gemini trust prompt",
			content: "Gemini CLI\nOpen documentation url for more info.\n" +
				"(D)on't ask again",
			want: true,
		},
		{
			name:    "only open documentation url without confirm",
			content: "See Open documentation url for details about this command.",
			want:    false,
		},
		{
			name:    "only dont ask again without doc url",
			content: "Some prompt asking (D)on't ask again without the documentation prefix",
			want:    false,
		},
		{
			name:    "unrelated output",
			content: "installing dependencies...\nready soon",
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadyContent(tc.content); got != tc.want {
				t.Errorf("isReadyContent(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}
