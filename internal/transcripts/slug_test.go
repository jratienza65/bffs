package transcripts

import (
	"errors"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ascii path", "/Users/jonas/build/projects/bffs", "-Users-jonas-build-projects-bffs"},
		{"windows path", `C:\Users\jonas\proj`, "C--Users-jonas-proj"},
		{"underscore and dot", "/tmp/my_proj.v2", "-tmp-my-proj-v2"},
		{"bmp non-ascii is one unit", "/tmp/café", "-tmp-caf-"},
		{"decomposed accent is two units", "/tmp/cafe\u0301", "-tmp-cafe-"},
		{"astral char is two units", "/x/😀", "-x---"},
		{"cjk", "/p/日本", "-p---"},
		{"exactly 200", "/" + strings.Repeat("a", 199), "-" + strings.Repeat("a", 199)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Slug(c.in)
			if err != nil {
				t.Fatalf("Slug(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
			}
			if len(got) != len(c.want) {
				t.Errorf("len = %d, want %d", len(got), len(c.want))
			}
		})
	}
}

func TestSlugTooLong(t *testing.T) {
	in := "/" + strings.Repeat("a", 200) // 201 units
	got, err := Slug(in)
	if !errors.Is(err, ErrSlugTooLong) {
		t.Fatalf("Slug(201 chars) err = %v, want ErrSlugTooLong", err)
	}
	if got != "" {
		t.Errorf("Slug(201 chars) = %q, want empty on error", got)
	}
	// Astral characters count twice: 100 of them plus the slash is 201.
	in = "/" + strings.Repeat("😀", 100)
	if _, err := Slug(in); !errors.Is(err, ErrSlugTooLong) {
		t.Errorf("Slug(100 astral) err = %v, want ErrSlugTooLong", err)
	}
	if _, err := Slug("/" + strings.Repeat("é", 199)); err != nil {
		t.Errorf("Slug(199 bmp) err = %v, want nil", err)
	}
}

func TestSlugEmpty(t *testing.T) {
	if _, err := Slug(""); err == nil {
		t.Fatal("Slug(\"\") = nil error, want error")
	}
}
