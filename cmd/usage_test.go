package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/usage"
)

var usageNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func TestHumanizeAgo(t *testing.T) {
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero", time.Time{}, "never"},
		{"seconds", usageNow.Add(-30 * time.Second), "just now"},
		{"minutes", usageNow.Add(-5 * time.Minute), "5m ago"},
		{"hours", usageNow.Add(-3 * time.Hour), "3h ago"},
		{"days", usageNow.Add(-49 * time.Hour), "2d ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanizeAgo(tc.t, usageNow); got != tc.want {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}
}

func TestCompactTokens(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{987, "987"},
		{12_345, "12.3k"},
		{1_000, "1k"},
		{1_234_567, "1.2M"},
		{3_400_000_000, "3.4B"},
	}
	for _, tc := range cases {
		if got := compactTokens(tc.n); got != tc.want {
			t.Errorf("compactTokens(%d): want %q, got %q", tc.n, tc.want, got)
		}
	}
}

func TestWindowHeader(t *testing.T) {
	if got := windowHeader(168 * time.Hour); got != "7D" {
		t.Errorf("168h: %q", got)
	}
	if got := windowHeader(36 * time.Hour); got != "36H" {
		t.Errorf("36h: %q", got)
	}
}

func testReport() usage.Report {
	return usage.Report{
		Now:     usageNow,
		Horizon: usage.DefaultHorizon,
		Accounts: []usage.AccountUsage{
			{
				Name: "personal", Type: "api_key",
				LastUsed: usageNow.Add(-2 * time.Hour),
				Short:    usage.WindowStats{Tokens: usage.Tokens{Input: 100, Output: 2000, Messages: 12}, Sessions: 1},
				Long:     usage.WindowStats{Tokens: usage.Tokens{Input: 500, Output: 9500, Messages: 40}, Sessions: 3},
			},
			{
				Name: "work", Type: "oauth",
				Tier:        usage.Tier{User: "max_20x"},
				LastUsed:    usageNow.Add(-10 * time.Minute),
				Short:       usage.WindowStats{Tokens: usage.Tokens{Input: 1000, Output: 2_500_000, Messages: 300}, Sessions: 2},
				Long:        usage.WindowStats{Tokens: usage.Tokens{Input: 5000, Output: 9_000_000, Messages: 900}, Sessions: 9},
				ByModel:     map[string]usage.Tokens{"claude-opus-5": {Input: 5000, Output: 9_000_000, Messages: 900}},
				ByDay:       map[string]usage.Tokens{"2026-08-24": {Output: 9_000_000, Messages: 900}},
				Attribution: map[string]int{usage.SrcLastSession: 7, usage.SrcLaunchLog: 2},
				Limit:       &usage.LimitEvent{TS: usageNow.Add(-time.Hour), Kind: "session", ResetAt: usageNow.Add(2 * time.Hour)},
			},
			{Name: "spare", Type: "oauth"},
		},
		Unattributed: usage.WindowStats{Tokens: usage.Tokens{Output: 123_000, Messages: 17}, Sessions: 4},
		Suggested:    "spare",
	}
}

func TestRenderUsageTable(t *testing.T) {
	var sb strings.Builder
	if err := renderUsageTable(&sb, testReport()); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()

	for _, want := range []string{
		"ACCOUNT", "TIER", "LAST-USED", "5H", "7D", "STATUS",
		"max_20x", "limited until", "unused", "ok",
		"suggested: spare",
		"unattributed (7d): 4 sessions, 123k tok",
		"note: heuristic",
		"10m ago", "never",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

func TestRenderUsageDetail(t *testing.T) {
	rep := testReport()
	var sb strings.Builder
	if err := renderUsageDetail(&sb, rep.Accounts[1], rep); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()

	for _, want := range []string{
		"account:     work",
		"tier:        max_20x",
		"attribution: last-session=7 launch-log=2",
		"MODEL", "claude-opus-5",
		"DAY", "2026-08-24",
		"limited until",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q:\n%s", want, out)
		}
	}
}
