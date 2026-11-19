package sessionenv

import (
	"fmt"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

func TestProbeDebug5(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}}
	fmt.Printf("nestedFields(-ex[ec],pwsh) = %v\n",
		namedNestedFieldsRefuse([]string{"-ex[ec]"}, "pwsh", names, nil, newReachBudget()))
	fmt.Printf("nestedFields(-ex[ec] x,pwsh) = %v\n",
		namedNestedFieldsRefuse([]string{"-ex[ec]", "x"}, "pwsh", names, nil, newReachBudget()))
	fmt.Printf("globOpt(-ex[ec],pwsh,hidden) = %v\n",
		globLiteralSpellsOption("-ex[ec]", "pwsh", namedLauncherHiddenWords["pwsh"], true, newReachBudget()))
	fmt.Printf("globOpt(-ex[ec],pwsh,payload) = %v\n",
		globLiteralSpellsOption("-ex[ec]", "pwsh", namedLauncherTailPayloadWords["pwsh"], false, newReachBudget()))
	// What does -abc[de] hit that -ex[ec] doesn't?  and -ex[e]?
	for _, lit := range []string{"-abc[de]", "-ex[e]", "-ex[ec]", "-ab", "-e[e]"} {
		fmt.Printf("globOpt(%s,hidden)=%v payload=%v\n", lit,
			globLiteralSpellsOption(lit, "pwsh", namedLauncherHiddenWords["pwsh"], true, newReachBudget()),
			globLiteralSpellsOption(lit, "pwsh", namedLauncherTailPayloadWords["pwsh"], false, newReachBudget()))
	}
	var p syntax.Parser
	_ = p
}
