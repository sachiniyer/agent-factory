package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// parseGitConfigCount mirrors git's strtoul on GIT_CONFIG_COUNT: the leading
// run of base-10 digits is the count, anything else yields 0.
func TestParseGitConfigCount(t *testing.T) {
	cases := map[string]int{
		"":      0,
		"0":     0,
		"1":     1,
		"2":     2,
		" 3":    3,  // strtoul skips leading whitespace
		"12abc": 12, // stops at the first non-digit
		"abc":   0,  // no leading digit -> 0
		"-1":    0,  // a leading '-' is not a non-negative digit -> 0
		"007":   7,
	}
	for value, want := range cases {
		require.Equal(t, want, parseGitConfigCount(value), "parseGitConfigCount(%q)", value)
	}
}

// ValidateGitConfigCountForward refuses a count-only forward whose value
// resolves to N>0 and accepts every complete, empty, zero, or absent forward.
func TestValidateGitConfigCountForward(t *testing.T) {
	source := func(count string, pairs ...string) []string {
		out := []string{"PATH=/bin"}
		if count != "" {
			out = append(out, "GIT_CONFIG_COUNT="+count)
		}
		for i, p := range pairs {
			out = append(out, "GIT_CONFIG_KEY_"+itoa(i)+"="+p)
			out = append(out, "GIT_CONFIG_VALUE_"+itoa(i)+"=/v")
		}
		return out
	}

	t.Run("count forwarded with value N>0 and no pairs -> refused, names missing indices", func(t *testing.T) {
		err := ValidateGitConfigCountForward([]string{"GIT_CONFIG_COUNT"}, source("2"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "GIT_CONFIG_COUNT (resolves to 2")
		require.Contains(t, err.Error(), "GIT_CONFIG_KEY_0")
		require.Contains(t, err.Error(), "GIT_CONFIG_KEY_1")
		require.Contains(t, err.Error(), "GIT_CONFIG_VALUE_0")
		require.Contains(t, err.Error(), "GIT_CONFIG_VALUE_1")
		require.Contains(t, err.Error(), "session_env_passthrough")
		// The error must not render any value (no credential disclosure).
		require.NotContains(t, err.Error(), "/v")
		require.True(t, strings.HasPrefix(err.Error(), "session_env_passthrough"),
			"the refusal must identify the offending config key")
	})

	t.Run("count forwarded with value N>0 and only some pairs -> refused, names only the missing", func(t *testing.T) {
		err := ValidateGitConfigCountForward(
			[]string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"},
			source("2", "safe.directory"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "resolves to 2")
		require.Contains(t, err.Error(), "GIT_CONFIG_KEY_1")
		require.Contains(t, err.Error(), "GIT_CONFIG_VALUE_1")
		require.NotContains(t, err.Error(), "GIT_CONFIG_KEY_0")
		require.NotContains(t, err.Error(), "GIT_CONFIG_VALUE_0")
	})

	t.Run("count forwarded with all required pairs -> accepted", func(t *testing.T) {
		err := ValidateGitConfigCountForward(
			[]string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
				"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"},
			source("2", "a", "b"))
		require.NoError(t, err)
	})

	t.Run("count forwarded with value 0 -> accepted (no pairs required)", func(t *testing.T) {
		err := ValidateGitConfigCountForward([]string{"GIT_CONFIG_COUNT"}, source("0"))
		require.NoError(t, err)
	})

	t.Run("count forwarded but absent in source -> accepted", func(t *testing.T) {
		err := ValidateGitConfigCountForward([]string{"GIT_CONFIG_COUNT"}, source(""))
		require.NoError(t, err)
	})

	t.Run("count forwarded and defined-empty in source -> accepted", func(t *testing.T) {
		err := ValidateGitConfigCountForward([]string{"GIT_CONFIG_COUNT"}, append(source(""), "GIT_CONFIG_COUNT="))
		require.NoError(t, err)
	})

	t.Run("count not forwarded but present in source -> accepted (not the operator's forward)", func(t *testing.T) {
		err := ValidateGitConfigCountForward(nil, source("5"))
		require.NoError(t, err)
	})

	t.Run("count forwarded with non-numeric value -> accepted (git reads 0)", func(t *testing.T) {
		err := ValidateGitConfigCountForward([]string{"GIT_CONFIG_COUNT"}, append(source(""), "GIT_CONFIG_COUNT=abc"))
		require.NoError(t, err)
	})

	t.Run("passthrough names are trimmed before membership is checked", func(t *testing.T) {
		err := ValidateGitConfigCountForward([]string{" GIT_CONFIG_COUNT "}, source("1"))
		require.Error(t, err, "a count surrounded by whitespace is still GIT_CONFIG_COUNT")
	})
}

// To require a pair, the operator names it without a value, exactly as
// session_env_passthrough carries it: ValidateGitConfigCountForward consults
// names only.
func TestValidateGitConfigCountForward_NeverRendersValues(t *testing.T) {
	// A sensitive value the operator happened to attach to an indexed pair in
	// source must not appear in the error (the boundary never echoes values).
	err := ValidateGitConfigCountForward(
		[]string{"GIT_CONFIG_COUNT"},
		[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=secret-do-not-render", "GIT_CONFIG_VALUE_0=also-secret"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-do-not-render")
	require.NotContains(t, err.Error(), "also-secret")
}

// NeutralizeIncompleteGitConfigCount pins the count empty when a live count
// arrives without its required pairs, and leaves complete or zero counts
// untouched.
func TestNeutralizeIncompleteGitConfigCount(t *testing.T) {
	t.Run("count N>0 with no pairs -> pinned empty", func(t *testing.T) {
		in := []string{"PATH=/bin", "GIT_CONFIG_COUNT=2", "HOME=/h"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.True(t, ok)
		v, present := envEntryValue(out, "GIT_CONFIG_COUNT")
		require.True(t, present)
		require.Empty(t, v, "the count must be pinned defined-empty")
		require.Contains(t, out, "PATH=/bin")
		require.Contains(t, out, "HOME=/h")
	})

	t.Run("count N>0 with only some pairs -> pinned empty", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.True(t, ok)
		v, _ := envEntryValue(out, "GIT_CONFIG_COUNT")
		require.Empty(t, v)
	})

	t.Run("count N>0 with all required pairs -> untouched", func(t *testing.T) {
		in := []string{
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=k0", "GIT_CONFIG_VALUE_0=v0",
			"GIT_CONFIG_KEY_1=k1", "GIT_CONFIG_VALUE_1=v1",
		}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.False(t, ok)
		require.Equal(t, in, out, "a complete forward must pass through unchanged")
	})

	t.Run("count 0 -> untouched", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=0"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.False(t, ok)
		require.Equal(t, in, out)
	})

	t.Run("count defined-empty -> untouched", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT="}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.False(t, ok)
		require.Equal(t, in, out)
	})

	t.Run("count absent -> untouched", func(t *testing.T) {
		in := []string{"PATH=/bin"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.False(t, ok)
		require.Equal(t, in, out)
	})

	t.Run("duplicated count entry is replaced exactly once", func(t *testing.T) {
		in := []string{"GIT_CONFIG_COUNT=3", "GIT_CONFIG_COUNT=3"}
		out, ok := NeutralizeIncompleteGitConfigCount(in)
		require.True(t, ok)
		seen := 0
		for _, e := range out {
			if e == "GIT_CONFIG_COUNT=" {
				seen++
			}
		}
		require.Equal(t, 1, seen, "the count must be blanked once, not duplicated")
	})
}

// GitConfigCountForwardMissingPairs returns the names a forward is missing,
// sorted, and nil when the forward is complete.
func TestGitConfigCountForwardMissingPairs(t *testing.T) {
	require.Equal(t,
		[]string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_VALUE_1"},
		GitConfigCountForwardMissingPairs([]string{"GIT_CONFIG_COUNT"}, []string{"GIT_CONFIG_COUNT=2"}))
	require.Nil(t, GitConfigCountForwardMissingPairs(
		[]string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"},
		[]string{"GIT_CONFIG_COUNT=1"}))
	require.Nil(t, GitConfigCountForwardMissingPairs([]string{"GIT_CONFIG_COUNT"}, []string{"GIT_CONFIG_COUNT=0"}))
	require.Nil(t, GitConfigCountForwardMissingPairs(nil, []string{"GIT_CONFIG_COUNT=5"}))
}

func itoa(i int) string {
	// strconv.Itoa, kept local to the test for readability.
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return string(b)
}
