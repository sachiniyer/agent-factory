package envcommand

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		policy Policy
		want   Invocation
		bad    bool
	}{
		{name: "attached unset", args: []string{"-uCODEX_HOME", "codex"}, want: Invocation{Mutations: []Mutation{{Name: "CODEX_HOME", Unset: true}}, CommandIndex: 1}},
		{name: "separate unset", args: []string{"-u", "CODEX_HOME", "codex"}, want: Invocation{Mutations: []Mutation{{Name: "CODEX_HOME", Unset: true}}, CommandIndex: 2}},
		{name: "attached chdir", args: []string{"-C/tmp", "codex"}, want: Invocation{Chdir: "/tmp", CommandIndex: 1}},
		{name: "separate chdir", args: []string{"-C", "/tmp", "codex"}, want: Invocation{Chdir: "/tmp", CommandIndex: 2}},
		{name: "clear assign and chdir", args: []string{"-i", "--chdir=/tmp", "HOME=/home/agent", "codex"}, policy: Policy{AllowAssignments: true}, want: Invocation{ClearEnvironment: true, Chdir: "/tmp", Mutations: []Mutation{{Name: "HOME", Value: "/home/agent"}}, CommandIndex: 3}},
		{name: "guard rejects assignment", args: []string{"HOME=/tmp", "codex"}, bad: true},
		{name: "resolver rejects dynamic assignment", args: []string{"HOME=$OTHER", "codex"}, policy: Policy{AllowAssignments: true}, bad: true},
		{name: "resolver rejects dynamic chdir", args: []string{"-C", "$OTHER", "codex"}, policy: Policy{AllowAssignments: true}, bad: true},
		{name: "reject split string", args: []string{"-S", "codex"}, bad: true},
		{name: "reject help before command", args: []string{"--help", "codex"}, bad: true},
		{name: "reject version before command", args: []string{"--version", "codex"}, bad: true},
		{name: "reject short null with command", args: []string{"-0", "codex"}, bad: true},
		{name: "reject long null with command", args: []string{"--null", "codex"}, bad: true},
		{name: "reject unknown option", args: []string{"--future", "codex"}, bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.args, tc.policy)
			if tc.bad {
				require.ErrorIs(t, err, ErrUnsupported)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
