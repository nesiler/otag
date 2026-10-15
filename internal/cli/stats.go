package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"text/tabwriter"
)

// modelStat is a v_model_usage row.
type modelStat struct {
	Model            string   `json:"model"`
	Harness          string   `json:"harness"`
	Runs             int64    `json:"runs"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	Estimated        bool     `json:"estimated"`
}

// profileStat is a v_profile_stats row.
type profileStat struct {
	Profile        string   `json:"profile"`
	Runs           int64    `json:"runs"`
	InputTokens    int64    `json:"input_tokens"`
	OutputTokens   int64    `json:"output_tokens"`
	CostUSD        *float64 `json:"cost_usd"`
	AvgDurationS   *float64 `json:"avg_duration_s"`
	TotalDurationS *float64 `json:"total_duration_s"`
	Reviews        int64    `json:"reviews"`
	Rejected       int64    `json:"rejected"`
	RejectRate     *float64 `json:"reject_rate"`
}

// deliveryStat is v_task_delivery per project: done tasks waiting for delivery and
// the done -> delivered wait of the delivered ones.
type deliveryStat struct {
	Project   string   `json:"project"`
	Delivered int64    `json:"delivered"`
	Waiting   int64    `json:"waiting"` // done, not delivered yet
	AvgWaitS  *float64 `json:"avg_wait_s"`
	MaxWaitS  *float64 `json:"max_wait_s"`
}

type statsResult struct {
	Models   []modelStat    `json:"models"`
	Profiles []profileStat  `json:"profiles"`
	Delivery []deliveryStat `json:"delivery"`
}

// cmdStats shows usage per reported model; per profile, runs, tokens, durations and
// the review reject rate; per project, the done -> delivered wait (all projects):
// otag stats.
func cmdStats(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("stats")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) > 0 {
		return result{}, usageErr("usage: otag stats")
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	res := statsResult{Models: []modelStat{}, Profiles: []profileStat{}, Delivery: []deliveryStat{}}
	rows, err := d.QueryContext(ctx, `SELECT model, harness, runs, input_tokens, output_tokens, cache_read_tokens,
		cache_write_tokens, reasoning_tokens, cost_usd, estimated FROM v_model_usage ORDER BY input_tokens + output_tokens DESC`)
	if err != nil {
		return result{}, err
	}
	for rows.Next() {
		var s modelStat
		var cost sql.NullFloat64
		if err := rows.Scan(&s.Model, &s.Harness, &s.Runs, &s.InputTokens, &s.OutputTokens, &s.CacheReadTokens,
			&s.CacheWriteTokens, &s.ReasoningTokens, &cost, &s.Estimated); err != nil {
			_ = rows.Close()
			return result{}, err
		}
		s.CostUSD = nullFloat(cost)
		res.Models = append(res.Models, s)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return result{}, err
	}
	rows, err = d.QueryContext(ctx, `SELECT profile, runs, input_tokens, output_tokens, cost_usd, avg_duration_s,
		total_duration_s, reviews, rejected, reject_rate FROM v_profile_stats WHERE runs > 0 OR reviews > 0 ORDER BY profile`)
	if err != nil {
		return result{}, err
	}
	for rows.Next() {
		var s profileStat
		var cost, avg, total, rate sql.NullFloat64
		if err := rows.Scan(&s.Profile, &s.Runs, &s.InputTokens, &s.OutputTokens, &cost, &avg, &total, &s.Reviews,
			&s.Rejected, &rate); err != nil {
			return result{}, err
		}
		s.CostUSD, s.AvgDurationS, s.TotalDurationS, s.RejectRate = nullFloat(cost), nullFloat(avg), nullFloat(total), nullFloat(rate)
		res.Profiles = append(res.Profiles, s)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return result{}, err
	}
	rows, err = d.QueryContext(ctx, `SELECT project, sum(status = 'delivered'), sum(status = 'done'), avg(wait_s), max(wait_s)
		FROM v_task_delivery GROUP BY project ORDER BY project`)
	if err != nil {
		return result{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s deliveryStat
		var avg, most sql.NullFloat64
		if err := rows.Scan(&s.Project, &s.Delivered, &s.Waiting, &avg, &most); err != nil {
			return result{}, err
		}
		s.AvgWaitS, s.MaxWaitS = nullFloat(avg), nullFloat(most)
		res.Delivery = append(res.Delivery, s)
	}
	if err := rows.Err(); err != nil {
		return result{}, err
	}
	return result{data: res, human: func(w io.Writer) { printStats(w, res) }}, nil
}

func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func printStats(w io.Writer, res statsResult) {
	opt := func(v *float64, format string) string {
		if v == nil {
			return "-"
		}
		return fmt.Sprintf(format, *v)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tHARNESS\tRUNS\tIN\tOUT\tCACHE R/W\tCOST")
	for _, s := range res.Models {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d/%d\t%s\n", s.Model, s.Harness, s.Runs, s.InputTokens, s.OutputTokens,
			s.CacheReadTokens, s.CacheWriteTokens, opt(s.CostUSD, "$%.2f"))
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROFILE\tRUNS\tIN\tOUT\tAVG MIN\tTOTAL H\tREVIEWS\tREJECTED")
	for _, s := range res.Profiles {
		var avg, total *float64
		if s.AvgDurationS != nil {
			v := *s.AvgDurationS / 60
			avg = &v
		}
		if s.TotalDurationS != nil {
			v := *s.TotalDurationS / 3600
			total = &v
		}
		rate := "-"
		if s.RejectRate != nil {
			rate = fmt.Sprintf("%d (%.0f%%)", s.Rejected, *s.RejectRate*100)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\t%d\t%s\n", s.Profile, s.Runs, s.InputTokens, s.OutputTokens,
			opt(avg, "%.1f"), opt(total, "%.1f"), s.Reviews, rate)
	}
	_ = tw.Flush()
	if len(res.Delivery) == 0 {
		return
	}
	hours := func(v *float64) *float64 {
		if v == nil {
			return nil
		}
		h := *v / 3600
		return &h
	}
	_, _ = fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT\tDELIVERED\tWAITING\tAVG WAIT H\tMAX WAIT H")
	for _, s := range res.Delivery {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\n", s.Project, s.Delivered, s.Waiting, opt(hours(s.AvgWaitS), "%.1f"),
			opt(hours(s.MaxWaitS), "%.1f"))
	}
	_ = tw.Flush()
}
