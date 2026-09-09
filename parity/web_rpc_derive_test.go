package parity

import (
	"regexp"
	"strings"
)

// webRPCCall is one statically resolved call through the web client's af()
// chokepoint. argStart points just after the method argument's comma so the
// field-coverage pass can inspect the same call the verb inventory derived.
type webRPCCall struct {
	method   string
	callPos  int
	argStart int
}

// webCallRe accepts a literal method or a plain identifier. Identifiers are
// resolved only from module-level const declarations below; a dynamic method
// cannot be inventoried honestly and is reported rather than dropped.
var webCallRe = regexp.MustCompile(`(?s)\baf(?:<[^>]*>)?\(\s*(?:"([A-Za-z0-9_]+)"|'([A-Za-z0-9_]+)'|([A-Za-z_$][A-Za-z0-9_$]*))\s*,`)

// Module constants are unindented in the TypeScript sources. Anchoring at the
// start of a line deliberately excludes function-local const declarations: a
// method shared as module protocol metadata is stable, while a local/dynamic
// alias should make the audit fail closed.
var webModuleStringConstRe = regexp.MustCompile(`(?m)^(?:export[ \t]+)?const[ \t]+([A-Za-z_$][A-Za-z0-9_$]*)[ \t]*(?::[^=\n]+)?=[ \t]*(?:"([A-Za-z0-9_]+)"|'([A-Za-z0-9_]+)')[ \t]*;`)

func webModuleStringConsts(src string) map[string]string {
	out := map[string]string{}
	for _, match := range webModuleStringConstRe.FindAllStringSubmatch(src, -1) {
		value := match[2]
		if value == "" {
			value = match[3]
		}
		out[match[1]] = value
	}
	return out
}

func webRPCCalls(src string) (calls []webRPCCall, unresolved []string) {
	consts := webModuleStringConsts(src)
	for _, match := range webCallRe.FindAllStringSubmatchIndex(src, -1) {
		// Ignore the af() helper's own declaration. Any other identifier first
		// argument must resolve to a module string constant or fail the audit.
		prefix := strings.TrimSpace(src[max(0, match[0]-32):match[0]])
		if strings.HasSuffix(prefix, "function") {
			continue
		}

		method := submatch(src, match[2], match[3])
		if method == "" {
			method = submatch(src, match[4], match[5])
		}
		if method == "" {
			identifier := submatch(src, match[6], match[7])
			method = consts[identifier]
			if method == "" {
				unresolved = append(unresolved, identifier)
				continue
			}
		}
		calls = append(calls, webRPCCall{method: method, callPos: match[0], argStart: match[1]})
	}
	return calls, unresolved
}

func submatch(src string, start, end int) string {
	if start < 0 || end < 0 {
		return ""
	}
	return src[start:end]
}
