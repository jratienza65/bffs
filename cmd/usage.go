package cmd

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/usage"
	"github.com/jratienza65/bffs/internal/usagelog"
)

var (
	usageHorizon   time.Duration
	usageClaudeDir string
)

var usageCmd = &cobra.Command{
	Use:   "usage [name]",
	Short: "Estimate per-account usage and which account has the most headroom",
	Long: `Estimates each account's recent Claude usage from local data: token burn in
the last 5 hours (Anthropic's session-limit window) and over a longer horizon,
last-used times, detected limit events, and the account's plan tier.

Everything here is heuristic. Claude's transcripts carry no account identity,
so sessions are attributed by, in order: the config tree they live in (full
isolation), the per-account session metadata claude itself records, and the
bffs launch log (` + "`launches.jsonl`" + `, written by the shim at every launch —
disable with $` + usagelog.EnvDisable + `). What cannot be attributed is reported as
"unattributed", never guessed. Anthropic publishes no official limit API for
subscriptions; "suggested" simply means lowest recent weighted burn among
accounts with no active limit event.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		rep, err := usage.Collect(dir, usage.Options{Horizon: usageHorizon, HomeClaudeDir: usageClaudeDir})
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(rep.Accounts) == 0 {
			fmt.Fprintln(out, "No accounts configured. Try `bffs add <name>`.")
			return nil
		}
		if len(args) == 1 {
			for _, a := range rep.Accounts {
				if a.Name == args[0] {
					return renderUsageDetail(out, a, rep)
				}
			}
			names := make([]string, 0, len(rep.Accounts))
			for _, a := range rep.Accounts {
				names = append(names, a.Name)
			}
			return fmt.Errorf("unknown account %q; known: %v", args[0], names)
		}
		return renderUsageTable(out, rep)
	},
}

func init() {
	usageCmd.Flags().DurationVar(&usageHorizon, "horizon", usage.DefaultHorizon, "lookback window for the long column and the transcript scan")
	usageCmd.Flags().StringVar(&usageClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
	_ = usageCmd.Flags().MarkHidden("claude-dir")
	rootCmd.AddCommand(usageCmd)
}

func renderUsageTable(w io.Writer, rep usage.Report) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ACCOUNT\tTIER\tLAST-USED\t5H\t%s\tSTATUS\n", windowHeader(rep.Horizon))
	for _, a := range rep.Accounts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			a.Name, a.Tier.Display(), humanizeAgo(a.LastUsed, rep.Now),
			windowCell(a.Short), windowCell(a.Long), statusCell(a, rep.Now))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	if rep.Suggested != "" {
		fmt.Fprintf(w, "suggested: %s\n", rep.Suggested)
	}
	if rep.Unattributed.Sessions > 0 {
		fmt.Fprintf(w, "unattributed (%s): %d sessions, %s tok\n",
			strings.ToLower(windowHeader(rep.Horizon)), rep.Unattributed.Sessions, compactTokens(rep.Unattributed.Total()))
	}
	fmt.Fprintln(w, "note: heuristic estimates — attribution is best-effort, and only shim-launched or metadata-attributable sessions are counted.")
	return nil
}

func renderUsageDetail(w io.Writer, a usage.AccountUsage, rep usage.Report) error {
	longLabel := strings.ToLower(windowHeader(rep.Horizon)) + " window:"
	fmt.Fprintf(w, "account:     %s\n", a.Name)
	fmt.Fprintf(w, "type:        %s\n", a.Type)
	fmt.Fprintf(w, "tier:        %s\n", a.Tier.Display())
	fmt.Fprintf(w, "last used:   %s\n", humanizeAgo(a.LastUsed, rep.Now))
	fmt.Fprintf(w, "5h window:   %s\n", windowDetail(a.Short))
	fmt.Fprintf(w, "%-12s %s\n", longLabel, windowDetail(a.Long))
	fmt.Fprintf(w, "status:      %s\n", statusCell(a, rep.Now))
	if len(a.Attribution) > 0 {
		keys := make([]string, 0, len(a.Attribution))
		for k := range a.Attribution {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, a.Attribution[k]))
		}
		fmt.Fprintf(w, "attribution: %s\n", strings.Join(parts, " "))
	}
	if len(a.ByModel) > 0 {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "MODEL\tINPUT\tOUTPUT\tCACHE-W\tCACHE-R\tMSGS")
		for _, m := range sortedKeys(a.ByModel) {
			t := a.ByModel[m]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", dashIfEmpty(m),
				compactTokens(t.Input), compactTokens(t.Output),
				compactTokens(t.CacheCreation), compactTokens(t.CacheRead), t.Messages)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(a.ByDay) > 0 {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "DAY\tTOKENS\tMSGS")
		for _, d := range sortedKeys(a.ByDay) {
			t := a.ByDay[d]
			fmt.Fprintf(tw, "%s\t%s\t%d\n", d, compactTokens(t.Total()), t.Messages)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(m map[string]usage.Tokens) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func windowCell(ws usage.WindowStats) string {
	if ws.Messages == 0 {
		return "-"
	}
	return fmt.Sprintf("%s tok/%d msg", compactTokens(ws.Total()), ws.Messages)
}

func windowDetail(ws usage.WindowStats) string {
	if ws.Messages == 0 {
		return "-"
	}
	return fmt.Sprintf("%s tok / %d msg / %d sessions", compactTokens(ws.Total()), ws.Messages, ws.Sessions)
}

func statusCell(a usage.AccountUsage, now time.Time) string {
	switch {
	case a.Limit != nil && !a.Limit.ResetAt.IsZero() && a.Limit.ResetAt.After(now):
		reset := a.Limit.ResetAt.Local()
		layout := "15:04"
		if a.Limit.ResetAt.Sub(now) >= 24*time.Hour {
			layout = "Jan 2 15:04"
		}
		return "limited until " + reset.Format(layout)
	case a.Limit != nil && a.Limit.ResetAt.IsZero() && now.Sub(a.Limit.TS) < usage.ShortWindow:
		return "limited (reset unknown)"
	case a.LastUsed.IsZero() && a.Short.Messages == 0 && a.Long.Messages == 0:
		return "unused"
	default:
		return "ok"
	}
}

// humanizeAgo renders a past instant relative to now: "never", "just now",
// "5m ago", "3h ago", "2d ago".
func humanizeAgo(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// compactTokens renders token counts for tables: 987, 12.3k, 1.2M, 3.4B.
func compactTokens(n int64) string {
	trim := func(s string) string { return strings.Replace(s, ".0", "", 1) }
	switch {
	case n < 1_000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return trim(fmt.Sprintf("%.1fk", float64(n)/1e3))
	case n < 1_000_000_000:
		return trim(fmt.Sprintf("%.1fM", float64(n)/1e6))
	default:
		return trim(fmt.Sprintf("%.1fB", float64(n)/1e9))
	}
}

// windowHeader renders a horizon as a column header: 168h → "7D", 36h → "36H".
func windowHeader(d time.Duration) string {
	h := int(d.Hours())
	if h >= 24 && h%24 == 0 {
		return fmt.Sprintf("%dD", h/24)
	}
	return fmt.Sprintf("%dH", h)
}

// lastUsedByAccount is the cheap last-used source for `bffs list`/`show`: the
// latest launch-log event per account. `bffs usage` refines this with
// attributed transcript activity; list/show must stay fast, so they never
// parse transcripts.
func lastUsedByAccount(cfgDir string) map[string]time.Time {
	m := map[string]time.Time{}
	events, err := usagelog.Read(cfgDir)
	if err != nil {
		return m
	}
	for _, e := range events {
		if e.Account != "" && e.TS.After(m[e.Account]) {
			m[e.Account] = e.TS
		}
	}
	return m
}
