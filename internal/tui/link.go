package tui

import (
	"net/url"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// A browser that lists paths can hand them to the terminal instead:
// OSC 8 turns a label into a link the reader opens with cmd- or
// ctrl-click, and a terminal that does not support it prints the label
// unchanged. The target is resolved by the local terminal, so a link
// still works when bffs runs over SSH — and it costs no row, which a
// printed URL would.

// fileURL is the file:// URL of an absolute local path, "" for anything
// else: a relative path has no meaning on the reader's machine, and a
// path recorded on another machine is not a file here.
//
// The path may come from a transcript written elsewhere, so it is
// sanitised first and then escaped by net/url — a label is text, but a
// URL is handed to the terminal to act on.
func fileURL(path string) string {
	path = transcripts.Sanitize(path)
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows path starts with its drive letter
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}

// link renders text as a hyperlink to target, marked so the reader can
// tell it is one. Without a target — or in ASCII mode, where the marker
// has no glyph to spare — it is the text, styled the same way, so the
// two read alike in a terminal that ignores the escape.
func link(st lipgloss.Style, text, target string) string {
	if target == "" {
		return st.Render(text)
	}
	return st.Hyperlink(target).Render(text) + styleFaint.Render(glyph.open)
}

// linkPath renders a path as a link to itself, shortened for display.
func linkPath(st lipgloss.Style, path string) string {
	return link(st, shortPath(path), fileURL(path))
}
