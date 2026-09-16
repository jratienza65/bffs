package transcripts

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitize(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "fix the login bug", "fix the login bug"},
		{"unicode kept", "café 😀 日本", "café 😀 日本"},
		{"tab kept", "a\tb", "a\tb"},
		{"newline dropped", "line\nbreak\r\n", "linebreak"},
		{"nul dropped", "a\x00b", "ab"},
		{"del dropped", "a\x7fb", "ab"},
		{"c1 dropped", "a\u0085b\u0086c", "abc"},
		{"c1 apc swallows to st", "a\u009fpayload\u009cb", "ab"},
		{"osc 52 clipboard", "\x1b]52;c;aGVsbG8=\x07next", "next"},
		{"osc 52 st terminated", "title\x1b]52;c;aGVsbG8=\x1b\\ tail", "title tail"},
		{"osc unterminated swallows rest", "\x1b]0;evil title", ""},
		{"csi clear screen", "\x1b[2Jhello", "hello"},
		{"csi cursor home and colours", "\x1b[H\x1b[31mred\x1b[0m done", "red done"},
		{"csi with private and intermediates", "\x1b[?25l\x1b[>4;2mx", "x"},
		{"c1 csi", "\u009b2Jx", "x"},
		{"c1 osc", "\u009d52;c;Zm9v\u009cy", "y"},
		{"esc charset", "\x1b(Bok", "ok"},
		{"esc single", "\x1b7save\x1b8", "save"},
		{"esc at end", "trail\x1b", "trail"},
		{"dcs", "\x1bPq#0;2;0;0;0#0~~\x1b\\z", "z"},
		{"apc", "\x1b_Gf=100;\x1b\\img", "img"},
		{"csi unterminated swallows rest", "a\x1b[38;5;", "a"},
		{"invalid utf8 replaced", "a\xffb", "a\ufffdb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Sanitize(c.in); got != c.want {
				t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSanitizeCap(t *testing.T) {
	in := strings.Repeat("é", 600)
	got := Sanitize(in)
	if n := utf8.RuneCountInString(got); n != sanitizeCap {
		t.Errorf("rune count = %d, want %d", n, sanitizeCap)
	}
	if !strings.HasPrefix(in, got) {
		t.Error("capped output is not a prefix of the input")
	}
	// Control characters do not count towards the cap.
	in = strings.Repeat("\x1b[31m", 600) + "x"
	if got := Sanitize(in); got != "x" {
		t.Errorf("Sanitize(600 escapes + x) = %q, want %q", got, "x")
	}
}

func TestSanitizeIdempotent(t *testing.T) {
	for _, in := range []string{"\x1b]52;c;aGVsbG8=\x07next", "\x1b[2Jhello", "plain", strings.Repeat("😀", 700)} {
		once := Sanitize(in)
		if twice := Sanitize(once); twice != once {
			t.Errorf("Sanitize not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}
