package sessionenv

import (
	"fmt"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

func TestProbeDebug6(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}}
	var p syntax.Parser
	parse := func(s string) []*syntax.Word {
		f, err := p.Parse(strings.NewReader(s), "")
		if err != nil || len(f.Stmts) == 0 {
			return nil
		}
		call, ok := f.Stmts[0].Cmd.(*syntax.CallExpr)
		if !ok {
			return nil
		}
		return call.Args
	}
	for _, cmd := range []string{
		`watch 'pwsh -ex[ec]'`,
		`watch 'pwsh -ex[e] x'`,
		`watch 'pwsh -abc[de] x'`,
		`watch 'pwsh -ex[ec] x'`,
	} {
		words := parse(cmd)
		memo := newOperandTailMemo()
		fmt.Printf("%-30q words=%d envInj=%v hideAssign=%v tail=%v\n", cmd, len(words),
			unwrapEnvInjectionLauncher(words[1:], names, false, false, memo),
			wrapperTailHidesAccountAssignment(words[1:], false, names, memo),
			namedLauncherTailRefuses(words[1:], names, nil, "watch", newReachBudget()))
	}
}
