package task

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCronFieldExpandWildcard(t *testing.T) {
	vals, err := expandCronField("*", 0, 59)
	assert.NoError(t, err)
	assert.Nil(t, vals)
}

func TestCronFieldExpandSingle(t *testing.T) {
	vals, err := expandCronField("30", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{30}, vals)
}

func TestCronFieldExpandList(t *testing.T) {
	vals, err := expandCronField("1,3,5", 0, 7)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 3, 5}, vals)
}

func TestCronFieldExpandRange(t *testing.T) {
	vals, err := expandCronField("1-5", 0, 7)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3, 4, 5}, vals)
}

func TestCronFieldExpandStep(t *testing.T) {
	vals, err := expandCronField("*/15", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 15, 30, 45}, vals)
}

func TestCronFieldExpandRangeWithStep(t *testing.T) {
	vals, err := expandCronField("0-10/3", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 3, 6, 9}, vals)
}

func TestCronFieldExpandDedup(t *testing.T) {
	vals, err := expandCronField("1,1,3", 0, 7)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 3}, vals)
}
