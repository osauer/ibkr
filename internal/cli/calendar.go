package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func runCalendar(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "calendar")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	market := fs.String("market", "us", "market calendar: us, us-options, or de")
	date := fs.String("date", "", "local market date YYYY-MM-DD (default: today)")
	next := fs.Int("next", 14, "number of calendar days to include (1-400)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "calendar: takes no positional args (got %v)", fs.Args())
	}
	if *next <= 0 {
		return fail(env, "calendar: --next must be positive")
	}
	params := rpc.MarketCalendarParams{
		Market: strings.TrimSpace(*market),
		Date:   strings.TrimSpace(*date),
		Days:   *next,
	}
	var res rpc.MarketCalendarResult
	if err := env.Conn.Call(ctx, rpc.MethodMarketCalendar, params, &res); err != nil {
		return fail(env, "calendar: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderCalendarText(env, &res)
}

func renderCalendarText(env *Env, r *rpc.MarketCalendarResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s calendar  ·  %s\n", r.Label, r.Timezone)
	fmt.Fprintln(out)
	renderCalendarSessionLine(env, r.Session)
	if r.Session.SourceURL != "" {
		fmt.Fprintf(out, "  Source:        %s\n", r.Session.SourceURL)
	}
	fmt.Fprintf(out, "  Coverage:      %s to %s\n", r.CoverageStart, r.CoverageEnd)

	if len(r.Sessions) > 1 {
		fmt.Fprintln(out)
		header := fmt.Sprintf("  %-12s  %-11s  %-20s  %s", "DATE", "STATE", "HOURS", "REASON")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
		for _, s := range r.Sessions {
			fmt.Fprintf(out, "  %-12s  %-11s  %-20s  %s\n",
				s.Date, s.State, marketSessionHours(s), nonEmpty(s.Reason, ""))
		}
	}
	fmt.Fprintln(out)
	return 0
}

func renderCalendarSessionLine(env *Env, s rpc.MarketSession) {
	out := env.Stdout
	state := s.State
	if s.IsOpen {
		state = env.green(state)
	} else if s.State == "holiday" || s.State == "early_close" || s.State == "unknown" {
		state = env.yellow(state)
	}
	fmt.Fprintf(out, "  Session:       %s  %s\n", s.Date, state)
	if hours := marketSessionHours(s); hours != "" {
		fmt.Fprintf(out, "  Hours:         %s\n", hours)
	}
	if s.Reason != "" {
		fmt.Fprintf(out, "  Reason:        %s\n", s.Reason)
	}
	if s.NextOpen != nil {
		next := marketSessionTime(s, *s.NextOpen).Format("2006-01-02 15:04 MST")
		fmt.Fprintf(out, "  Next open:     %s\n", next)
	}
	if s.Notes != "" {
		fmt.Fprintf(out, "  Notes:         %s\n", s.Notes)
	}
}

func marketSessionHours(s rpc.MarketSession) string {
	if s.Open.IsZero() || s.Close.IsZero() {
		return ""
	}
	open := marketSessionTime(s, s.Open)
	closeT := marketSessionTime(s, s.Close)
	return open.Format("15:04") + "-" + closeT.Format("15:04 MST")
}

func marketSessionTime(s rpc.MarketSession, t time.Time) time.Time {
	if s.Timezone == "" {
		return t
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return t
	}
	return t.In(loc)
}

func quoteSessionHint(env *Env, s *rpc.MarketSession) string {
	if s == nil {
		return ""
	}
	parts := []string{s.Label}
	if s.State != "" {
		parts = append(parts, s.State)
	}
	if s.Reason != "" {
		parts = append(parts, s.Reason)
	}
	if s.NextOpen != nil {
		parts = append(parts, "next open "+marketSessionTime(*s, *s.NextOpen).Format("2006-01-02 15:04 MST"))
	}
	return env.yellow(strings.Join(parts, " · "))
}
