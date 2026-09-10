package bugreport

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// accountsUnredactedWarning is the tail of every message this file reports when
// the accounts registry cannot be read. It is what makes the failure honest: the
// bundle is still produced — a user filing a bug is the last person who should be
// denied a report — but it says, in its own collection errors, that the pass which
// removes account labels did not run. Silence is the one outcome not allowed: it
// makes an unswept bundle look swept.
const accountsUnredactedWarning = "so any account label in this bundle is NOT redacted — review it before sharing"

// noteRegisteredAccounts records every registered account label so scrub can
// strip it from text the bundle otherwise keeps verbatim (#3871).
//
// The record-side policy has always replaced InstanceData.Account with the marker
// (#3051, #3588). The label reaches a bundle by two other routes, though, and
// naming the account is the POINT of both: a login completion that does not say
// which identity it moved to answers nothing, and the config file's default
// account is a setting the operator asked to see. So the label comes out at
// BUNDLE time rather than at log time, which leaves the operator's own
// agent-factory.log intact — the person reading it on their own box is exactly
// who needs to see "acme-prod".
//
// The set is READ, not guessed. An account label is free text a user chose, with
// no shape a matcher could key on; a regex over "account %q" forms would miss the
// emitters that phrase it differently — accountlogin.go's unauthenticated-account
// error renders the same name a second time as a bare "--account %s" — and would
// eat unrelated text besides. The registry is a directory tree under the AF home,
// so the exact set of names is known at bundle time, which is what makes this
// cheap.
//
// Every account-scoped agent is read, not just the one this box happens to use: a
// label registered for codex reaches the log the same way a claude one does.
func (r *redactor) noteRegisteredAccounts() error {
	home, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("accounts: cannot resolve the agent-factory home (%v), %s", err, accountsUnredactedWarning)
	}
	var failures []string
	for _, agent := range sessionenv.AccountAgents() {
		names, listErr := agentaccount.List(home, agent)
		// Keep going rather than returning: one unreadable agent directory must not
		// discard the labels the other agents did yield, or a single broken path
		// would silently un-redact every account on the box.
		if listErr != nil {
			failures = append(failures, fmt.Sprintf("%s (%v)", agent, listErr))
			continue
		}
		for _, name := range names {
			r.noteAccount(name)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("accounts: cannot read the registry for %s, %s",
			strings.Join(failures, "; "), accountsUnredactedWarning)
	}
	return nil
}

// noteAccount records one account label, skipping blanks. Registration is split
// from the read above so a test can pin the sweep without planting a directory
// tree, exactly as noteTitle does for titles.
func (r *redactor) noteAccount(name string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	if r.accounts == nil {
		r.accounts = make(map[string]struct{})
	}
	r.accounts[name] = struct{}{}
}

// isAccountNameRune reports whether r can appear inside an account name, and so
// whether a neighboring rune CONTINUES a label rather than bounding it. It is
// agentaccount.ValidateName's alphabet — letters, digits, '.', '-', '_' — widened
// to the non-ASCII word runes isWordRune already knows, so a label is not matched
// against a fragment of a word in some other script either.
func isAccountNameRune(r rune) bool {
	return r == '.' || r == '-' || isWordRune(r)
}
