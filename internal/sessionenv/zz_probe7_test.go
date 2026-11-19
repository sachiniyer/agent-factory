package sessionenv

import (
	"fmt"
	"testing"
)

func TestProbeDebug7(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}}
	fmt.Printf("text(pwsh -ex[ec]) = %v\n", namedLauncherTextRefuses("pwsh -ex[ec]", names, nil, newReachBudget()))
	fmt.Printf("text(pwsh -ex[e] x) = %v\n", namedLauncherTextRefuses("pwsh -ex[e] x", names, nil, newReachBudget()))
	for _, pc := range []string{"pwsh", "-ex[ec]", "ec", "x", "-ex", "pwsh -ex[ec]"} {
		fmt.Printf("piece(%-14q) refuses=%v names=%v\n", pc,
			namedLauncherPieceRefuses(pc, names, nil, newReachBudget()),
			namedLauncherNamesOf(pc, newReachBudget()))
	}
	fmt.Printf("nestedFields([pwsh -ex[ec]], watch) = %v\n",
		namedNestedFieldsRefuse([]string{"pwsh", "-ex[ec]"}, "watch", names, nil, newReachBudget()))
}
