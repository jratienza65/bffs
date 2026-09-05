package rehome

import "testing"

func TestVerifyCommand(t *testing.T) {
	cases := []struct {
		cwd, sid, account string
		want              string
	}{
		{"/home/jonas/src/bffs", testSID, "", "cd /home/jonas/src/bffs && claude --resume " + testSID},
		{"/home/jonas/src/bffs", testSID, "work", "BFFS_ACCOUNT=work cd /home/jonas/src/bffs && claude --resume " + testSID},
		{"/Users/jonas/My Projects/bffs", testSID, "", "cd '/Users/jonas/My Projects/bffs' && claude --resume " + testSID},
		{"/tmp/it's here", testSID, "", `cd '/tmp/it'\''s here' && claude --resume ` + testSID},
		{"/tmp/a$b", testSID, "", "cd '/tmp/a$b' && claude --resume " + testSID},
		{"C:/Users/jonas/src", testSID, "", "cd C:/Users/jonas/src && claude --resume " + testSID},
		{"", testSID, "work", "BFFS_ACCOUNT=work claude --resume " + testSID},
		{"/x", testSID, "odd name", "BFFS_ACCOUNT='odd name' cd /x && claude --resume " + testSID},
		{"/x\x1b]52;c;evil\x07/y", testSID, "", "cd /x/y && claude --resume " + testSID},
	}
	for _, tc := range cases {
		if got := VerifyCommand(tc.cwd, tc.sid, tc.account); got != tc.want {
			t.Errorf("VerifyCommand(%q, %q, %q) = %q, want %q", tc.cwd, tc.sid, tc.account, got, tc.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":              "''",
		"plain":         "plain",
		"a-b_c.d/e:f@g": "a-b_c.d/e:f@g",
		"has space":     "'has space'",
		"tilde~":        "'tilde~'",
		"q'q":           `'q'\''q'`,
		"n\nl":          "'n\nl'",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
