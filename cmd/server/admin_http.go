package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultAdminBaseURL = "http://127.0.0.1:8317"

type adminOptions struct {
	json bool
}

type adminCommand struct {
	URL   string            `name:"url" default:"http://127.0.0.1:8317" help:"Base URL of the running proxy server."`
	JSON  bool              `name:"json" help:"Write machine-readable JSON output."`
	Users adminUsersCommand `cmd:"" help:"Manage users and API keys."`
	Usage adminUsageCommand `cmd:"" help:"Inspect usage data."`
}

type adminClient struct {
	baseURL    *url.URL
	httpClient *http.Client
}

func (c *adminCommand) ProvideAdminClient() (*adminClient, error) {
	return newAdminClient(c.URL)
}

func (c *adminCommand) options() adminOptions {
	return adminOptions{json: c.JSON}
}

func newAdminClient(rawURL string) (*adminClient, error) {
	rawURL = strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if rawURL == "" {
		rawURL = defaultAdminBaseURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse admin url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("admin url must include scheme and host")
	}
	return &adminClient{
		baseURL: parsed,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}, nil
}

func (c *adminClient) doJSON(ctx context.Context, method string, path string, query url.Values, body any, target any) error {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v0/local-admin" + path
	endpoint.RawQuery = query.Encode()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", c.baseURL.String(), err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("admin request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if target == nil {
		return nil
	}
	if err = json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func writeJSONOutput(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
