package sessionenv

import (
	"fmt"
	"testing"
)

func TestProbeDebug3(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}}
	for _, tc := range []string{
		`watch 'pwsh -ex[ec] x'`,
		`pwsh '-ex[ec]' x`,
		`pwsh '-ex[ec] x'`,
		`watch 'pwsh -ex x'`,
		`watch 'echo -ex[ec] x'`,
		`watch 'pwsh -e[nc] x'`,
	} {
		fmt.Printf("%-28q => %v\n", tc, commandMutatesAccountEnvironment(tc, names))
	}
}
