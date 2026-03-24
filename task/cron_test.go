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

func TestCronToOnCalendarSimple(t *testing.T) {
	result, err := CronToOnCalendar("30 2 * * *")
	require.NoError(t, err)
	assert.Equal(t, "*-*-* 02:30:00", result)
}

func TestCronToOnCalendarRange(t *testing.T) {
	result, err := CronToOnCalendar("0 9-17 * * *")
	require.NoError(t, err)
	assert.Equal(t, "*-*-* 09..17:00:00", result)
}

func TestCronToOnCalendarRangeWithStep(t *testing.T) {
	result, err := CronToOnCalendar("0 9-17/2 * * *")
	require.NoError(t, err)
	assert.Equal(t, "*-*-* 09..17/2:00:00", result)
}

func TestCronToOnCalendarList(t *testing.T) {
	result, err := CronToOnCalendar("0 1,3,5 * * *")
	require.NoError(t, err)
	assert.Equal(t, "*-*-* 01,03,05:00:00", result)
}

func TestCronToOnCalendarMonthRange(t *testing.T) {
	result, err := CronToOnCalendar("0 0 1 6-8 *")
	require.NoError(t, err)
	assert.Equal(t, "*-06..08-01 00:00:00", result)
}

func TestCronToOnCalendarDOMRange(t *testing.T) {
	result, err := CronToOnCalendar("0 0 1-15 * *")
	require.NoError(t, err)
	assert.Equal(t, "*-*-01..15 00:00:00", result)
}

func TestCronToOnCalendarMinuteRange(t *testing.T) {
	result, err := CronToOnCalendar("0-30/10 * * * *")
	require.NoError(t, err)
	assert.Equal(t, "*-*-* *:00..30/10:00", result)
}

func TestCronToOnCalendarDOW(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 1-5")
	require.NoError(t, err)
	assert.Equal(t, "Mon..Fri *-*-* 09:00:00", result)
}
