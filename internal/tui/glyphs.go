package tui

// No terminal exposes a "this font renders symbols" signal, so the
// fallback is an opt-in: BFFS_ASCII=1 swaps every symbol bffs draws for
// an ASCII stand-in. Everything that prints a symbol reads it from here,
// so the set is the one place to look when a terminal shows boxes.
type glyphSet struct {
	live, imported, missing string // session states
	mark, pin, here         string // marked rows, pinned memory, the selected root
	ok, bad, unknown        string // verdicts
	sep, ellipsis, arrow    string // separators and pointers
	bullet, quote, fold     string // markdown: list bullet, quote bar, a folded code line
	open                    string // the mark after a hyperlink
	ruleH                   string // markdown: a horizontal rule
	tl, tr, bl, br, h, v    string // box borders
}

var unicodeGlyphs = glyphSet{
	live: "●", imported: "↓", missing: "!",
	mark: "*", pin: "pin", here: "←",
	ok: "✓", bad: "✗", unknown: "–",
	sep: "·", ellipsis: "…", arrow: "→",
	bullet: "•", quote: "│", fold: "↪", open: " ↗",
	ruleH: "─",
	tl:    "┌", tr: "┐", bl: "└", br: "┘", h: "─", v: "│",
}

var asciiGlyphs = glyphSet{
	live: "*", imported: "v", missing: "!",
	mark: "*", pin: "pin", here: "<-",
	ok: "y", bad: "n", unknown: "-",
	sep: "-", ellipsis: "...", arrow: "->",
	bullet: "-", quote: "|", fold: ">", open: "",
	ruleH: "-",
	tl:    "+", tr: "+", bl: "+", br: "+", h: "-", v: "|",
}

// glyph is the set in force. It is resolved once, at start-up, because
// the answer cannot change while the program runs.
var glyph = unicodeGlyphs

// asciiMode reports whether the terminal was told it cannot draw
// symbols. BFFS_ASCII takes any non-empty value other than "0".
func asciiMode() bool {
	v := osEnv("BFFS_ASCII")
	return v != "" && v != "0"
}

// resolveGlyphs picks the set for this run.
func resolveGlyphs() {
	if asciiMode() {
		glyph = asciiGlyphs
		return
	}
	glyph = unicodeGlyphs
}
