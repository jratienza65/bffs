package rehome

import (
	"strings"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// VerifyCommand renders the line every import, copy and rehome ends with
// (plan §9.12): "cd <newCwd> && claude --resume <sid>". A non-empty account
// (a full-isolation root or an explicit --account) puts
// "BFFS_ACCOUNT=<account> " on the claude word itself — "cd X && A=1 claude"
// — never before the cd: in a POSIX shell a leading assignment binds to the
// first simple command only, so "A=1 cd X && claude" would launch claude
// without it. An empty newCwd drops the "cd" half. newCwd is single-quoted
// only when it needs quoting for a POSIX shell. Every input passes
// transcripts.Sanitize — a cwd or title from a bundle must not carry escape
// sequences into the terminal.
func VerifyCommand(newCwd, sid, account string) string {
	cwd := transcripts.Sanitize(newCwd)
	sid = transcripts.Sanitize(sid)
	account = transcripts.Sanitize(account)

	var b strings.Builder
	if cwd != "" {
		b.WriteString("cd ")
		b.WriteString(ShellQuote(cwd))
		b.WriteString(" && ")
	}
	if account != "" {
		b.WriteString("BFFS_ACCOUNT=")
		b.WriteString(ShellQuote(account))
		b.WriteByte(' ')
	}
	b.WriteString("claude --resume ")
	b.WriteString(ShellQuote(sid))
	return b.String()
}

// ShellQuote returns s as a single POSIX shell word: unchanged when every
// byte is in the safe set, else single-quoted with embedded quotes escaped.
// It is what every "check it" line renders a path or id with.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isShellSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == '/', c == ':', c == '@', c == '%', c == '+', c == '=', c == ',':
		default:
			return false
		}
	}
	return true
}
