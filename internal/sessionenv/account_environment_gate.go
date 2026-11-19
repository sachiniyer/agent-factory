package sessionenv

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// accountEnvironmentMaxPayloadDepth bounds how deep a `-c` payload can
// decode into another `-c` payload before the gate stops trusting what it
// can see (`sh -c 'bash -c "sh -c ..."'`). Each level strictly shrinks the
// string under judgement, so the recursion always terminates; the bound
// just keeps a pathological nesting level cheap, and hitting it counts as
// unproven rather than admitted.
const accountEnvironmentMaxPayloadDepth = 8

// shellPayloadVerdict classifies what the first failing check inside a
// decoded `-c` command string was, so the refusal message can name the
// real problem rather than the option that carried it.
type shellPayloadVerdict int

const (
	shellPayloadClean shellPayloadVerdict = iota
	shellPayloadMutates
	shellPayloadFeeds
	shellPayloadUnproven
)

// accountEnvironmentGateProven applies every check the top-level entry
// point applies to a command line — the environment-mutation walk, the
// proven-shell stdin-feed check, and the same payload decode again — to a
// decoded `-c` command string. A payload is not a reduced judgement:
// `sh -c '/bin/sh -i < ./script'` feeds a trusted shell code the outer
// argv never spelled, and `sh -c 'xargs -a secrets'` hides input exactly
// the way the bare line does (Codex P1 on this head). depth is the
// remaining decode budget; at zero the payload is unproven.
func accountEnvironmentGateProven(command string, names map[string]struct{}, depth int) bool {
	if depth <= 0 {
		return false
	}
	// The payload verdict runs first: it is the cheap check (one linear
	// collection plus a deduped candidate set) and it refuses early the
	// shapes the mutation walk would spend its shadowed-child budget on —
	// a later `sh` word is itself a candidate whose bare-shell judgement
	// is unproven (independent-review P2 on this head).
	if decodedShellPayloadVerdict(command, names, depth) != shellPayloadClean {
		return false
	}
	if commandMutatesAccountEnvironment(command, names) {
		return false
	}
	return !commandFeedsProvenShell(command, depth)
}

// decodedShellPayloadVerdict reports the first failing check inside any
// `-c`-style command string in command — clean when every decodable
// payload is judged clean. The scan is position-blind for the same reason
// the stdin-feed check is: which word a launcher or wrapper hands its
// payload to is not a local question, so the FIRST argv suffix in each
// call headed by a command-string runner (a shell, su, runuser or script)
// contributes every candidate the tail can produce — `x sh -c 'unset
// CODEX_HOME'` hides the same mutation a top-level `sh -c` does. One
// collection per call keeps the walk linear: a later runner head's
// candidates are already words inside the first head's tail. Each
// candidate is judged by the shared entry point, so a mutating, a
// stdin-fed, or an undecodable payload refuses wherever it sits.
func decodedShellPayloadVerdict(command string, names map[string]struct{}, depth int) shellPayloadVerdict {
	if command == "" {
		return shellPayloadClean
	}
	verdict := shellPayloadClean
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(command), "")
		if err != nil {
			// commandMutatesAccountEnvironment already refused a command
			// either variant cannot parse; there is nothing here to
			// answer "clean" about text that was not read.
			continue
		}
		if v := filePayloadVerdict(file, names, depth); v != shellPayloadClean {
			return v
		}
	}
	return verdict
}

func filePayloadVerdict(file *syntax.File, names map[string]struct{}, depth int) shellPayloadVerdict {
	verdict := shellPayloadClean
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		for _, payload := range callRunnerPayloadCandidates(call.Args) {
			switch {
			case depth <= 0:
				verdict = shellPayloadUnproven
			case commandMutatesAccountEnvironment(payload, names):
				verdict = shellPayloadMutates
			case commandFeedsProvenShell(payload, depth-1):
				verdict = shellPayloadFeeds
			default:
				if v := decodedShellPayloadVerdict(payload, names, depth-1); v != shellPayloadClean {
					verdict = v
				}
			}
			if verdict != shellPayloadClean {
				return false
			}
		}
		return true
	})
	return verdict
}

// callRunnerPayloadCandidates returns the `-c` payload candidates for a
// call, decoded with EACH runner family's grammar: the first shell-named
// word contributes the literals after the first `-c` cluster in its tail
// (every later shell head's set is a subset — its `-c` cluster lies inside
// the same range), and the first su/runuser/script-named word contributes
// the getopt payload spellings in its tail. Decoding each family once is
// complete — a word that merely precedes a runner (`env -u su sh -ce x`)
// cannot hide the shell's payload behind su's attached-operand reading —
// and collecting each family's union once keeps the scan O(argv) rather
// than O(runners × argv) (independent-review P1/P2 on this head).
// Candidates are deduped so a repeated string is gated once.
func callRunnerPayloadCandidates(args []*syntax.Word) []string {
	var candidates []string
	seen := make(map[string]struct{})
	done := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		literal, ok := launcherLiteralWord(args[i])
		if !ok {
			continue
		}
		base := filepath.Base(literal)
		family, ok := runnerPayloadFamily(base)
		if !ok || done[family] {
			continue
		}
		done[family] = true
		for _, payload := range runnerPayloadCandidates(base, args[i+1:]) {
			if _, dup := seen[payload]; !dup {
				seen[payload] = struct{}{}
				candidates = append(candidates, payload)
			}
		}
	}
	return candidates
}

// runnerPayloadFamily reports the payload-collector family an argv word
// belongs to — one collector runs per family per call, and the first head's
// tail scan already covers every later head of the same family (the second
// independent-review P2 on #7604's head). pwsh and nu are absent: their
// payload grammar is not parsed — the launcher arm's boundary-name scan
// judges their -c operand instead (approved rule on #7603).
func runnerPayloadFamily(base string) (string, bool) {
	if _, ok := shellBaseName(base); ok {
		return "shell", true
	}
	switch base {
	case "su", "runuser", "script":
		return "runner", true
	case "watch", "ssh", "tmux", "nix-shell", "nix", "distrobox", "toolbox":
		return base, true
	}
	return "", false
}
