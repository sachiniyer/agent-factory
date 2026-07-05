package bugreport

import (
	"encoding/json"
	"fmt"
	"strings"
)

// renderText renders the human-readable, single-file bundle from an
// already-redacted Bundle. The output is plain text with clearly delimited
// sections so it is pasteable into a GitHub issue and reviewable in one scroll
// before sharing. The caller runs a final scrub pass over the returned string.
func renderText(b Bundle) string {
	var sb strings.Builder

	section := func(title string) {
		fmt.Fprintf(&sb, "\n===== %s =====\n", title)
	}

	fmt.Fprintln(&sb, "AGENT FACTORY BUG REPORT")
	fmt.Fprintf(&sb, "generated: %s\n", b.GeneratedAt)
	fmt.Fprintf(&sb, "\n!! %s\n", b.Warning)

	section("VERSIONS")
	fmt.Fprintf(&sb, "af:      %s\n", b.Versions.AF)
	fmt.Fprintf(&sb, "go:      %s\n", b.Versions.Go)
	fmt.Fprintf(&sb, "os/arch: %s/%s\n", b.Versions.OS, b.Versions.Arch)

	section("DAEMON STATUS")
	if strings.TrimSpace(b.DaemonHumanText()) != "" {
		sb.WriteString(b.DaemonHumanText())
		if !strings.HasSuffix(b.DaemonHumanText(), "\n") {
			sb.WriteByte('\n')
		}
	} else {
		writeIndentedJSON(&sb, b.Daemon)
	}

	section("TASKS (redacted)")
	if len(b.Tasks) == 0 {
		fmt.Fprintln(&sb, "(none configured)")
	} else {
		writeIndentedJSON(&sb, b.Tasks)
	}

	section("INSTANCES (redacted)")
	if len(b.Instances) == 0 {
		fmt.Fprintln(&sb, "(no sessions)")
	} else {
		for _, ri := range b.Instances {
			fmt.Fprintf(&sb, "--- repo %s ---\n", ri.RepoID)
			sb.Write(indentRaw(ri.Instances))
			sb.WriteByte('\n')
		}
	}

	section("CONFIG (redacted)")
	if b.Config == nil {
		fmt.Fprintln(&sb, "(no config file; running on defaults)")
	} else {
		fmt.Fprintf(&sb, "# %s (%s)\n", b.Config.Path, b.Config.Format)
		sb.WriteString(b.Config.Contents)
		if !strings.HasSuffix(b.Config.Contents, "\n") {
			sb.WriteByte('\n')
		}
	}

	section("DAEMON LOG TAIL (redacted)")
	fmt.Fprintf(&sb, "# %s", b.Log.Path)
	if b.Log.Truncated {
		fmt.Fprint(&sb, " (truncated to the last ~2MiB / 5000 lines)")
	}
	sb.WriteByte('\n')
	sb.WriteString(b.Log.Contents)
	if b.Log.Contents != "" && !strings.HasSuffix(b.Log.Contents, "\n") {
		sb.WriteByte('\n')
	}

	if len(b.Errors) > 0 {
		section("COLLECTION ERRORS")
		for _, e := range b.Errors {
			fmt.Fprintf(&sb, "- %s\n", e)
		}
	}

	return sb.String()
}

// DaemonHumanText exposes the pre-rendered human daemon status carried
// alongside the structured snapshot. It is threaded through Build via a
// package-private field set on the Bundle by the command layer; when unset the
// renderer falls back to the structured JSON.
func (b Bundle) DaemonHumanText() string { return b.daemonHuman }

// writeIndentedJSON marshals v with two-space indent into sb, falling back to a
// Go-syntax dump if marshaling fails (it should not for our own types).
func writeIndentedJSON(sb *strings.Builder, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(sb, "%+v\n", v)
		return
	}
	sb.Write(data)
	sb.WriteByte('\n')
}

// indentRaw re-indents an already-valid JSON payload for readable embedding.
func indentRaw(raw json.RawMessage) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return out
}
