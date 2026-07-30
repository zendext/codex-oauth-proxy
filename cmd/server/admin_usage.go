package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type adminUsageCommand struct {
	Snapshot   adminUsageSnapshotCommand   `cmd:"" help:"Show rolling usage totals."`
	Timeseries adminUsageTimeseriesCommand `cmd:"" help:"Show bucketed usage data."`
}

type adminUsageSnapshotCommand struct {
	UserID   string `name:"user-id" help:"Filter by user ID."`
	APIKeyID string `name:"api-key-id" help:"Filter by API key ID."`
}

type adminUsageTimeseriesCommand struct {
	Window   string `help:"Usage window."`
	Step     string `help:"Aggregation step."`
	GroupBy  string `name:"group-by" help:"Comma-separated dimensions."`
	Fill     string `help:"Fill mode."`
	UserID   string `name:"user-id" help:"Filter by user ID."`
	APIKeyID string `name:"api-key-id" help:"Filter by API key ID."`
}

type usageSnapshotResponse struct {
	Usage []codexonly.ManagementUsageEntry `json:"usage"`
}

func (c *adminUsageSnapshotCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	query := url.Values{}
	addQueryValue(query, "user_id", c.UserID)
	addQueryValue(query, "api_key_id", c.APIKeyID)
	var payload usageSnapshotResponse
	if err := client.doJSON(runtime.ctx, http.MethodGet, "/usage", query, nil, &payload); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, payload)
	}
	return writeUsageSnapshotTable(runtime.stdout, payload.Usage)
}

func (c *adminUsageTimeseriesCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	query := url.Values{}
	addQueryValue(query, "window", c.Window)
	addQueryValue(query, "step", c.Step)
	addQueryValue(query, "fill", c.Fill)
	addQueryValue(query, "user_id", c.UserID)
	addQueryValue(query, "api_key_id", c.APIKeyID)
	for _, group := range strings.Split(c.GroupBy, ",") {
		group = strings.TrimSpace(group)
		if group != "" {
			query.Add("group_by", group)
		}
	}
	var payload codexonly.UsageTimeseries
	if err := client.doJSON(runtime.ctx, http.MethodGet, "/usage/timeseries", query, nil, &payload); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, payload)
	}
	return writeUsageTimeseriesTable(runtime.stdout, payload.Series)
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
