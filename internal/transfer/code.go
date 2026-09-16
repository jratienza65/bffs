package transfer

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// alphabet is Crockford base32: 32 symbols, no I, L, O or U.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	// ShortCodeSymbols is the default code length: 8 × 5 bits = 40 bits.
	ShortCodeSymbols = 8
	// LongCodeSymbols is the --long-code length: 12 × 5 bits = 60 bits.
	LongCodeSymbols = 12

	codeSymbols = ShortCodeSymbols // the length GenerateCode uses
	codeGroup   = 4                // Display groups symbols by four
)

// ErrInvalidCode is returned by ParseCode for anything that is not eight or
// twelve Crockford base32 symbols (after folding).
var ErrInvalidCode = errors.New("invalid pairing code: use the 8 (or 12) characters shown on the other machine")

// Code is a pairing code: eight or twelve Crockford base32 symbols in
// canonical form. It has no String method on purpose — the only way to
// render it is Display, and nothing in this package ever puts it in an
// error, an Event or the UI writer.
type Code struct {
	s string
}

// GenerateCode draws 40 bits from crypto/rand and encodes them as eight
// symbols of the Crockford alphabet: GenerateCodeN(ShortCodeSymbols).
func GenerateCode() (Code, error) {
	return GenerateCodeN(codeSymbols)
}

// GenerateCodeN draws 5×symbols bits from crypto/rand and encodes them as
// that many symbols of the Crockford alphabet. symbols must be
// ShortCodeSymbols (40 bits) or LongCodeSymbols (60 bits); the wire
// protocol is the same for both — the code never crosses the wire, only a
// key derived from it does, and the KDF takes the canonical bytes whatever
// their length.
func GenerateCodeN(symbols int) (Code, error) {
	if symbols != ShortCodeSymbols && symbols != LongCodeSymbols {
		return Code{}, fmt.Errorf("generating pairing code: %d symbols (want %d or %d)", symbols, ShortCodeSymbols, LongCodeSymbols)
	}
	b := make([]byte, (symbols*5+7)/8) // 5 or 8 bytes: at least 5×symbols bits
	if _, err := rand.Read(b); err != nil {
		return Code{}, fmt.Errorf("generating pairing code: %w", err)
	}
	out := make([]byte, symbols)
	// Consume the random bytes five bits at a time, most significant first;
	// the last few bits of the final byte may go unused.
	var acc uint64
	nbits := 0
	i := 0
	for _, x := range b {
		acc = acc<<8 | uint64(x)
		nbits += 8
		for nbits >= 5 && i < symbols {
			nbits -= 5
			out[i] = alphabet[(acc>>uint(nbits))&31]
			acc &= 1<<uint(nbits) - 1
			i++
		}
		if i == symbols {
			break
		}
	}
	return Code{s: string(out)}, nil
}

// ParseCode accepts what a human typed: any case, "-" and whitespace ignored,
// O folded to 0, I and L folded to 1. Anything else, or a length other than
// ShortCodeSymbols or LongCodeSymbols, yields ErrInvalidCode.
func ParseCode(s string) (Code, error) {
	var buf [LongCodeSymbols]byte
	n := 0
	for _, r := range s {
		if r == '-' || unicode.IsSpace(r) {
			continue
		}
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		switch r {
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if r > 'Z' || strings.IndexByte(alphabet, byte(r)) < 0 {
			return Code{}, ErrInvalidCode
		}
		if n == LongCodeSymbols {
			return Code{}, ErrInvalidCode
		}
		buf[n] = byte(r)
		n++
	}
	if n != ShortCodeSymbols && n != LongCodeSymbols {
		return Code{}, ErrInvalidCode
	}
	return Code{s: string(buf[:n])}, nil
}

// Display renders the code for a screen in groups of four: "XXXX-XXXX" or
// "XXXX-XXXX-XXXX". The zero Code renders as an empty string.
func (c Code) Display() string {
	if c.s == "" {
		return ""
	}
	var sb strings.Builder
	for i := 0; i < len(c.s); i += codeGroup {
		if i > 0 {
			sb.WriteByte('-')
		}
		sb.WriteString(c.s[i:min(i+codeGroup, len(c.s))])
	}
	return sb.String()
}

// isZero reports whether c was never generated or parsed.
func (c Code) isZero() bool { return c.s == "" }
