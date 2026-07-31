package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCopiedDirectoryRoutesRetainLinearAncestry(t *testing.T) {
	const depth = 128
	manifest := copiedDirectory{}
	current := &manifest
	for range depth {
		child := &copiedDirectory{}
		current.entries = []copiedEntry{{name: "d", directory: child}}
		current = child
	}

	routes := copiedDirectoryRoutes(&manifest)
	assert.LessOrEqual(t, retainedRouteEntries(routes), depth*2,
		"cleanup routes must not retain a complete ancestry copy for every directory")
}
