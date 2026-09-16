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
	crumb, times, dash      string // the header's path separator, a size, a range
	emdash                  string // the break in a sentence
	up, down, left          string // the arrows in a hint or a table
	quoteL, quoteR          string // around a filter's text
	barFull, barEmpty       string // the bytes bar
	tool, result            string // the transcript's tool call and its result
	bullet, quote, fold     string // markdown: list bullet, quote bar, a folded code line
	open                    string // the mark after a hyperlink
	ruleH                   string // markdown: a horizontal rule
	tl, tr, bl, br, h, v    string // box borders
	lt, rt                  string // the tee where two panels meet
}

var unicodeGlyphs = glyphSet{
	live: "●", imported: "↓", missing: "!",
	mark: "*", pin: "pin", here: "←",
	ok: "✓", bad: "✗", unknown: "–",
	sep: "·", ellipsis: "…", arrow: "→",
	crumb: "›", times: "×", dash: "–", emdash: "—",
	up: "↑", down: "↓", left: "←",
	quoteL: "“", quoteR: "”",
	barFull: "█", barEmpty: "░",
	tool: "⚙", result: "⇠",
	bullet: "•", quote: "│", fold: "↪", open: " ↗",
	ruleH: "─",
	tl:    "┌", tr: "┐", bl: "└", br: "┘", h: "─", v: "│", lt: "├", rt: "┤",
}

var asciiGlyphs = glyphSet{
	live: "*", imported: "v", missing: "!",
	mark: "*", pin: "pin", here: "<-",
	ok: "y", bad: "n", unknown: "-",
	sep: "-", ellipsis: "...", arrow: "->",
	crumb: ">", times: "x", dash: "-", emdash: "--",
	up: "^", down: "v", left: "<-",
	quoteL: "\"", quoteR: "\"",
	barFull: "#", barEmpty: ".",
	tool: "*", result: "<-",
	bullet: "-", quote: "|", fold: ">", open: "",
	ruleH: "-",
	tl:    "+", tr: "+", bl: "+", br: "+", h: "-", v: "|", lt: "+", rt: "+",
}

// glyph is the set in force. It is resolved once, at start-up, because
// the answer cannot change while the program runs. sepDot is the
// separator with the spaces every line puts around it, which is how it
// is written in the middle of a sentence.
var (
	glyph  = unicodeGlyphs
	sepDot = " " + glyph.sep + " "
)

// asciiMode reports whether the terminal was told it cannot draw
// symbols. BFFS_ASCII takes any non-empty value other than "0".
func asciiMode() bool {
	v := osEnv("BFFS_ASCII")
	return v != "" && v != "0"
}

// resolveGlyphs picks the set for this run.
func resolveGlyphs() {
	glyph = unicodeGlyphs
	if asciiMode() {
		glyph = asciiGlyphs
	}
	sepDot = " " + glyph.sep + " "
	// The few strings built once, rather than per render, are rebuilt
	// here too — a package-level value would otherwise keep the
	// symbols BFFS_ASCII asked it to drop.
	cancelling = "cancelling" + glyph.ellipsis
	reservedHint = "(not available here; d never deletes " + glyph.emdash + " use bffs sessions rm)"
	quitPrompt = "an operation is running " + glyph.emdash + " quit anyway? it is cancelled first [y/N]"
	keys = newKeyMap()
}
