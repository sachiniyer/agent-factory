package commands

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/sachiniyer/agent-factory/config"
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

func loadResolvedConfig(projectSelector string) (*config.ResolvedConfig, error) {
	if projectSelector == "" {
		return config.ResolveGlobalConfig()
	}
	abs, err := config.ResolveUserPath(projectSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve --project path %q: %w", projectSelector, err)
	}
	repo, err := config.RepoFromPath(abs)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve --project path %q: %w", projectSelector, err)
	}
	return config.ResolveConfig(repo.Root)
}

func configEntriesFromResolution(resolved *config.ResolvedConfig) []configEntry {
	entries := make([]configEntry, 0, len(resolved.Resolution))
	for _, value := range resolved.Resolution {
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
