package transfer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGenerateCodeNShape(t *testing.T) {
	for range 20_000 {
		c, err := GenerateCodeN(LongCodeSymbols)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.s) != 12 {
			t.Fatalf("code %q: want 12 symbols", c.s)
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
		if len(d) != 14 || d[4] != '-' || d[9] != '-' || strings.Count(d, "-") != 2 {
			t.Fatalf("Display() = %q, want XXXX-XXXX-XXXX", d)
		}
		back, err := ParseCode(d)
		if err != nil || back != c {
			t.Fatalf("round trip of %q: %q, %v", d, back.s, err)
		}
	}
	short, err := GenerateCodeN(ShortCodeSymbols)
	if err != nil || len(short.s) != 8 || len(short.Display()) != 9 {
		t.Fatalf("GenerateCodeN(8) = %q, %v", short.Display(), err)
	}
	for _, n := range []int{0, 7, 9, 10, 16} {
		if _, err := GenerateCodeN(n); err == nil {
			t.Errorf("GenerateCodeN(%d) accepted", n)
		}
	}
}

func TestGenerateCodeNUsesAllSymbols(t *testing.T) {
	// Every position of a long code must vary: a bit-packing slip would
	// pin the last symbols to a constant.
	seen := make([]map[byte]bool, LongCodeSymbols)
	for i := range seen {
		seen[i] = map[byte]bool{}
	}
	for range 2_000 {
		c, err := GenerateCodeN(LongCodeSymbols)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(c.s); i++ {
			seen[i][c.s[i]] = true
		}
	}
	for i, m := range seen {
		if len(m) < 20 {
			t.Errorf("position %d took only %d distinct symbols in 2000 codes", i, len(m))
		}
	}
}

func TestParseCodeLongFoldingAndLengths(t *testing.T) {
	c, err := ParseCode(" 7k3q-m9xd-2pnw ")
	if err != nil || c.s != "7K3QM9XD2PNW" {
		t.Fatalf("ParseCode long = %q, %v", c.s, err)
	}
	if c.Display() != "7K3Q-M9XD-2PNW" {
		t.Errorf("Display = %q", c.Display())
	}
	folded, err := ParseCode("olio-olio-olio")
	if err != nil || folded.s != "0110"+"0110"+"0110" {
		t.Fatalf("folding = %q, %v", folded.s, err)
	}
	for _, bad := range []string{"7K3QM9XD2", "7K3QM9XD2P", "7K3QM9XD2PN", "7K3QM9XD2PNWX", "7K3Q-M9XD-2PNW-7K3Q"} {
		if _, err := ParseCode(bad); !errors.Is(err, ErrInvalidCode) {
			t.Errorf("ParseCode(%q) = %v, want ErrInvalidCode", bad, err)
		}
	}
}

func TestLoopbackLongCode(t *testing.T) {
	fastKDF(t)
	code, err := GenerateCodeN(LongCodeSymbols)
	if err != nil {
		t.Fatal(err)
	}
	body := testBody(256 << 10)
	var srec, crec recorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startServe(t, ctx, baseServe(t, code, body, &srec))

	// The client types the code as a human would: lower case, spaces.
	typed, err := ParseCode(strings.ToLower(strings.ReplaceAll(code.Display(), "-", " ")))
	if err != nil || typed != code {
		t.Fatalf("typed code: %q, %v", typed.s, err)
	}
	var got bytes.Buffer
	d, err := Fetch(context.Background(), baseFetch(addr, typed, &got, &crec))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !d.OK || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("Fetch done = %+v, %d bytes", d, got.Len())
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil || !r.res.Done.OK || r.res.Attempts != 1 {
		t.Fatalf("Serve = %+v, %v", r.res, r.err)
	}
	assertNoCode(t, code, "events", srec.text(), crec.text())

	// A short code against a long one is just a wrong code.
	short := mustCode(t)
	var srec2, crec2 recorder
	addr2, out2 := startServe(t, ctx, baseServe(t, code, body, &srec2))
	if _, err := Fetch(context.Background(), baseFetch(addr2, short, &got, &crec2)); !errors.Is(err, ErrBadCode) {
		t.Fatalf("short code against a long one: %v, want ErrBadCode", err)
	}
	cancel()
	waitServe(t, out2, 10*time.Second)
}
