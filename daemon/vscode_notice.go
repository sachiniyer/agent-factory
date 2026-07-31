package daemon

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
)

// writeVSCodeNoticePage renders a human-readable notice INTO the pane: the web
// UI iframes this route, so whatever this returns is what the user sees where the
// editor would be.
//
// It answers 503 rather than 200 because the editor genuinely is not being
// served: a 200 would tell a scripted client the editor is up and hand it a
// message. Browsers render an error status's body in an iframe, so the pane still
// shows this text — the status is honest without costing the UX. When retry is
// set the page re-requests itself, so a pane opened while the editor is still
// starting resolves into the editor on its own.
func writeVSCodeNoticePage(w http.ResponseWriter, message string) {
	writeVSCodeNoticePageRetry(w, message, false)
}

func writeVSCodeNoticePageRetry(w http.ResponseWriter, message string, retry bool) {
	writeTabNoticePage(w, "VS Code", message, retry)
}

// writeTabNoticePage is writeVSCodeNoticePage's kind-agnostic form: the same
// notice, under a caller-chosen title. A web tab frames the SAME route as an
// editor, so a notice that can be reached before the tab's kind is known (the
// warm-up gate, #1878) must not announce itself as VS Code to someone previewing
// their dev server. title is escaped like the message — no caller can inject
// markup through it.
func writeTabNoticePage(w http.ResponseWriter, title, message string, retry bool) {
	refresh := ""
	if retry {
		refresh = `<meta http-equiv="refresh" content="2">`
	}
	body := fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><title>%s</title>%s
<style>
 html,body{margin:0;height:100%%}
 body{display:flex;align-items:center;justify-content:center;
      font:14px/1.6 ui-sans-serif,system-ui,sans-serif;
      background:#1f1f1f;color:#cccccc;padding:2rem;text-align:center}
 .m{max-width:46rem}
 a{color:#4daafc}
</style></head>
<body><div class="m">%s</div></body></html>`, html.EscapeString(title), refresh, htmlLinkify(message))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never cache a notice: the very next request may be the running editor.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, body)
}

// htmlLinkify escapes message for HTML and turns a bare https:// URL in it into
// a clickable link, so the install hint's URL is actionable from the pane. The
// escape happens FIRST and the anchor is built from the escaped text, so no part
// of message can inject markup.
func htmlLinkify(message string) string {
	escaped := html.EscapeString(message)
	start := strings.Index(escaped, "https://")
	if start < 0 {
		return escaped
	}
	end := start
	for end < len(escaped) && !strings.ContainsRune(" \t\n<)", rune(escaped[end])) {
		end++
	}
	url := escaped[start:end]
	return escaped[:start] + `<a href="` + url + `" target="_blank" rel="noopener noreferrer">` + url + `</a>` + escaped[end:]
}
