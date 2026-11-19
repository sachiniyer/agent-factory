package sessionenv

import (
	"fmt"
	"testing"
)

func TestProbeDebug4(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}}
	for _, tc := range []string{
		`watch 'pwsh -ex[ec]'`,
		`watch 'pwsh -ex[e] x'`,
		`watch 'nix -ex[ec] x'`,
		`watch 'pwsh -ex[ec]'`,
		`watch 'pwsh -abc[de] x'`,
		`watch 'pwsh -ex x'`,
		`watch 'pwsh -ex[ x'`,
	} {
		fmt.Printf("%-26q => %v\n", tc, commandMutatesAccountEnvironment(tc, names))
	}
	// isolate the verdict pieces for -ex[ec] under pwsh
	v := namedLiteralVerdict("-ex[ec]", false, false, "pwsh", names, nil, newReachBudget())
	fmt.Printf("verdict(-ex[ec],pwsh) = %+v\n", v)
	v2 := namedLiteralVerdict("-ex[ec]", true, false, "pwsh", names, nil, newReachBudget())
	fmt.Printf("verdict(-ex[ec],pwsh,last) = %+v\n", v2)
	fmt.Printf("textRefuses(-ex[ec]) = %v\n", namedLauncherTextRefuses("-ex[ec]", names, nil, newReachBudget()))
}
