package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestAPICatalogListsOnComplete(t *testing.T) {
	assert.Contains(t, runAPICmd(t, true), "/v1/ListOnComplete")
}
