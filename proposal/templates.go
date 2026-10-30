package proposal

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

//go:embed templates/*.md
var builtinTemplates embed.FS

// BuiltinTemplates is the default doc set of a new proposal, in display order.
var BuiltinTemplates = []string{"goals", "technical", "api", "ui", "steps", "future"}

func userTemplateDir() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config dir: %w", err)
	}
	return filepath.Join(dir, "templates", "proposal"), nil
}

// loadTemplate returns the raw text of template name. A user file at
// <AF_HOME>/templates/proposal/<name>.md wins over the built-in one. found is
// false when neither exists.
func loadTemplate(name string) (text string, found bool, err error) {
	dir, err := userTemplateDir()
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, docFile(name)))
	if err == nil {
		return string(data), true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, fmt.Errorf("read template %q: %w", name, err)
	}
	data, err = builtinTemplates.ReadFile("templates/" + docFile(name))
	if err == nil {
		return string(data), true, nil
	}
	return "", false, nil
}

// renderDoc returns the initial text of doc name in proposal slug: the
// template with {{title}} filled in, or an empty doc when there is no template.
func renderDoc(slug, name string) (string, error) {
	text, found, err := loadTemplate(name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	return strings.ReplaceAll(text, "{{title}}", Title(slug)), nil
}

// Title is the human-readable form of a slug.
func Title(slug string) string {
	return strings.ReplaceAll(slug, "-", " ")
}
