package commands

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

type configExplainContext struct {
	Scope               string `json:"scope"`
	ProjectRoot         string `json:"project_root,omitempty"`
	View                string `json:"view"`
	RunningValueChecked bool   `json:"running_value_checked"`
}

type configGetExplanation struct {
	Context configExplainContext `json:"context"`
	config.ResolvedValue
}

type configListExplanation struct {
	Context configExplainContext   `json:"context"`
	Values  []config.ResolvedValue `json:"values"`
}

func configReadProjectSelector(repoSelector, projectAlias string) (selector string, explicit bool, err error) {
	if repoSelector != "" && projectAlias != "" {
		return "", false, fmt.Errorf("--repo and --project are aliases; pass only one")
	}
	if repoSelector != "" {
		return repoSelector, true, nil
	}
	if projectAlias != "" {
		return projectAlias, true, nil
	}
	repo, err := config.CurrentRepo()
	if err != nil {
		if errors.Is(err, config.ErrNotGitRepository) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve current repository: %w", err)
	}
	return repo.Root, false, nil
}

func loadResolvedConfig(projectSelector string) (*config.ResolvedConfig, error) {
	if projectSelector == "" {
		return config.ResolveGlobalConfig()
	}
	abs, err := config.ResolveUserPath(projectSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project path %q: %w", projectSelector, err)
	}
	repo, err := config.RepoFromPath(abs)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project path %q: %w", projectSelector, err)
	}
	resolved, err := config.ResolveConfigForInspection(repo.Root)
	if err != nil {
		return nil, err
	}
	displayRoot := selectedProjectDisplayRoot(abs, repo.Root)
	if err := resolved.RebaseProjectPathsForDisplay(displayRoot); err != nil {
		return nil, fmt.Errorf("failed to preserve project path %q for display: %w", projectSelector, err)
	}
	return resolved, nil
}

// selectedProjectDisplayRoot walks the lexical selector toward the filesystem
// root until it finds the ancestor with the same identity as the repository
// root returned by git. ResolveForCompare is deliberately used only for that
// comparison; the returned path keeps the spelling the user supplied. A
// linked worktree has no such ancestor because its configuration lives at the
// main worktree root, so the git-provided root is the honest fallback.
func selectedProjectDisplayRoot(selector, resolvedRoot string) string {
	want := pathutil.ResolveForCompare(resolvedRoot)
	for candidate := filepath.Clean(selector); ; candidate = filepath.Dir(candidate) {
		if pathutil.ResolveForCompare(candidate) == want {
			return candidate
		}
		if filepath.Dir(candidate) == candidate {
			return resolvedRoot
		}
	}
}

// isRootAgentExplainKey reports whether key names the root_agent table or one of
// its leaves. It matches "root_agent" and "root_agent.<leaf>" but deliberately
// NOT "root_agents" (the legacy map is a distinct key).
func isRootAgentExplainKey(key string) bool {
	return key == "root_agent" || strings.HasPrefix(key, "root_agent.")
}

// rootAgentReadValue returns the specialized four-layer root_agent resolution:
// the whole table, or a projected leaf for a dotted key. It mirrors what the
// daemon resolves (built-in/global/legacy/personal), unlike the generic
// global<personal resolver. A dotted leaf is projected through the same
// ResolvedValuePath machinery every other key uses, by wrapping the specialized
// table in a throwaway ResolvedConfig — so concise and --explain reads cannot
// disagree about the effective value.
func rootAgentReadValue(projectSelector, keyPath string, strictProjectLookup bool) (config.ResolvedValue, error) {
	parent, err := config.ResolveRootAgentForInspection(projectSelector, strictProjectLookup)
	if err != nil {
		return config.ResolvedValue{}, err
	}
	if keyPath == "root_agent" {
		return parent, nil
	}
	synthetic := &config.ResolvedConfig{Resolution: []config.ResolvedValue{parent}}
	projected, ok := synthetic.ResolvedValuePath(keyPath)
	if !ok {
		return config.ResolvedValue{}, unknownConfigKeyError(keyPath)
	}
	return projected, nil
}

func rootAgentAwareResolution(resolved *config.ResolvedConfig, projectSelector string, strictProjectLookup bool) ([]config.ResolvedValue, error) {
	values := append([]config.ResolvedValue(nil), resolved.Resolution...)
	rootAgent, err := config.ResolveRootAgentForInspection(projectSelector, strictProjectLookup)
	if err != nil {
		return nil, err
	}
	for i := range values {
		if values[i].Key == "root_agent" {
			values[i] = rootAgent
			break
		}
	}
	return values, nil
}

func configEntriesFromResolution(values []config.ResolvedValue) []configEntry {
	entries := make([]configEntry, 0, len(values))
	for _, value := range values {
		entries = append(entries, configEntry{Key: value.Key, Value: value.Value})
	}
	return entries
}

func configExplanationContext(resolved *config.ResolvedConfig) configExplainContext {
	context := configExplainContext{
		Scope:               "global",
		View:                "on-disk",
		RunningValueChecked: false,
	}
	if resolved.ProjectRoot != "" {
		context.Scope = "project"
		context.ProjectRoot = resolved.ProjectRoot
	}
	return context
}

func writeConfigExplanations(w io.Writer, resolved *config.ResolvedConfig, values []config.ResolvedValue) error {
	context := configExplanationContext(resolved)
	if context.ProjectRoot == "" {
		fmt.Fprintln(w, "scope: global defaults")
	} else {
		fmt.Fprintf(w, "project: %s\n", context.ProjectRoot)
	}
	fmt.Fprintln(w, "runtime: on-disk config · running daemon value not checked")

	for i, value := range values {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if err := writeConfigValueExplanation(w, value); err != nil {
			return err
		}
	}
	return nil
}

func writeConfigValueExplanation(w io.Writer, value config.ResolvedValue) error {
	fmt.Fprintf(w, "%s = %s\n", value.Key, formatConfigExplanationValue(value.Value))
	if value.Default != "" {
		fmt.Fprintf(w, "default: %s\n", value.Default)
	}
	fmt.Fprintf(w, "policy: %s · %s\n\n", value.Merge, strings.Join(value.Precedence, " < "))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tVALUE\tLOCATION\tRESULT")
	for _, candidate := range value.Candidates {
		candidateValue := "—"
		if candidate.Present {
			candidateValue = formatConfigExplanationValue(candidate.Value)
		}
		location := "compiled default"
		if candidate.Path != "" {
			location = prettyPath(candidate.Path) + ":" + candidate.KeyPath
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s · %s\n",
			candidate.Layer, candidateValue, location, candidate.Result, candidate.Reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(value.Origins) > 0 {
		fmt.Fprintln(w, "origins:")
		leaves := make([]string, 0, len(value.Origins))
		for leaf := range value.Origins {
			leaves = append(leaves, leaf)
		}
		sort.Strings(leaves)
		for _, leaf := range leaves {
			origin := value.Origins[leaf]
			location := "compiled default"
			if origin.Path != "" {
				location = prettyPath(origin.Path) + ":" + origin.KeyPath
			}
			fmt.Fprintf(w, "  %s: %s · %s\n", leaf, origin.Layer, location)
		}
	}
	return nil
}

func formatConfigExplanationValue(value any) string {
	if text, ok := value.(string); ok && text == "" {
		return `""`
	}
	return formatConfigValue(value)
}
