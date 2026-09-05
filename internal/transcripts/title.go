package transcripts

import "strings"

// Title sources, in precedence order.
const (
	TitleSourceCustom      = "custom"       // a /rename or custom-title record
	TitleSourceAI          = "ai"           // Claude's generated ai-title
	TitleSourceLastPrompt  = "last-prompt"  // the last-prompt record's text
	TitleSourceSummary     = "summary"      // a summary record's text
	TitleSourceFirstPrompt = "first-prompt" // the first human prompt in the head window (or the first slash command)
	TitleSourceHistory     = "history"      // the first history.jsonl line for the session
)

// Title picks the name a session is shown under, the way Claude's picker
// does: the custom title, else the AI title, else the last prompt, else
// the summary, else the first prompt from the head window, else the first
// prompt recorded in history.jsonl (hist may be nil; Claude has no such
// tier). Every candidate is reduced to its
// first line, trimmed, capped at promptCap runes and passed through
// Sanitize; the first non-empty one wins. source is one of the
// TitleSource constants, or "" with an empty title.
func Title(h Head, t Tail, hist HistoryIndex) (title, source string) {
	candidates := []struct{ text, source string }{
		{t.CustomTitle, TitleSourceCustom},
		{t.AITitle, TitleSourceAI},
		{t.LastPrompt, TitleSourceLastPrompt},
		{t.Summary, TitleSourceSummary},
		{h.FirstPrompt, TitleSourceFirstPrompt},
	}
	if h.SessionID != "" && hist != nil {
		candidates = append(candidates, struct{ text, source string }{hist[h.SessionID], TitleSourceHistory})
	}
	for _, c := range candidates {
		if s := cleanTitle(c.text); s != "" {
			return s, c.source
		}
	}
	return "", ""
}

// cleanTitle is the normalisation every title candidate goes through.
func cleanTitle(s string) string {
	return strings.TrimSpace(Sanitize(firstLine(s, promptCap)))
}
