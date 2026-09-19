package sessionenv

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
	"mvdan.cc/sh/v3/syntax"
)

// AgentForCommand returns a supported agent only when the complete command is
// one literal invocation of that agent (optionally through env or exec). Nested
// agent-server commands are never classified from strings; Docker/SSH use the
// effect-bound exec protocol instead. This result grants credentials to a
// process, so an agent-looking argument to an arbitrary executable must never
// match.
func AgentForCommand(command string) string {
	invocation, ok := literalAgentCommand(command, isTrustedEnvExecutable)
	if !ok {
		return ""
	}
	return invocation.agent
}

// AgentNamespaceForCommand derives the agent NAMESPACE from a command for the
// non-credential caller that resolves an already-validated account's directory
// (vscodeAccountScopeForInstance). Unlike AgentForCommand it recognizes any env
// wrapper whose basename is env — including operator-supplied path-qualified
// spellings such as /usr/local/bin/env — using the same env-basename rule the
// create-time gate tmux.DetectAgentExecutable keeps, so a path-qualified env
// wrapper the gate accepted onto a pre-#4356 session stays classifiable on
// restore. This shares only the env-basename rule, not a parser:
// DetectAgentExecutable and AgentNamespaceForCommand are otherwise independent
// (the gate uses splitShellTokens + baseCommand, this surface uses mvdan.cc/sh +
// filepath.Base) and agree on the env-wrapper spellings the #4356 fix targets,
// not on every command.
//
// It grants no credentials. The agent namespace it returns names a directory
// under the af home the operator already selected an account in; nothing about
// this call hands a forked process the credentials the directory holds. A
// caller that filters or installs credentials for an exec must use
// AgentForCommand (or credentialAgentForCommand) instead, which keep the strict
// isTrustedEnvExecutable env set so a repository-controlled ./env cannot turn an
// agent-looking argument into a grant.
func AgentNamespaceForCommand(command string) string {
	invocation, ok := literalAgentCommand(command, isEnvExecutableByBase)
	if !ok {
		return ""
	}
	return invocation.agent
}

type agentCommand struct {
	agent                       string
	executable                  string
	executableResolutionChanged bool
}

// literalAgentCommand parses one literal agent invocation, optionally through an
// env wrapper. envMatch decides which spellings count as the env wrapper: the
// strict isTrustedEnvExecutable for credential-bearing callers, and the lenient
// isEnvExecutableByBase for namespace-only callers that share the env-basename
// rule tmux.DetectAgentExecutable keeps for env-wrapper spellings.
func literalAgentCommand(command string, envMatch func(string) bool) (agentCommand, bool) {
	if strings.TrimSpace(command) == "" {
		return agentCommand{}, false
	}
	call, ok := singleSimpleCall(command)
	if !ok || !callIsLiteral(call) {
		return agentCommand{}, false
	}
	// Detection, so the separator is ignored: `exec -- claude` is still a command
	// about claude, and the account boundary refuses it with its own message.
	words, _ := stripExecPrefix(call.Args)
	args, _ := literalCommandArgs(words)
	if len(args) == 0 {
		return agentCommand{}, false
	}
	executable := args[0]
	resolutionChanged := shellAssignmentsChangeExecutableResolution(call.Assigns)
	if envMatch(args[0]) {
		invocation, err := envcommand.Parse(args[1:], envcommand.Policy{AllowAssignments: true})
		if err != nil || invocation.CommandIndex < 0 {
			return agentCommand{}, false
		}
		executable = args[1+invocation.CommandIndex]
		resolutionChanged = resolutionChanged || envChangesExecutableResolution(invocation)
	}
	agent := supportedAgent(executable)
	return agentCommand{
		agent:                       agent,
		executable:                  executable,
		executableResolutionChanged: resolutionChanged,
	}, agent != ""
}

// credentialAgentForCommand is the fail-closed classifier for an untrusted
// resolved program. Unlike AgentForCommand it preserves the executable token as
// identity-bearing data and admits only a bare supported-agent name whose
// resolution still uses the inherited operator environment. A slash selects an
// untrusted file directly; PATH changes, env clearing or PATH removal, and env
// chdir can select one indirectly.
//
// The residual accepted set is a literal bare agent invocation, optionally
// preceded by exec or one of the modelled system env spellings, with literal
// non-resolution assignments such as TERM, LANG, and agent cloud selectors.
// Those remain accepted because POSIX executable lookup does not consult them.
// Every path-qualified or resolution-changing form is deliberately
// credential-free: this boundary receives no trusted provenance for the file it
// selects. The command still launches, and an operator can explicitly authorize
// required names through session_env_passthrough.
func credentialAgentForCommand(command string) string {
	invocation, ok := literalAgentCommand(command, isTrustedEnvExecutable)
	if !ok || strings.Contains(invocation.executable, "/") || invocation.executableResolutionChanged {
		return ""
	}
	return invocation.agent
}

func shellAssignmentsChangeExecutableResolution(assignments []*syntax.Assign) bool {
	for _, assignment := range assignments {
		if assignment != nil && assignment.Name != nil && assignment.Name.Value == "PATH" {
			return true
		}
	}
	return false
}

func envChangesExecutableResolution(invocation envcommand.Invocation) bool {
	// Clearing the environment or changing cwd changes lookup whenever PATH is
	// absent or contains a relative entry. Neither can be proven harmless here.
	if invocation.ClearEnvironment || invocation.Chdir != "" {
		return true
	}
	for _, mutation := range invocation.Mutations {
		if mutation.Name == "PATH" {
			return true
		}
	}
	return false
}

func supportedAgent(command string) string {
	agent := strings.ToLower(filepath.Base(command))
	if _, ok := agentNames[agent]; ok {
		return agent
	}
	return ""
}

// commandEnvironmentFlagState recognizes a selector override only when the
// command is one simple invocation of the selected agent. In particular, an
// agent-looking word used as data, a compound command, a redirect, or a second
// possible execution path can never widen the credential allowlist. Nothing is
// expanded or executed.
func commandEnvironmentFlagState(command, agent, name string) (found, enabled bool) {
	if strings.TrimSpace(command) == "" || agent == "" {
		return false, false
	}
	call, ok := singleSimpleCall(command)
	if !ok {
		return false, false
	}

	return directAgentFlagState(call, agent, name)
}

func singleSimpleCall(command string) (*syntax.CallExpr, bool) {
	return singleCall(command, false)
}

// singleCallIgnoringRedirections is singleSimpleCall for callers that ask about
// the command's WORDS rather than about what af can prove it will do.
//
// A redirection makes a command unprovable — it is why singleSimpleCall refuses
// one — but it says nothing about the exec prefix: `exec -- claude >agent.log`
// still hands dash `--` as the command name, and dash still exits 127. A caller
// that rejected the whole string because it ends in `>agent.log` would answer
// "no separator here" about a command that has one (#3566).
func singleCallIgnoringRedirections(command string) (*syntax.CallExpr, bool) {
	return singleCall(command, true)
}

// singleCall is the one parse behind both. allowRedirections relaxes exactly one
// rule and nothing else, so the two questions cannot drift apart on any of the
// others.
func singleCall(command string, allowRedirections bool) (*syntax.CallExpr, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 {
		return nil, false
	}
	stmt := file.Stmts[0]
	if stmt == nil || stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown {
		return nil, false
	}
	if !allowRedirections && len(stmt.Redirs) != 0 {
		return nil, false
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return nil, false
	}
	return call, true
}

func directAgentFlagState(call *syntax.CallExpr, agent, name string) (found, enabled bool) {
	words, _ := stripExecPrefix(call.Args)
	if len(words) == 0 {
		return false, false
	}

	allLiteral := callIsLiteral(call)
	if isTrustedEnvWord(words[0]) {
		found, enabled, ok := envAgentFlagState(call.Assigns, words[1:], agent, name)
		if !ok || !found {
			return false, false
		}
		if !allLiteral {
			return true, false
		}
		return true, enabled
	}
	if !wordBaseEquals(words[0], agent) {
		return false, false
	}
	found, enabled = callAssignmentFlagState(call.Assigns, name)
	if !found {
		return false, false
	}
	if !allLiteral {
		return true, false
	}
	return true, enabled
}

func envAgentFlagState(assignments []*syntax.Assign, words []*syntax.Word, agent, name string) (found, enabled, ok bool) {
	args := make([]string, 0, len(words))
	dynamicValues := make(map[string]struct{})
	for idx, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			assignmentName, assignment := shellWordAssignmentName(word)
			if assignment {
				value = fmt.Sprintf("%s=AF_DYNAMIC_VALUE_%d", assignmentName, idx)
				dynamicValues[value] = struct{}{}
			} else {
				// Parse still needs a placeholder to locate env's command. If this
				// dynamic word is the command, the placeholder cannot match agent;
				// if it follows the literal command, Parse has already stopped.
				value = fmt.Sprintf("AF_DYNAMIC_WORD_%d", idx)
			}
		}
		args = append(args, value)
	}
	invocation, err := envcommand.Parse(args, envcommand.Policy{AllowAssignments: true})
	if err != nil || invocation.CommandIndex < 0 || invocation.CommandIndex >= len(args) ||
		!strings.EqualFold(filepath.Base(args[invocation.CommandIndex]), agent) {
		return false, false, false
	}

	found, enabled = callAssignmentFlagState(assignments, name)
	if invocation.ClearEnvironment {
		found, enabled = true, false
	}
	for _, mutation := range invocation.Mutations {
		if mutation.Name != name {
			continue
		}
		found = true
		if mutation.Unset {
			enabled = false
			continue
		}
		_, dynamic := dynamicValues[mutation.Name+"="+mutation.Value]
		enabled = !dynamic && flagValueEnabled(mutation.Value)
	}
	return found, enabled, true
}

// agentServerProgram parses the exact argument order generated by the Docker and
// SSH launchers. It deliberately accepts no executable word: the effect-bound
// exec mode always re-execs the CURRENT af binary, so there is no name or path
// here that repository content can substitute.
func agentServerProgram(args []string) (string, bool) {
	if len(args) < 7 || args[0] != "agent-server" {
		return "", false
	}
	if args[1] != "--listen" || args[2] == "" || args[3] != "--repo" || args[4] == "" ||
		args[5] != "--title" || args[6] == "" {
		return "", false
	}
	program := "claude"
	idx := 7
	if idx < len(args) && args[idx] == "--program" {
		if idx+2 >= len(args) || args[idx+1] == "" || args[idx+2] != "--program-resolved" {
			return "", false
		}
		program = args[idx+1]
		idx += 3
	}
	for idx < len(args) {
		if args[idx] != "--session-env" || idx+1 >= len(args) || !validName(args[idx+1]) {
			return "", false
		}
		idx += 2
	}
	return program, true
}

func callIsLiteral(call *syntax.CallExpr) bool {
	for _, assignment := range call.Assigns {
		if assignment == nil || assignment.Name == nil || assignment.Append || assignment.Naked ||
			assignment.Index != nil || assignment.Array != nil || assignment.Value == nil {
			return false
		}
		if _, ok := literalShellWord(assignment.Value); !ok {
			return false
		}
	}
	_, literal := literalCommandArgs(call.Args)
	return literal
}

func callAssignmentFlagState(assignments []*syntax.Assign, name string) (found, enabled bool) {
	// Shell assignment prefixes are applied left-to-right, so the last
	// assignment to the selector decides its value. A dynamic value is an
	// explicit but unprovable override and therefore fails closed as disabled.
	for idx := len(assignments) - 1; idx >= 0; idx-- {
		assignment := assignments[idx]
		if assignment == nil || assignment.Name == nil || assignment.Name.Value != name {
			continue
		}
		if assignment.Append || assignment.Naked || assignment.Index != nil || assignment.Array != nil || assignment.Value == nil {
			return true, false
		}
		value, literal := literalShellWord(assignment.Value)
		return true, literal && flagValueEnabled(value)
	}
	return false, false
}

func shellWordAssignmentName(word *syntax.Word) (string, bool) {
	if word == nil || len(word.Parts) == 0 {
		return "", false
	}
	literal, ok := word.Parts[0].(*syntax.Lit)
	if !ok {
		return "", false
	}
	name, _, assignment := strings.Cut(literal.Value, "=")
	return name, assignment && validName(name)
}

func wordEquals(word *syntax.Word, want string) bool {
	value, literal := literalShellWordExpandableSafe(word)
	return literal && value == want
}

func wordBaseEquals(word *syntax.Word, want string) bool {
	value, literal := literalShellWordExpandableSafe(word)
	return literal && strings.EqualFold(filepath.Base(value), want)
}

func isTrustedEnvWord(word *syntax.Word) bool {
	value, literal := literalShellWordExpandableSafe(word)
	return literal && isTrustedEnvExecutable(value)
}

// isTrustedEnvExecutable keeps the existing bare form for compatibility: it is
// resolved through the operator's inherited PATH, not a path selected from the
// repository command. The two absolute forms are the conventional root-owned
// system binaries on supported Unix hosts and are strictly less redirectable.
// Every other path stays untrusted; in particular, a repository's ./env and a
// user-writable /tmp/env cannot turn an agent-looking argument into a grant.
func isTrustedEnvExecutable(executable string) bool {
	switch executable {
	case "env", "/bin/env", "/usr/bin/env":
		return true
	default:
		return false
	}
}

// isEnvExecutableByBase is the lenient counterpart that matches any spelling
// whose basename is env, mirroring tmux.DetectAgentExecutable's create-time
// detection. It is the env rule for AgentNamespaceForCommand's namespace-only
// callers: those classify operator-controlled input (instance.AgentProgram is
// stored verbatim from opts.Program) purely to look up an account directory,
// never to grant a credential, so the overwrite-by-./env threat the strict set
// closes does not apply.
func isEnvExecutableByBase(executable string) bool {
	return strings.EqualFold(filepath.Base(executable), "env")
}

func literalCommandArgs(words []*syntax.Word) ([]string, bool) {
	args := make([]string, len(words))
	for idx, word := range words {
		value, ok := literalShellWord(word)
		if !ok {
			return nil, false
		}
		args[idx] = value
	}
	return args, true
}

func literalShellWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	var value strings.Builder
	for _, part := range word.Parts {
		if !appendLiteralShellPart(&value, part) {
			return "", false
		}
	}
	return value.String(), true
}

// literalShellWordExpandableSafe is literalShellWord plus the guarantee that
// /bin/sh -c cannot expand the word into different argv: unquoted literal parts
// must carry no glob metacharacters (* ? and a bracket expression that closes),
// no unquoted brace-expansion open ({ — {a,b} and {a..z} split one word into
// several argv entries under bash, the /bin/sh of the supported macOS case),
// and no leading ~ unless tildePrefixNamesAPath proves it a directory prefix —
// the word then keeps its literal ~ spelling, which matches as a path by
// basename and never as a builtin, option, or assignment, and
// withTildeBindingNames makes the walk refuse any command that could rebind
// that directory. Backslash escapes are resolved so an escaped \| still
// reads as | to hazard checks that inspect the resolved string. Glob and brace
// syntax are tracked across part boundaries because quote removal runs before
// pathname expansion — `["|"]` is the bracket `[|]`, and `{-E,CODEX_HOME}`
// spans Lits the same way — while quoted parts outside such an expression
// cannot glob and keep their raw value, matching what strace would receive.
func literalShellWordExpandableSafe(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	var value strings.Builder
	var braces braceExpansionState
	var bracket bracketGlobState
	first := true
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			if !appendUnexpandedLit(&value, part.Value, first, &braces, &bracket) {
				return "", false
			}
		case *syntax.SglQuoted:
			if part.Dollar {
				return "", false
			}
			for i := 0; i < len(part.Value); i++ {
				bracket.feedMember(part.Value[i])
			}
			value.WriteString(part.Value)
		case *syntax.DblQuoted:
			if part.Dollar {
				return "", false
			}
			for _, nested := range part.Parts {
				if bracket.open {
					lit, isLit := nested.(*syntax.Lit)
					if !isLit {
						return "", false
					}
					for i := 0; i < len(lit.Value); i++ {
						bracket.feedMember(lit.Value[i])
					}
					value.WriteString(lit.Value)
					continue
				}
				if !appendLiteralShellPart(&value, nested) {
					return "", false
				}
			}
		default:
			return "", false
		}
		first = false
	}
	bracket.finish(&value)
	return value.String(), true
}

// provableCommandHead reports whether the word in command position resolves to
// a fixed executable: literalShellWordExpandableSafe, which admits a leading ~
// only as a directory prefix ending in a slash. Every other unprovable head
// fails closed: an unquoted glob or brace can expand to a builtin name (u* to
// unset, e{val,} to eval), and a bare tilde form is replaced by HOME, PWD, or
// OLDPWD outright — `PWD=unset; ~+ CODEX_HOME` runs the unset builtin under
// bash (Codex on #4466) — so judging the tail as env's argv cannot model it.
func provableCommandHead(word *syntax.Word) bool {
	if word == nil || len(word.Parts) == 0 {
		return false
	}
	_, ok := literalShellWordExpandableSafe(word)
	return ok
}

// braceExpansionState tracks unquoted '{', '}', ',', and '..' across a word's
// literal parts: an open brace region closed by '}' that contained a separator
// is a brace expansion — bash splits it into several argv entries even in POSIX
// mode. '{a}', '{}', and unclosed '{' stay literal, and quoted parts contribute
// neither braces nor separators ('{a','b}' is literal to bash).
type braceExpansionState struct {
	depth int
	sep   bool
}

// bracketGlobState tracks an unquoted '[' bracket expression across a word's
// parts. Quote removal runs before pathname expansion, so the pattern can span
// quoted and unquoted fragments — `["|"]` is the bracket `[|]` even though the
// member sits in a quoted part a per-literal scan cannot see. Quoted bytes
// therefore count as members while the expression is open, but only an
// unquoted ']' closes it, and only after a real member: a ']' in first
// position (or right after a leading '!'/'^' negation) is itself a member, and
// an unquoted '\' escapes the next member byte. A '[' that never closes is a
// literal character, not a glob.
//
// The state only TRACKS; it consumes no byte from the word's other readers.
// Letting it consume them is the bug #4579 measured — see appendUnexpandedLit.
type bracketGlobState struct {
	open     bool
	negation bool
	members  int
	escape   bool
}

// feedMember counts one byte of a quoted part inside a bracket expression. A
// quoted byte can never close it, but a '!' or '^' in first position is still
// the negation marker — quote removal happens before the pattern is read.
func (b *bracketGlobState) feedMember(c byte) {
	if b.escape {
		// A '\' at the end of the preceding unquoted part quotes this byte.
		b.consumeEscape()
		return
	}
	if b.members == 0 && !b.negation && (c == '!' || c == '^') {
		b.negation = true
		return
	}
	b.members++
}

// feed applies one unescaped, unquoted byte to the bracket state and reports
// whether the byte CLOSED a live expression — which makes the whole word a
// pathname expansion. It writes nothing and swallows nothing: every reader of
// the word (the brace-expansion state, the glob-metacharacter check, the value
// builder) still sees the byte, because a '[' that never closes leaves all of
// them live for the rest of the word.
func (b *bracketGlobState) feed(c byte) (closed bool) {
	if !b.open {
		if c == '[' {
			b.open = true
			b.negation = false
			b.members = 0
		}
		return false
	}
	switch c {
	case ']':
		if b.members > 0 {
			return true
		}
		// First position (also right after the negation marker): a member.
		b.members++
		b.negation = false
	case '!', '^':
		if b.members == 0 && !b.negation {
			b.negation = true
		} else {
			b.members++
		}
	default:
		b.members++
	}
	return false
}

// beginEscape records an unquoted '\'. One escape rule serves the whole word:
// the next byte is quoted, so it is neither brace syntax, nor a glob
// metacharacter, nor bracket syntax — inside a live bracket expression it is an
// ordinary member, and outside one it is ordinary literal text.
func (b *bracketGlobState) beginEscape() {
	b.escape = true
}

// escaped reports whether the previous byte was an unquoted '\'.
func (b *bracketGlobState) escaped() bool {
	return b.escape
}

// consumeEscape closes the escape beginEscape opened, counting the quoted byte
// as a bracket member when an expression is open.
func (b *bracketGlobState) consumeEscape() {
	b.escape = false
	if b.open {
		b.members++
	}
}

// finish flushes a dangling escape at the end of a word: a '\' with no byte
// after it is literal, matching the word-level rule.
func (b *bracketGlobState) finish(value *strings.Builder) {
	if b.escape {
		b.consumeEscape()
		value.WriteByte('\\')
	}
}

func (b *braceExpansionState) feed(c byte, next byte, hasNext bool) (expanded bool, skip bool) {
	switch c {
	case '{':
		b.depth++
	case '}':
		if b.depth > 0 {
			b.depth--
			if b.depth == 0 {
				return b.sep, false
			}
		}
	case ',':
		if b.depth > 0 {
			b.sep = true
		}
	case '.':
		if b.depth > 0 && hasNext && next == '.' {
			b.sep = true
			return false, true
		}
	}
	return false, false
}

// appendUnexpandedLit resolves one unquoted literal part of a word into value,
// reporting whether the part still spells exactly the text it reads as.
//
// The order of the readers is the whole correctness argument, and #4579
// measured what getting it wrong costs. Brace expansion is an EARLIER phase
// than pathname expansion, so it runs whether or not a bracket expression is
// open: bash splits `{unset,a[b}` into `unset` and `a[b` before anything reads
// '[' as a pattern at all. The bracket state used to consume every byte after
// an unclosed '[' — so the ',' and the '}' never reached the brace state, the
// word read as one literal, and `{unset,a[b} CODEX_HOME` passed the account
// guard while bash unset the identity variable.
//
// The glob metacharacters are unconditional for the mirror-image reason: an
// unescaped '*' or '?' seen while a bracket is open refuses whichever way that
// bracket goes. If it closes, the word is a bracket glob and refuses anyway; if
// it never closes, the '[' is an ordinary character and the metacharacter is a
// live wildcard. So the bracket state now only tracks, and every reader sees
// every byte.
func appendUnexpandedLit(
	value *strings.Builder,
	s string,
	wordStart bool,
	braces *braceExpansionState,
	bracket *bracketGlobState,
) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if bracket.escaped() {
			// Quoted by the preceding '\': literal text and, inside a live
			// bracket expression, an ordinary member. Nothing else reads it.
			bracket.consumeEscape()
			value.WriteByte(c)
			continue
		}
		if c == '\\' {
			bracket.beginEscape()
			continue
		}
		hasNext := i+1 < len(s)
		var next byte
		if hasNext {
			next = s[i+1]
		}
		expanded, skip := braces.feed(c, next, hasNext)
		if expanded {
			return false
		}
		if skip {
			// The '..' of a sequence expression: the second '.' carries no
			// further meaning to any reader, but the bracket state still
			// counts it as a member.
			i++
			if bracket.feed('.') {
				return false
			}
			value.WriteByte('.')
		}
		switch c {
		case '*', '?':
			return false
		case '~':
			if wordStart && i == 0 && !tildePrefixNamesAPath(s) {
				return false
			}
		}
		if bracket.feed(c) {
			return false
		}
		value.WriteByte(c)
	}
	return true
}

// unprovableWordCausedRefusal names the first argv word whose unprovability is
// WHY the command refused — so the diagnostic can point the user at the exact
// word to pin. It returns empty when the refusal has a literal cause instead:
// a denied name, a mutating builtin, or a mutating node no word-pinning can
// clear. Naming a dynamic word that is unrelated to the verdict would send the
// user to fix something that cannot make the command pass, so
// `echo "$HOME"; unset CODEX_HOME` keeps the generic message (the cause is the
// literal unset) while `env "$X" codex` names "$X" (pinning it clears the
// refusal).
//
// The test is causal, not positional: a call whose mutation survives pinning
// every unprovable word to an inert literal — or that has no unprovable word at
// all — is a literal cause. An assignment-shaped word keeps its provable name
// when pinned (the denial lives in the name, not the dynamic value), so
// `env CODEX_HOME=$X codex` is likewise a literal-cause refusal.
func unprovableWordCausedRefusal(
	command string,
	names map[string]struct{},
	evaluation *evaluationBudget,
) string {
	// The diagnostic walk draws on the caller's meter, the same one the verdict
	// walk spent: it re-walks the whole program, so giving it a fresh budget
	// would let one validation pay for the advertised bound several times over.
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(command), "")
		if err != nil {
			continue
		}
		var blamed *syntax.Word
		literalCause := false
		syntax.Walk(file, func(node syntax.Node) bool {
			if literalCause {
				return false
			}
			call, isCall := node.(*syntax.CallExpr)
			if !isCall {
				if nodeMutatesAccountEnvironment(node, names, evaluation) {
					literalCause = true
					return false
				}
				return true
			}
			if !callMutatesAccountEnvironment(call, names, evaluation) {
				return true
			}
			pinned, first := pinnedCallArgs(call)
			if first == nil {
				literalCause = true
				return false
			}
			substituted := &syntax.CallExpr{Assigns: call.Assigns, Args: pinned}
			if callMutatesAccountEnvironment(substituted, names, evaluation) {
				literalCause = true
				return false
			}
			if blamed == nil {
				blamed = first
			}
			return true
		})
		if literalCause {
			return ""
		}
		if blamed != nil {
			var sb strings.Builder
			if err := syntax.NewPrinter().Print(&sb, blamed); err != nil {
				return ""
			}
			return sb.String()
		}
	}
	return ""
}

// pinnedCallArgs returns call.Args with every unprovable word replaced by an
// inert literal placeholder, plus the first such word. An assignment-shaped
// word keeps its provable NAME= prefix so a denied name still denies.
func pinnedCallArgs(call *syntax.CallExpr) ([]*syntax.Word, *syntax.Word) {
	var first *syntax.Word
	args := make([]*syntax.Word, len(call.Args))
	copy(args, call.Args)
	for idx, arg := range call.Args {
		if _, safe := literalShellWordExpandableSafe(arg); safe {
			continue
		}
		if first == nil {
			first = arg
		}
		replacement := "AF_UNPROVABLE_WORD"
		if name, assignment := shellWordAssignmentName(arg); assignment {
			replacement = name + "=AF_UNPROVABLE_WORD"
		}
		args[idx] = &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: replacement}}}
	}
	return args, first
}

func appendLiteralShellPart(value *strings.Builder, part syntax.WordPart) bool {
	switch part := part.(type) {
	case *syntax.Lit:
		value.WriteString(part.Value)
		return true
	case *syntax.SglQuoted:
		if part.Dollar {
			return false
		}
		value.WriteString(part.Value)
		return true
	case *syntax.DblQuoted:
		if part.Dollar {
			return false
		}
		for _, nested := range part.Parts {
			if !appendLiteralShellPart(value, nested) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
