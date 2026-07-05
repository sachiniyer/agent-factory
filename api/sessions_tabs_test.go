package api

import (
	"testing"

	"github.com/spf13/cobra"
)

// findSub returns the direct subcommand of parent whose first Use word matches
// name, or nil.
func findSub(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// TestSessionsTabsAliasesRegistered pins the #1192 additive aliases: the
// noun-subcommand `sessions tabs {create,delete}` group exists alongside the
// original hyphen verbs (which must stay for scripts), and each alias carries
// the same required flags as the verb it mirrors.
func TestSessionsTabsAliasesRegistered(t *testing.T) {
	// Hyphen verbs must still be present — removing them would break scripts.
	if findSub(SessionsCmd, "tab-create") == nil {
		t.Error("sessions tab-create must remain registered")
	}
	if findSub(SessionsCmd, "tab-delete") == nil {
		t.Error("sessions tab-delete must remain registered")
	}

	tabs := findSub(SessionsCmd, "tabs")
	if tabs == nil {
		t.Fatal("sessions tabs group not registered")
	}

	create := findSub(tabs, "create")
	if create == nil {
		t.Fatal("sessions tabs create not registered")
	}
	if create.RunE == nil {
		t.Error("sessions tabs create has no RunE")
	}
	if create.Flag("command") == nil {
		t.Error("sessions tabs create missing --command flag")
	}
	if create.Flag("name") == nil {
		t.Error("sessions tabs create missing --name flag")
	}

	del := findSub(tabs, "delete")
	if del == nil {
		t.Fatal("sessions tabs delete not registered")
	}
	if del.RunE == nil {
		t.Error("sessions tabs delete has no RunE")
	}
	if del.Flag("name") == nil {
		t.Error("sessions tabs delete missing --name flag")
	}
}
