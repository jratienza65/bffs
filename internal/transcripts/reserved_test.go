package transcripts

import "testing"

func TestIsReserved(t *testing.T) {
	sid := "0f3b2c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e"
	cases := []struct {
		name string
		want bool
	}{
		{"memory", true},
		{"Memory", true},
		{"MEMORY", true},
		{"tiny_memory", true},
		{"bagel", true},
		{"cloud-snapshots", true},
		{"bridge-pointer.json", true},
		{".session-aliases", true},
		{"memory.bffs-replaced-1", true},
		{"memory.bffs-replaced-1756987654321", true},
		{sid + ".jsonl.bffs-replaced-1756987654321", true},
		{sid + ".bffs-replaced-1756987654321", true},
		{sid + ".jsonl.bffs-tmp", true},
		{sid + ".jsonl.bffs-tmp-4f2a9c", true},
		{sid + ".bffs-tmp", true},
		{".bffs-tmp", true},
		{"my_proj", false},
		{"-Users-jonas-build-projects-bffs", false},
		{sid, false},
		{sid + ".jsonl", false},
		{"memory.bffs-replaced-", false},
		{"memory.bffs-replaced-abc", false},
		{"memory.bffs-replaced-1x", false},
		{"bffs-tmp", false},
		{"x.bffs-tmp-", false},
		{"memories", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsReserved(c.name); got != c.want {
			t.Errorf("IsReserved(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsSetAsideDoesNotCoverReservedEntries(t *testing.T) {
	for name := range ReservedProjectEntries {
		if IsSetAside(name) {
			t.Errorf("IsSetAside(%q) = true; reserved entries and set-asides must be disjoint", name)
		}
		if name != toLower(name) {
			t.Errorf("ReservedProjectEntries key %q is not lower-case", name)
		}
	}
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
