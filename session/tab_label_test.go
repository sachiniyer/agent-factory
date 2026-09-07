package session

import "testing"

func TestTabLabel_DefaultKindNamesUseDisplayLabels(t *testing.T) {
	tests := []struct {
		name string
		tab  *Tab
		want string
	}{
		{name: "vscode empty name", tab: &Tab{Kind: TabKindVSCode}, want: "VS Code"},
		{name: "vscode legacy default name", tab: &Tab{Name: "vscode", Kind: TabKindVSCode}, want: "VS Code"},
		{name: "vscode custom name", tab: &Tab{Name: "My editor", Kind: TabKindVSCode}, want: "My editor"},
		{name: "web empty name", tab: &Tab{Kind: TabKindWeb}, want: "Web"},
		{name: "web legacy default name", tab: &Tab{Name: "web", Kind: TabKindWeb}, want: "Web"},
		{name: "web custom name", tab: &Tab{Name: "My browser", Kind: TabKindWeb}, want: "My browser"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TabLabel(tt.tab); got != tt.want {
				t.Fatalf("TabLabel(%+v) = %q, want %q", tt.tab, got, tt.want)
			}
		})
	}
}
