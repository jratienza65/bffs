package transfer

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func FuzzParseCode(f *testing.F) {
	for _, s := range []string{"7K3Q-M9XD", "7k3q m9xd", "OIL0-1234", "", "-", "7K3Q-M9XU", "७K3Q-M9XD", "7K3Q-M9XD-7K3Q"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := ParseCode(s)
		if err != nil {
			if c != (Code{}) {
				t.Fatalf("ParseCode(%q): non-zero code with error", s)
			}
			return
		}
		d := c.Display()
		if (len(d) != 9 && len(d) != 14) || d[4] != '-' || (len(d) == 14 && d[9] != '-') {
			t.Fatalf("ParseCode(%q).Display() = %q", s, d)
		}
		for i := 0; i < len(c.s); i++ {
			if bytes.IndexByte([]byte(alphabet), c.s[i]) < 0 {
				t.Fatalf("ParseCode(%q) produced %q outside the alphabet", s, c.s)
			}
		}
		back, err := ParseCode(d)
		if err != nil || back != c {
			t.Fatalf("ParseCode(%q) does not round-trip through Display: %v", s, err)
		}
	})
}

func FuzzReadFrame(f *testing.F) {
	hdr := func(n uint32) []byte {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], n)
		return b[:]
	}
	f.Add(frameBytes([]byte(`{"t":"ping"}`)))
	f.Add(append(hdr(1<<31), make([]byte, 16)...))
	f.Add(hdr(5))
	f.Add([]byte{0, 0})
	f.Add(frameBytes(nil))
	f.Add(frameBytes([]byte(`{"t":"hello","v":1,"formats":[1],"proof":"AA=="}`)))
	f.Fuzz(func(t *testing.T, b []byte) {
		cr := &countingReader{r: bytes.NewReader(b)}
		p, err := readFrame(cr, maxControlFrame)
		if err != nil {
			if len(b) >= 4 && binary.BigEndian.Uint32(b) > maxControlFrame && cr.n != 4 {
				t.Fatalf("oversize header: read %d bytes, want exactly the header", cr.n)
			}
			return
		}
		if len(p) > maxControlFrame {
			t.Fatalf("frame of %d bytes above the cap accepted", len(p))
		}
		if cr.n != 4+len(p) {
			t.Fatalf("consumed %d bytes for a %d-byte frame", cr.n, len(p))
		}
		// Whatever the payload, decoding must not panic.
		_, _ = readMsg(bytes.NewReader(b))
	})
}
