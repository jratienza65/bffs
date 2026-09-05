package transcripts

import "strings"

// sanitizeCap is the longest string Sanitize returns, in runes. Titles,
// paths and peer names never need more, and a cap keeps a hostile manifest
// from flooding a table.
const sanitizeCap = 512

// Sanitize makes a string safe to print in a terminal or a table cell:
// C0 and C1 control characters are dropped (tab survives; newlines do not —
// rendered strings are single-line), ESC sequences are removed whole —
// CSI (ESC [ … final byte 0x40–0x7E), OSC (ESC ] … BEL or ESC \), the other
// string types (DCS, SOS, PM, APC, up to ST) and short ESC + intermediates +
// final escapes — including their C1 single-byte forms, and the result is
// cut at sanitizeCap runes. Every peer-, manifest- or transcript-derived
// string passes through here before it is rendered, so an OSC 52 clipboard
// write or a screen-clearing CSI inside a session title cannot reach the
// user's terminal.
func Sanitize(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(min(len(s), sanitizeCap))
	n := 0
	for i := 0; i < len(rs) && n < sanitizeCap; i++ {
		r := rs[i]
		switch {
		case r == 0x1B: // ESC
			i = skipEscape(rs, i)
		case r == 0x9B: // C1 CSI
			i = skipCSI(rs, i)
		case r == 0x9D, r == 0x90, r == 0x98, r == 0x9E, r == 0x9F: // C1 OSC, DCS, SOS, PM, APC
			i = skipString(rs, i)
		case r == '\t':
			b.WriteRune(r)
			n++
		case r < 0x20, r == 0x7F, r >= 0x80 && r <= 0x9F:
			// C0 control, DEL or C1 control: dropped.
		default:
			b.WriteRune(r)
			n++
		}
	}
	return b.String()
}

// skipEscape is called with rs[i] == ESC and returns the index of the last
// rune of the escape sequence that starts there. An unterminated sequence
// swallows the rest of the string — safer than letting its tail through.
func skipEscape(rs []rune, i int) int {
	next := i + 1
	if next >= len(rs) {
		return len(rs) - 1
	}
	switch rs[next] {
	case '[':
		return skipCSI(rs, next)
	case ']', 'P', 'X', '^', '_':
		return skipString(rs, next)
	}
	// ESC, zero or more intermediates (0x20–0x2F), one final (0x30–0x7E):
	// covers ESC ( B, ESC 7, ESC = and friends.
	j := next
	for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x2F {
		j++
	}
	if j >= len(rs) {
		return len(rs) - 1
	}
	return j
}

// skipCSI is called with rs[i] the CSI introducer ('[' after ESC, or the C1
// byte) and returns the index of the final byte (0x40–0x7E).
func skipCSI(rs []rune, i int) int {
	for j := i + 1; j < len(rs); j++ {
		if rs[j] >= 0x40 && rs[j] <= 0x7E {
			return j
		}
	}
	return len(rs) - 1
}

// skipString is called with rs[i] the introducer of an OSC/DCS/SOS/PM/APC
// string and returns the index of its terminator: BEL, C1 ST (0x9C) or the
// second rune of ESC \.
func skipString(rs []rune, i int) int {
	for j := i + 1; j < len(rs); j++ {
		switch rs[j] {
		case 0x07, 0x9C:
			return j
		case 0x1B:
			if j+1 < len(rs) && rs[j+1] == '\\' {
				return j + 1
			}
		}
	}
	return len(rs) - 1
}
