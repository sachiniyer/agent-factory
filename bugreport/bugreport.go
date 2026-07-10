// Package bugreport builds `af bug-report`: a single, self-contained,
// best-effort-redacted diagnostics bundle a user can attach to a GitHub issue.
// It gathers the daemon log tail, versions, the configured tasks, the session
// state (instances.json), the daemon health snapshot, and the global config,
// then runs every free-text and secret-bearing field through the redactor in
// redact.go. Redaction is best-effort by design — the bundle always carries a
// "review before sharing" warning — but the policy is conservative: structural
// triage fields are kept, free-text bodies are dropped, and $HOME/username and
// known credential shapes are scrubbed everywhere.
package bugreport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

const (
	// logTailMaxBytes bounds how much of the (possibly large) log we read —
	// the tail is what matters for triage and the bundle must stay pasteable.
	logTailMaxBytes = 2 << 20 // 2 MiB
	// logTailMaxLines further caps the tail after the byte read so a log of
	// very long lines still yields a bounded, readable slice.
	logTailMaxLines = 5000

	// reviewWarning is repeated at the top of the text bundle and carried in
	// the JSON so no consumer can miss it: redaction is best-effort.
	reviewWarning = "REVIEW THIS BUNDLE BEFORE SHARING. Redaction is best-effort — " +
		"paths, prompts, and secrets are scrubbed on a conservative default, but " +
		"perfect redaction is impossible. Open it, read it, and remove anything " +
		"sensitive before attaching it to a public issue."
)

// Inputs are the already-collected, caller-supplied facts the bugreport
// package cannot gather without importing package main. The command layer
// resolves the af version and the daemon status (it owns collectDaemonStatus)
// and passes them in; everything else this package reads itself.
type Inputs struct {
	// AFVersion is the `af` binary version (main.version).
	AFVersion string
	// GeneratedAt is a caller-formatted timestamp for the bundle header.
	GeneratedAt string
	// DaemonStatus is the structured `af daemon status` snapshot, embedded
	// verbatim in the JSON manifest (its socket/pid paths are scrubbed by the
	// final text pass).
	DaemonStatus any
	// DaemonHuman is the pre-rendered human `af daemon status` text for the
	// text bundle's daemon section.
	DaemonHuman string
}

// Versions is the version/platform block of the bundle.
type Versions struct {
	AF   string `json:"af"`
	Go   string `json:"go"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// repoInstances is one repo's redacted session records.
type repoInstances struct {
	RepoID    string          `json:"repo_id"`
	Instances json.RawMessage `json:"instances"`
}

// configSection is the redacted global config file, or nil when none exists.
type configSection struct {
	Path     string `json:"path"`
	Format   string `json:"format"`
	Contents string `json:"contents"`
}

// logSection is the redacted, bounded daemon log tail.
type logSection struct {
	Path      string `json:"path"`
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
	Contents  string `json:"contents"`
}

// Bundle is the structured manifest emitted by `--json` and the source the
// text bundle is rendered from.
type Bundle struct {
	GeneratedAt string          `json:"generated_at"`
	Warning     string          `json:"warning"`
	Versions    Versions        `json:"versions"`
	Daemon      any             `json:"daemon"`
	Tasks       []redactedTask  `json:"tasks"`
	Instances   []repoInstances `json:"instances"`
	Config      *configSection  `json:"config,omitempty"`
	Log         logSection      `json:"log"`
	// Errors records non-fatal collection failures (an unreadable section)
	// so a partial bundle is still useful and the gaps are explicit.
	Errors []string `json:"errors,omitempty"`

	// daemonHuman is the pre-rendered human daemon-status text used by the
	// text renderer. Unexported so it never appears in the JSON manifest (the
	// structured Daemon snapshot is the machine-readable form there).
	daemonHuman string
}

// Result is what Build returns: a rendered text bundle (the file the user
// attaches), the JSON manifest payload (the `--json` data member), and the
// pre-filled GitHub issue-draft title/body the default flow opens in the
// browser. Title/Body are short, already-redacted (built only from redacted
// Bundle facts + a static template), and safe to inline in an issues/new URL —
// the full bundle never rides in the URL; it reaches the issue as the attached
// file. Body carries BundlePathPlaceholder for the command layer to replace with
// the written bundle's path.
type Result struct {
	Text  string
	JSON  []byte
	Title string
	Body  string
}

// BundlePathPlaceholder marks where in the issue-draft body the command layer
// substitutes the written bundle's path (known only once the file is written).
const BundlePathPlaceholder = "{{BUNDLE_PATH}}"

// Build collects, redacts, and renders the bundle. It never fails on a missing
// or unreadable section — those are recorded in Bundle.Errors and rendering
// continues — so a user on a broken install can still produce a report.
func Build(in Inputs) (Result, error) {
	r := newRedactor()
	b := Bundle{
		GeneratedAt: in.GeneratedAt,
		Warning:     reviewWarning,
		Versions: Versions{
			AF:   in.AFVersion,
			Go:   runtime.Version(),
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
		},
		Daemon:      in.DaemonStatus,
		daemonHuman: in.DaemonHuman,
	}

	b.Tasks, b.Errors = collectTasks(b.Errors)
	b.Instances, b.Errors = collectInstances(r, b.Errors)
	b.Config, b.Errors = collectConfig(r, b.Errors)
	b.Log, b.Errors = collectLog(r, b.Errors)

	jsonBytes, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("marshal bug-report bundle: %w", err)
	}
	// Final catch-all pass: scrubs $HOME/username in the passed-in daemon
	// socket/pid paths and any residue. Idempotent over the per-section
	// scrubbing already applied, and safe for JSON (replacements never
	// introduce characters that need escaping).
	jsonBytes = []byte(r.scrub(string(jsonBytes)))

	text := r.scrub(renderText(b))
	title, body := buildIssueDraft(b)

	return Result{Text: text, JSON: jsonBytes, Title: title, Body: body}, nil
}

// buildIssueDraft renders the pre-filled GitHub issue-draft title and body from
// the already-redacted Bundle. It is deliberately short (the full 2MiB bundle
// cannot inline in an issues/new URL — it reaches the issue as the attached
// file) and carries only redacted, structural facts plus a bug-report template
// stub. BundlePathPlaceholder marks the attach-path the command layer fills in.
func buildIssueDraft(b Bundle) (title, body string) {
	title = fmt.Sprintf("af bug-report: %s on %s/%s", b.Versions.AF, b.Versions.OS, b.Versions.Arch)

	daemon := "running"
	if strings.Contains(strings.ToLower(b.daemonHuman), "not running") {
		daemon = "not running"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "<!-- Opened by `af bug-report`. This is a DRAFT — review it, attach the bundle below, then submit. -->\n\n")
	fmt.Fprintf(&sb, "## Environment\n")
	fmt.Fprintf(&sb, "- af: %s\n", b.Versions.AF)
	fmt.Fprintf(&sb, "- go: %s\n", b.Versions.Go)
	fmt.Fprintf(&sb, "- os/arch: %s/%s\n", b.Versions.OS, b.Versions.Arch)
	fmt.Fprintf(&sb, "- daemon: %s\n", daemon)
	fmt.Fprintf(&sb, "- sessions: %d across %d repo(s)\n", countInstances(b.Instances), len(b.Instances))
	fmt.Fprintf(&sb, "- tasks: %d\n\n", len(b.Tasks))
	fmt.Fprintf(&sb, "## What happened\n<!-- Describe the bug. -->\n\n")
	fmt.Fprintf(&sb, "## What you expected\n\n")
	fmt.Fprintf(&sb, "## Steps to reproduce\n\n")
	fmt.Fprintf(&sb, "---\n")
	fmt.Fprintf(&sb, "A best-effort **redacted** diagnostics bundle was written to:\n\n")
	fmt.Fprintf(&sb, "    %s\n\n", BundlePathPlaceholder)
	fmt.Fprintf(&sb, "**Attach that file to this issue** (drag-and-drop) before submitting. "+
		"Open and review it first — redaction is best-effort and cannot catch everything.\n")
	return title, sb.String()
}

// countInstances totals the session records across every repo's redacted
// instances payload, for the issue-draft environment summary. A payload that
// does not decode as an array contributes zero — the count is a best-effort
// triage hint, never load-bearing.
func countInstances(repos []repoInstances) int {
	total := 0
	for _, ri := range repos {
		var arr []json.RawMessage
		if err := json.Unmarshal(ri.Instances, &arr); err == nil {
			total += len(arr)
		}
	}
	return total
}

// collectTasks loads and redacts the configured tasks.
func collectTasks(errs []string) ([]redactedTask, []string) {
	tasks, err := task.LoadTasks()
	if err != nil {
		return nil, append(errs, fmt.Sprintf("tasks: %v", err))
	}
	out := make([]redactedTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, redactTask(t))
	}
	return out, errs
}

// collectInstances loads every repo's instances.json and redacts each, sorted
// by repo id for stable output.
func collectInstances(r *redactor, errs []string) ([]repoInstances, []string) {
	all, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, append(errs, fmt.Sprintf("instances: %v", err))
	}
	repoIDs := make([]string, 0, len(all))
	for id := range all {
		repoIDs = append(repoIDs, id)
	}
	sort.Strings(repoIDs)
	out := make([]repoInstances, 0, len(repoIDs))
	for _, id := range repoIDs {
		out = append(out, repoInstances{
			RepoID:    id,
			Instances: r.redactInstancesJSON(all[id]),
		})
	}
	return out, errs
}

// collectConfig reads the global config file (config.toml preferred, else
// config.json) and scrubs it. A missing file is not an error — many installs
// run on defaults — it just yields a nil section.
func collectConfig(r *redactor, errs []string) (*configSection, []string) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return nil, append(errs, fmt.Sprintf("config dir: %v", err))
	}
	for _, c := range []struct{ name, format string }{
		{config.TomlConfigFileName, "toml"},
		{config.ConfigFileName, "json"},
	} {
		path := filepath.Join(dir, c.name)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			errs = append(errs, fmt.Sprintf("config %s: %v", c.name, readErr))
			continue
		}
		return &configSection{
			Path:     path,
			Format:   c.format,
			Contents: r.scrub(string(data)),
		}, errs
	}
	return nil, errs
}

// collectLog reads and scrubs the bounded tail of the production log.
func collectLog(r *redactor, errs []string) (logSection, []string) {
	path := aflog.LogFilePath()
	sec := logSection{Path: path}
	if path == "" {
		return sec, append(errs, "log: could not resolve log path")
	}
	data, truncated, err := tailFile(path, logTailMaxBytes)
	if err != nil {
		if os.IsNotExist(err) {
			sec.Contents = "(no log file at " + path + ")"
			return sec, errs
		}
		return sec, append(errs, fmt.Sprintf("log %s: %v", path, err))
	}
	text, lineTruncated := lastLines(string(data), logTailMaxLines)
	sec.Truncated = truncated || lineTruncated
	sec.Contents = r.scrubLog(text)
	sec.Bytes = len(sec.Contents)
	return sec, errs
}

// tailFile reads at most maxBytes from the end of path. When the file is
// larger, the read starts at a byte offset (so the first, likely-partial line
// is dropped by lastLines) and truncated is true.
func tailFile(path string, maxBytes int64) (data []byte, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	size := info.Size()
	if size <= maxBytes {
		buf, readErr := os.ReadFile(path)
		return buf, false, readErr
	}
	if _, err := f.Seek(size-maxBytes, 0); err != nil {
		return nil, false, err
	}
	buf := make([]byte, maxBytes)
	n, err := f.Read(buf)
	if err != nil {
		return nil, false, err
	}
	// Drop the partial first line left by the mid-file seek.
	if idx := bytes.IndexByte(buf[:n], '\n'); idx >= 0 && idx+1 <= n {
		return buf[idx+1 : n], true, nil
	}
	return buf[:n], true, nil
}

// lastLines keeps the final maxLines lines of s, reporting whether earlier
// lines were dropped.
func lastLines(s string, maxLines int) (string, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s, false
	}
	return strings.Join(lines[len(lines)-maxLines:], "\n"), true
}
