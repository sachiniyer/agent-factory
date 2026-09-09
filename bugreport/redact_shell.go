package bugreport

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"mvdan.cc/sh/v3/syntax"
)

// configShellValues mirrors only the global config fields whose consumers hand
// operator-authored values to /bin/sh -c. Decoding this projection identifies
// shell context without treating every quoted config value as shell source.
type configShellValues struct {
	ProgramOverrides map[string]string             `json:"program_overrides" toml:"program_overrides"`
	OnArchiveCommand string                        `json:"on_archive_command" toml:"on_archive_command"`
	SandboxSSH       string                        `json:"sandbox_ssh" toml:"sandbox_ssh"`
	Sandbox          configShellSandbox            `json:"sandbox" toml:"sandbox"`
	RootAgent        configShellProgram            `json:"root_agent" toml:"root_agent"`
	RootAgents       map[string]configShellProgram `json:"root_agents" toml:"root_agents"`
}

type configShellProgram struct {
	Program string `json:"program" toml:"program"`
}

type configShellSandbox struct {
	SSH string `json:"ssh" toml:"ssh"`
}

// noteConfigShellCommands records decoded command values before the raw config
// is scrubbed. A malformed config has no trustworthy grammar projection and
// registers nothing; ordinary root/text defenses still run over its bytes.
func (r *redactor) noteConfigShellCommands(data []byte, format string) {
	var values configShellValues
	var err error
	switch format {
	case "json":
		err = json.Unmarshal(data, &values)
	case "toml":
		err = toml.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &values)
	default:
		return
	}
	if err != nil {
		return
	}
	for _, command := range values.ProgramOverrides {
		r.noteShellCommand(command)
	}
	r.noteShellCommand(values.OnArchiveCommand)
	r.noteShellCommand(values.SandboxSSH)
	r.noteShellCommand(values.Sandbox.SSH)
	r.noteShellCommand(values.RootAgent.Program)
	for _, agent := range values.RootAgents {
		r.noteShellCommand(agent.Program)
	}
}

func (r *redactor) noteShellCommand(command string) {
	if strings.TrimSpace(command) == "" {
		return
	}
	if r.shellCommands == nil {
		r.shellCommands = make(map[string]struct{})
	}
	r.shellCommands[command] = struct{}{}
}

// appendKnownShellCommandPathSpans locates exact decoded command values in the
// surrounding config text, then offsets shell-owned path candidates back into
// that text. The quoted-value pass covers encoded spellings of the same value.
func (r *redactor) appendKnownShellCommandPathSpans(spans []redactionSpan, s string) []redactionSpan {
	for command := range r.shellCommands {
		commandSpans := r.shellCommandPathSpans(command)
		if len(commandSpans) == 0 {
			continue
		}
		for scan := 0; scan <= len(s)-len(command); {
			rel := strings.Index(s[scan:], command)
			if rel < 0 {
				break
			}
			start := scan + rel
			for _, span := range commandSpans {
				span.start += start
				span.end += start
				spans = append(spans, span)
			}
			scan = start + len(command)
		}
	}
	return spans
}

func (r *redactor) shellCommandPathSpans(command string) []redactionSpan {
	context, ok := parseShellPathContext(command)
	if !ok {
		return nil
	}
	endsAt := func(s string, start, end int) bool {
		return pathEndsAt(s, start, end) || context.expansionStartsAt(start, end)
	}
	worktreeBoundary := func(s string, start, end int) bool {
		return derivedWorktreePathBoundaryWithEnd(s, start, end, endsAt)
	}
	rootBoundary := func(s string, start, end int) bool {
		return knownRootTextBoundaryWithEnd(s, start, end, endsAt)
	}
	spans := r.appendWorktreePathTitleSpansWithBoundary(nil, command, worktreeBoundary)
	spans = r.appendWorktreeSubdirectoryTitleSpansWithBoundary(spans, command, worktreeBoundary)
	return r.appendKnownRootSpansWithBoundary(spans, command, rootBoundary)
}

type shellWordRange struct {
	start int
	end   int
}

type shellPathContext struct {
	expansions map[int][]shellWordRange
}

func parseShellPathContext(command string) (shellPathContext, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return shellPathContext{}, false
	}
	context := shellPathContext{expansions: make(map[int][]shellWordRange)}
	syntax.Walk(file, func(node syntax.Node) bool {
		word, ok := node.(*syntax.Word)
		if !ok {
			return true
		}
		wordRange := shellWordRange{start: int(word.Pos().Offset()), end: int(word.End().Offset())}
		syntax.Walk(word, func(part syntax.Node) bool {
			switch part.(type) {
			case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst:
				start := int(part.Pos().Offset())
				context.expansions[start] = append(context.expansions[start], wordRange)
				return false
			}
			return true
		})
		return false
	})
	return context, true
}

func (c shellPathContext) expansionStartsAt(start, end int) bool {
	for _, word := range c.expansions[end] {
		if start >= word.start && end <= word.end {
			return true
		}
	}
	return false
}
