package designtokens

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const contrastStart = "<!-- generated contrast: start -->"
const contrastEnd = "<!-- generated contrast: end -->"

// contrastReport uses the same WCAG relative luminance calculation as validation.
// Generate owns this section so --check detects documentation drift too.
func contrastReport(t Tokens) string {
	var b strings.Builder
	b.WriteString(contrastStart + "\n\n")
	b.WriteString("| Role | Light | Dark | Light on surface | Dark on surface | Light on raised | Dark on raised |\n")
	b.WriteString("| --- | --- | --- | ---: | ---: | ---: | ---: |\n")
	for _, role := range []string{"surface", "surface-raised", "ink", "ink-muted", "border", "accent", "running", "ready", "lost", "dead", "archived", "limit-reached"} {
		c := t.Colors[role]
		fmt.Fprintf(&b, "| %s | `%s` | `%s` | %.3f:1 | %.3f:1 | %.3f:1 | %.3f:1 |\n", role, c.Light, c.Dark,
			contrast(c.Light, t.Colors["surface"].Light), contrast(c.Dark, t.Colors["surface"].Dark),
			contrast(c.Light, t.Colors["surface-raised"].Light), contrast(c.Dark, t.Colors["surface-raised"].Dark))
	}
	fmt.Fprintf(&b, "\nSurface text on accent: light **%.3f:1**, dark **%.3f:1**.\nInk/muted hierarchy: light **%.3f:1**, dark **%.3f:1**.\n\n", contrast(t.Colors["surface"].Light, t.Colors["accent"].Light), contrast(t.Colors["surface"].Dark, t.Colors["accent"].Dark), contrast(t.Colors["ink"].Light, t.Colors["ink-muted"].Light), contrast(t.Colors["ink"].Dark, t.Colors["ink-muted"].Dark))
	b.WriteString(contrastEnd)
	return b.String()
}

func interfacePage(root string, t Tokens) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(root, "docs/design/interface-design.md"))
	if err != nil {
		return nil, err
	}
	s := string(raw)
	start, end := strings.Index(s, contrastStart), strings.Index(s, contrastEnd)
	if start < 0 || end < start {
		return nil, fmt.Errorf("interface design is missing contrast section markers")
	}
	return []byte(s[:start] + contrastReport(t) + s[end+len(contrastEnd):]), nil
}
