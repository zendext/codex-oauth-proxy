package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

const defaultGrokAdminBaseURL = "http://127.0.0.1:8318"

type adminGrokCommand struct {
	URL     string                  `name:"grok-url" default:"http://127.0.0.1:8318" help:"Base URL of the local Grok listener."`
	Status  adminGrokStatusCommand  `cmd:"" default:"withargs" help:"List Grok logins."`
	Refresh adminGrokRefreshCommand `cmd:"" help:"Refresh one Grok login."`
	Usage   adminGrokUsageCommand   `cmd:"" help:"Query live Grok subscription usage."`
}

type adminGrokStatusCommand struct{}

type adminGrokRefreshCommand struct {
	ID string `arg:"" name:"id" help:"Stable Grok auth ID."`
}

type adminGrokUsageCommand struct {
	ID string `arg:"" name:"id" help:"Stable Grok auth ID."`
}

type grokAuthStatusResponse struct {
	Auths []codexonly.GrokAuthStatus `json:"auths"`
}

func (c *adminGrokStatusCommand) Run(runtime *commandRuntime, parent *adminGrokCommand, admin *adminCommand) error {
	client, err := newAdminClient(firstNonEmpty(parent.URL, defaultGrokAdminBaseURL))
	if err != nil {
		return err
	}
	var payload grokAuthStatusResponse
	if err = client.doJSON(runtime.ctx, http.MethodGet, "/grok/auths", nil, nil, &payload); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, payload)
	}
	return writeGrokAuthTable(runtime.stdout, payload.Auths)
}

func (c *adminGrokRefreshCommand) Run(runtime *commandRuntime, parent *adminGrokCommand, admin *adminCommand) error {
	client, err := newAdminClient(firstNonEmpty(parent.URL, defaultGrokAdminBaseURL))
	if err != nil {
		return err
	}
	var payload map[string]any
	if err = client.doJSON(runtime.ctx, http.MethodPost, "/grok/auths/refresh", nil, map[string]string{"id": c.ID}, &payload); err != nil {
		return err
	}
	return writeJSONOutput(runtime.stdout, payload)
}

func (c *adminGrokUsageCommand) Run(runtime *commandRuntime, parent *adminGrokCommand, admin *adminCommand) error {
	client, err := newAdminClient(firstNonEmpty(parent.URL, defaultGrokAdminBaseURL))
	if err != nil {
		return err
	}
	query := url.Values{}
	query.Set("id", c.ID)
	var payload map[string]any
	if err = client.doJSON(runtime.ctx, http.MethodGet, "/grok/usage", query, nil, &payload); err != nil {
		return err
	}
	return writeJSONOutput(runtime.stdout, payload)
}

func writeGrokAuthTable(stdout io.Writer, auths []codexonly.GrokAuthStatus) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tACCOUNT\tEMAIL\tSOURCES\tREFRESHABLE\tEXPIRED")
	for _, auth := range auths {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%t\t%t\n", auth.ID, auth.AccountID, auth.Email, auth.SourceCount, auth.Refreshable, auth.Expired)
	}
	return tw.Flush()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
