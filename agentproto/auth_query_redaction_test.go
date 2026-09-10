package agentproto

import (
	"strings"
	"testing"
)

func TestRedactAccessTokenURLRawQueryCrossProduct(t *testing.T) {
	keys := []struct {
		name string
		raw  string
	}{
		{name: "literal", raw: "access_token"},
		{name: "percent encoded", raw: "%61ccess%5Ftoken"},
		{name: "mixed case", raw: "AcCeSs_ToKeN"},
		{name: "doubly encoded", raw: "%2561ccess%255Ftoken"},
	}
	separators := []struct {
		name   string
		before string
		after  string
	}{
		{name: "ampersand", before: "before=1&", after: "&after=2"},
		{name: "semicolon", before: "before=1;", after: ";after=2"},
		{name: "none"},
	}
	values := []struct {
		name string
		raw  string
	}{
		{name: "present", raw: "TOKSECRET"},
		{name: "empty"},
	}

	for _, key := range keys {
		for _, separator := range separators {
			for _, value := range values {
				name := strings.Join([]string{key.name, separator.name, value.name}, "/")
				t.Run(name, func(t *testing.T) {
					prefix := "http://localhost:3000/?"
					input := prefix + separator.before + key.raw + "=" + value.raw + separator.after
					want := prefix + separator.before + key.raw + "=" + accessTokenRedaction + separator.after
					if got := RedactAccessTokenURL(input); got != want {
						t.Fatalf("RedactAccessTokenURL(%q) = %q, want %q", input, got, want)
					}
				})
			}
		}
	}
}
