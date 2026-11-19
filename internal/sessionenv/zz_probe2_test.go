package sessionenv

import (
	"fmt"
	"testing"
)

func TestProbeDebug(t *testing.T) {
	for _, lit := range []string{"-ex[ec]", "-[-]encodedcommand", "-[-]enc:x", "-encodedcomman[d]", "*.log", "-e[nc]"} {
		h := literalNamesHiddenInput(lit, "pwsh", newReachBudget())
		p := literalSpellsTailPayload(lit, "pwsh", newReachBudget())
		g := globLiteralSpellsOption(lit, "pwsh", namedLauncherHiddenWords["pwsh"], true, newReachBudget())
		fmt.Printf("%-22q hidden=%v payload=%v glob=%v\n", lit, h, p, g)
	}
	for _, lit := range []string{"--exp[r]", "-e[x]", "-i[x]", "--[s]tdin"} {
		h := literalNamesHiddenInput(lit, "nix", newReachBudget())
		hn := literalNamesHiddenInput(lit, "nu", newReachBudget())
		fmt.Printf("%-22q nix=%v nu=%v\n", lit, h, hn)
	}
}
