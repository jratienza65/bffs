package rehome

import (
	"strings"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// VerifyCommand renders the line every import, copy and rehome ends with
// (plan §9.12): "cd <newCwd> && claude --resume <sid>", prefixed with
// "BFFS_ACCOUNT=<account> " when account is non-empty (a full-isolation
// root or an explicit --account). newCwd is single-quoted only when it
// needs quoting for a POSIX shell; an empty newCwd drops the "cd" half.
// Every input passes transcripts.Sanitize — a cwd or title from a bundle
// must not carry escape sequences into the terminal.
func VerifyCommand(newCwd, sid, account string) string {
	cwd := transcripts.Sanitize(newCwd)
	sid = transcripts.Sanitize(sid)
	account = transcripts.Sanitize(account)

	var b strings.Builder
	if account != "" {
		b.WriteString("BFFS_ACCOUNT=")
		b.WriteString(ShellQuote(account))
		b.WriteByte(' ')
	}
	if cwd != "" {
		b.WriteString("cd ")
		b.WriteString(ShellQuote(cwd))
		b.WriteString(" && ")
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
