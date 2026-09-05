package cmd

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "simple", in: "work"},
		{name: "digits dash underscore", in: "team-2_b"},
		{name: "mixed case", in: "Personal"},
		{name: "empty", in: "", wantErr: "must not be empty"},
		{name: "space", in: "my acct", wantErr: "invalid character"},
		{name: "path separator", in: "a/b", wantErr: "invalid character"},
		{name: "dot", in: "a.b", wantErr: "invalid character"},
		{name: "reserved home", in: "home", wantErr: `account name "home" is reserved for ~/.claude.json`},
		{name: "reserved is exact", in: "homer"},
		{name: "reserved is case-sensitive", in: "Home"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateName(tc.in)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateName(%q): unexpected error %v", tc.in, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateName(%q): want error containing %q, got %v", tc.in, tc.wantErr, err)
			}
		})
	}
}

// The reservation message must tell the user what `home` stands for and
// what to do, since `bffs add home` is a natural first guess.
func TestValidateNameReservedHomeMessage(t *testing.T) {
	err := validateName(reservedAccountName)
	if err == nil {
		t.Fatal("home must be rejected")
	}
	for _, want := range []string{"reserved for ~/.claude.json", "unmanaged/api_key home config", "choose another name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
}

// login derives a default name from --email; that path must hit the same
// gate, so "home@…" cannot sneak the reserved name in.
func TestDefaultNameFromEmailStillGated(t *testing.T) {
	name := defaultNameFromEmail("home@example.com")
	if name != "home" {
		t.Fatalf("defaultNameFromEmail derived %q, want the local part", name)
	}
	if err := validateName(name); err == nil {
		t.Error("derived name home must be rejected by validateName")
	}
}
