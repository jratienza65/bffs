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

const codeSymbols = 8 // 8 × 5 bits = 40 bits

// ErrInvalidCode is returned by ParseCode for anything that is not eight
// Crockford base32 symbols (after folding).
var ErrInvalidCode = errors.New("invalid pairing code: use the 8 characters shown on the other machine")

// Code is a pairing code: eight Crockford base32 symbols in canonical form.
// It has no String method on purpose — the only way to render it is
// Display, and nothing in this package ever puts it in an error, an Event or
// the UI writer.
type Code struct {
	s string
}

// GenerateCode draws 40 bits from crypto/rand and encodes them as eight
// symbols of the Crockford alphabet.
func GenerateCode() (Code, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Code{}, fmt.Errorf("generating pairing code: %w", err)
	}
	var acc uint64
	for _, x := range b {
		acc = acc<<8 | uint64(x)
	}
	var out [codeSymbols]byte
	for i := codeSymbols - 1; i >= 0; i-- {
		out[i] = alphabet[acc&31]
		acc >>= 5
	}
	return Code{s: string(out[:])}, nil
}

// ParseCode accepts what a human typed: any case, "-" and whitespace ignored,
// O folded to 0, I and L folded to 1. Anything else, or a wrong length,
// yields ErrInvalidCode.
func ParseCode(s string) (Code, error) {
	var buf [codeSymbols]byte
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
		if n == codeSymbols {
			return Code{}, ErrInvalidCode
		}
		buf[n] = byte(r)
		n++
	}
	if n != codeSymbols {
		return Code{}, ErrInvalidCode
	}
	return Code{s: string(buf[:])}, nil
}

// Display renders the code for a screen: "XXXX-XXXX". The zero Code renders
// as an empty string.
func (c Code) Display() string {
	if c.s == "" {
		return ""
	}
	return c.s[:4] + "-" + c.s[4:]
}

// isZero reports whether c was never generated or parsed.
func (c Code) isZero() bool { return c.s == "" }
