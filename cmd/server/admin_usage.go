package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type usageSnapshotResponse struct {
	Usage []codexonly.ManagementUsageEntry `json:"usage"`
}

func runAdminUsage(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("admin usage requires a command")
	}
	switch args[0] {
	case "snapshot":
		return runAdminUsageSnapshot(ctx, client, opts, args[1:], stdout)
	case "timeseries":
		return runAdminUsageTimeseries(ctx, client, opts, args[1:], stdout)
	default:
		return fmt.Errorf("unknown admin usage command %q", args[0])
	}
}

func runAdminUsageSnapshot(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin usage snapshot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	userID := fs.String("user-id", "", "Filter by user ID")
	apiKeyID := fs.String("api-key-id", "", "Filter by API key ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := url.Values{}
	if strings.TrimSpace(*userID) != "" {
		query.Set("user_id", strings.TrimSpace(*userID))
	}
	if strings.TrimSpace(*apiKeyID) != "" {
		query.Set("api_key_id", strings.TrimSpace(*apiKeyID))
	}
	var payload usageSnapshotResponse
	if err := client.doJSON(ctx, http.MethodGet, "/usage", query, nil, &payload); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, payload)
	}
	return writeUsageSnapshotTable(stdout, payload.Usage)
}

func runAdminUsageTimeseries(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin usage timeseries", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	window := fs.String("window", "", "Usage window")
	step := fs.String("step", "", "Aggregation step")
	groupBy := fs.String("group-by", "", "Comma-separated dimensions")
	fill := fs.String("fill", "", "Fill mode")
	userID := fs.String("user-id", "", "Filter by user ID")
	apiKeyID := fs.String("api-key-id", "", "Filter by API key ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := url.Values{}
	addQueryValue(query, "window", *window)
	addQueryValue(query, "step", *step)
	addQueryValue(query, "fill", *fill)
	addQueryValue(query, "user_id", *userID)
	addQueryValue(query, "api_key_id", *apiKeyID)
	for _, group := range strings.Split(*groupBy, ",") {
		group = strings.TrimSpace(group)
		if group != "" {
			query.Add("group_by", group)
		}
	}
	var payload codexonly.UsageTimeseries
	if err := client.doJSON(ctx, http.MethodGet, "/usage/timeseries", query, nil, &payload); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, payload)
	}
	return writeUsageTimeseriesTable(stdout, payload.Series)
}

func addQueryValue(query url.Values, key string, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		query.Set(key, value)
	}
}

func writeUsageSnapshotTable(stdout io.Writer, usage []codexonly.ManagementUsageEntry) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tUSER_ID\tAPI_KEY\tREQ_5H\tTOKENS_5H\tREQ_7D\tTOKENS_7D")
	for _, entry := range usage {
		fiveHour := entry.Windows["5h"]
		sevenDay := entry.Windows["7d"]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\n",
			entry.Name,
			entry.UserID,
			entry.APIKeyID,
			fiveHour.RequestCount,
			fiveHour.TotalTokens,
			sevenDay.RequestCount,
			sevenDay.TotalTokens,
		)
	}
	return tw.Flush()
}

func writeUsageTimeseriesTable(stdout io.Writer, series []codexonly.UsageTimeseriesPoint) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BUCKET_START\tUSER\tUSER_ID\tAPI_KEY\tMODEL\tREASONING\tTIER\tREQUESTS\tTOKENS")
	for _, point := range series {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n",
			point.BucketStart.Format(time.RFC3339),
			point.Name,
			point.UserID,
			point.APIKeyID,
			point.Model,
			point.ReasoningEffort,
			point.ServiceTier,
			point.RequestCount,
			point.TotalTokens,
		)
	}
	return tw.Flush()
}
