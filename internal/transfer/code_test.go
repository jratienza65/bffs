package transfer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestGenerateCodeShape(t *testing.T) {
	for range 100_000 {
		c, err := GenerateCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.s) != 8 {
			t.Fatalf("code %q: want 8 symbols", c.s)
		}
		if strings.ContainsAny(c.s, "ILOUilou") {
			t.Fatalf("code %q contains an excluded symbol", c.s)
		}
		for i := 0; i < len(c.s); i++ {
			if strings.IndexByte(alphabet, c.s[i]) < 0 {
				t.Fatalf("code %q: symbol %q outside the alphabet", c.s, c.s[i])
			}
		}
		d := c.Display()
		if len(d) != 9 || d[4] != '-' {
			t.Fatalf("Display() = %q, want XXXX-XXXX", d)
		}
	}
}

func TestGenerateCodeVaries(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		c, err := GenerateCode()
		if err != nil {
			t.Fatal(err)
		}
		seen[c.s] = true
	}
	if len(seen) < 45 {
		t.Fatalf("50 codes produced only %d distinct values", len(seen))
	}
}

func TestParseCodeRoundTripAndFolding(t *testing.T) {
	c, err := GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCode(c.Display())
	if err != nil {
		t.Fatalf("ParseCode(Display) error: %v", err)
	}
	if back != c {
		t.Fatalf("round trip: got %q, want %q", back.s, c.s)
	}

	cases := []struct {
		name, in, want string
	}{
		{"canonical", "7K3QM9XD", "7K3QM9XD"},
		{"display", "7K3Q-M9XD", "7K3QM9XD"},
		{"lowercase", "7k3q-m9xd", "7K3QM9XD"},
		{"spaces", " 7K3Q M9XD ", "7K3QM9XD"},
		{"tabs and newline", "7K3Q\tM9XD\n", "7K3QM9XD"},
		{"O folds to 0", "OK3Q-M9XD", "0K3QM9XD"},
		{"o folds to 0", "ok3q-m9xd", "0K3QM9XD"},
		{"I folds to 1", "IK3Q-M9XD", "1K3QM9XD"},
		{"L folds to 1", "LK3Q-M9XD", "1K3QM9XD"},
		{"l folds to 1", "lk3q-m9xd", "1K3QM9XD"},
		{"many dashes", "7-K-3-Q-M-9-X-D", "7K3QM9XD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCode(tc.in)
			if err != nil {
				t.Fatalf("ParseCode(%q) error: %v", tc.in, err)
			}
			if got.s != tc.want {
				t.Fatalf("ParseCode(%q) = %q, want %q", tc.in, got.s, tc.want)
			}
		})
	}
}

func TestParseCodeRejects(t *testing.T) {
	bad := []string{
		"",
		"7K3Q-M9X",      // 7 symbols
		"7K3Q-M9XDA",    // 9 symbols
		"7K3Q-M9XU",     // U is not in the alphabet
		"7K3Q-M9X!",     // punctuation
		"7K3Q_M9XD",     // underscore is not a separator
		"7K3Q-M9XÉ",     // non-ASCII
		"7K3Q-M9XD-7K3", // too long
		"7K3Q-M9X\x00D", // NUL
		"७K3Q-M9XD",     // Devanagari digit
	}
	for _, in := range bad {
		c, err := ParseCode(in)
		if !errors.Is(err, ErrInvalidCode) {
			t.Errorf("ParseCode(%q) = (%q, %v), want ErrInvalidCode", in, c.s, err)
		}
		if c != (Code{}) {
			t.Errorf("ParseCode(%q) returned a non-zero code on error", in)
		}
	}
	if got := ErrInvalidCode.Error(); got != "invalid pairing code: use the 8 characters shown on the other machine" {
		t.Fatalf("ErrInvalidCode text = %q", got)
	}
}

func TestCodeHasNoStringMethod(t *testing.T) {
	c, err := GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	var v any = c
	if _, ok := v.(fmt.Stringer); ok {
		t.Fatal("Code must not implement fmt.Stringer")
	}
	// %v on a struct with an unexported string field prints the field, so
	// nothing in the package may ever format a Code; this pins the
	// Display-only contract for the zero value at least.
	if (Code{}).Display() != "" {
		t.Fatal("zero Code must display as empty")
	}
}

func TestSentinelErrorsNeverContainACode(t *testing.T) {
	c, err := GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []error{ErrTooManyAttempts, ErrExpired, ErrRejected, ErrBadCode, ErrNotLAN, ErrPeerNotOwner, ErrInvalidCode} {
		s := fmt.Sprint(e)
		if s == "" {
			t.Errorf("empty sentinel text")
		}
		if strings.Contains(s, c.s) || strings.Contains(s, c.Display()) {
			t.Errorf("sentinel %q contains the code", s)
		}
		if !errors.Is(withDetail(e, "detail"), e) {
			t.Errorf("withDetail(%v) does not match with errors.Is", e)
		}
	}
	// Every sentinel wrapped through withDetail still matches.
	d := withDetail(ErrBadCode, "the other machine rejected the code (2 attempts left there)")
	if !errors.Is(d, ErrBadCode) || d.Error() != "the other machine rejected the code (2 attempts left there)" {
		t.Fatalf("detailErr: Is=%v text=%q", errors.Is(d, ErrBadCode), d.Error())
	}
	if errors.Is(d, ErrExpired) {
		t.Fatal("detailErr matched an unrelated sentinel")
	}
}
