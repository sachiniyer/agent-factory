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
	if commandMutatesAccountEnvironment(command, names) {
		return false
	}
	if commandFeedsProvenShell(command, depth) {
		return false
	}
	return decodedShellPayloadVerdict(command, names, depth) == shellPayloadClean
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

// callRunnerPayloadCandidates returns the `-c` payload candidates for the
// FIRST literal argv word naming a command-string runner. Decoding only
// the first runner head is complete: every later runner head in the same
// call is itself a word inside this tail's candidate set, and decoding
// only once keeps a call with N runner words O(N) instead of O(N²) (Codex
// P2 on this head).
func callRunnerPayloadCandidates(args []*syntax.Word) []string {
	for i := range args {
		literal, ok := launcherLiteralWord(args[i])
		if !ok {
			continue
		}
		base := filepath.Base(literal)
		if _, ok := commandRunnerPrograms[base]; !ok {
			continue
		}
		return runnerPayloadCandidates(base, args[i+1:])
	}
	return nil
}
