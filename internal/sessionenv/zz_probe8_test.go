package sessionenv

import (
	"fmt"
	"testing"
)

func TestProbeDebug8(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}}
	for _, lit := range []string{"pwsh -ex[ec]", "pwsh -ex[ec] x", "pwsh -abc[de] x", "pwsh -ex[e] x", "pwsh -ex x"} {
		for _, last := range []bool{false, true} {
			fmt.Printf("verdict(%-22q watch last=%v) = %+v\n", lit, last,
				namedLiteralVerdict(lit, last, false, "watch", names, nil, newReachBudget()))
		}
	}
	fmt.Printf("verdict(-ex[ec] pwsh last) = %+v\n",
		namedLiteralVerdict("-ex[ec]", true, false, "pwsh", names, nil, newReachBudget()))
	fmt.Printf("hidden(pwsh -ex[ec],watch) = %v\n",
		literalNamesHiddenInput("pwsh -ex[ec]", "watch", newReachBudget()))
	fmt.Printf("payload(pwsh -ex[ec],watch) = %v\n",
		literalSpellsTailPayload("pwsh -ex[ec]", "watch", newReachBudget()))
	fmt.Printf("hidden(-ex[ec],pwsh) = %v\n",
		literalNamesHiddenInput("-ex[ec]", "pwsh", newReachBudget()))
}
