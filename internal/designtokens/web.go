package designtokens

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// stampWebColors owns only the color attributes; the surrounding shell and
// manifest remain editable. Missing or duplicate slots are errors, not no-ops.
func stampWebColors(root, path string, slots map[string]string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return nil, err
	}
	for pattern, value := range slots {
		re := regexp.MustCompile(pattern)
		if len(re.FindAll(raw, -1)) != 1 {
			return nil, fmt.Errorf("%s: expected exactly one color slot matching %s", path, pattern)
		}
		raw = re.ReplaceAll(raw, []byte("${1}"+value+"${2}"))
	}
	return raw, nil
}

func webShell(root string, t Tokens) ([]byte, error) {
	slots := map[string]string{}
	for _, mode := range []string{"light", "dark"} {
		slots[`(<meta name="theme-color" content=")[^"]*(" media="\(prefers-color-scheme: `+mode+`\)" />)`] = t.Colors["surface"].value(mode)
	}
	return stampWebColors(root, "web/src/index.html", slots)
}

func webManifest(root string, t Tokens) ([]byte, error) {
	// A manifest has one theme_color: use LIGHT surface, matching the light meta
	// and prefers-color-scheme defaults. background_color uses light surface too.
	// Dark browser chrome comes from the dark meta; theme.ts rewrites it at runtime.
	return stampWebColors(root, "web/src/manifest.webmanifest", map[string]string{
		`("theme_color": ")[^"]*(")`:      t.Colors["surface"].Light,
		`("background_color": ")[^"]*(")`: t.Colors["surface"].Light,
	})
}
