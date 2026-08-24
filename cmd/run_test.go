package cmd

import (
	"strings"
	"testing"
)

func TestParseRunArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		dashAt     int
		account    string
		claudeArgs []string
		wantErr    string
	}{
		{name: "interactive, no dash", args: []string{"work"}, dashAt: -1, account: "work"},
		{name: "headless after dash", args: []string{"work", "-p", "hi"}, dashAt: 1, account: "work", claudeArgs: []string{"-p", "hi"}},
		{name: "dash first", args: []string{"-p", "hi"}, dashAt: 0, wantErr: "account name must come before"},
		{name: "extra arg without dash", args: []string{"work", "stray"}, dashAt: -1, wantErr: "after `--`"},
		{name: "extra arg before dash", args: []string{"work", "stray", "-p"}, dashAt: 2, wantErr: "after `--`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account, claudeArgs, err := parseRunArgs(tc.args, tc.dashAt)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRunArgs: %v", err)
			}
			if account != tc.account {
				t.Errorf("account: want %q, got %q", tc.account, account)
			}
			if len(claudeArgs) != len(tc.claudeArgs) {
				t.Fatalf("claudeArgs: want %v, got %v", tc.claudeArgs, claudeArgs)
			}
			for i := range claudeArgs {
				if claudeArgs[i] != tc.claudeArgs[i] {
					t.Fatalf("claudeArgs: want %v, got %v", tc.claudeArgs, claudeArgs)
				}
			}
		})
	}
}
