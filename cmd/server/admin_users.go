package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type adminUsersCommand struct {
	List     adminUsersListCommand     `cmd:"" help:"List users."`
	Create   adminUsersCreateCommand   `cmd:"" help:"Create a user and API key."`
	Get      adminUsersGetCommand      `cmd:"" help:"Get a user."`
	Update   adminUsersUpdateCommand   `cmd:"" help:"Rename a user."`
	Enable   adminUsersEnableCommand   `cmd:"" help:"Enable a user."`
	Disable  adminUsersDisableCommand  `cmd:"" help:"Disable a user."`
	ResetKey adminUsersResetKeyCommand `cmd:"" name:"reset-key" help:"Replace a user's API key."`
}

type adminUsersListCommand struct {
	Enabled string `help:"Filter by enabled state."`
}

type adminUsersCreateCommand struct {
	Name string `arg:"" name:"name" help:"User name."`
}

type adminUsersGetCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
}

type adminUsersUpdateCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
	Name   string `help:"New user name."`
}

type adminUsersEnableCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
}

type adminUsersDisableCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
}

type adminUsersResetKeyCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
}

type usersListResponse struct {
	Users []codexonly.UserWithAPIKey `json:"users"`
}

type userCreateRequest struct {
	Name string `json:"name"`
}

type userUpdateRequest struct {
	Name    *string `json:"name,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

func (c *adminUsersListCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	return runAdminUsersList(runtime.ctx, client, admin.options(), c.Enabled, runtime.stdout)
}

func (c *adminUsersCreateCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	return runAdminUsersCreate(runtime.ctx, client, admin.options(), c.Name, runtime.stdout)
}

func (c *adminUsersGetCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	return runAdminUsersGet(runtime.ctx, client, admin.options(), c.UserID, runtime.stdout)
}

func (c *adminUsersUpdateCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	return runAdminUsersUpdate(runtime.ctx, client, admin.options(), c.UserID, c.Name, runtime.stdout)
}

func (c *adminUsersEnableCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	return runAdminUsersSetEnabled(runtime.ctx, client, admin.options(), c.UserID, runtime.stdout, true)
}

func (c *adminUsersDisableCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	return runAdminUsersSetEnabled(runtime.ctx, client, admin.options(), c.UserID, runtime.stdout, false)
}

func (c *adminUsersResetKeyCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	return runAdminUsersResetKey(runtime.ctx, client, admin.options(), c.UserID, runtime.stdout)
}

func runAdminUsersList(ctx context.Context, client *adminClient, opts adminOptions, enabled string, stdout io.Writer) error {
	query := url.Values{}
	if strings.TrimSpace(enabled) != "" {
		query.Set("enabled", strings.TrimSpace(enabled))
	}
	var payload usersListResponse
	if err := client.doJSON(ctx, http.MethodGet, "/users", query, nil, &payload); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, payload)
	}
	return writeUsersTable(stdout, payload.Users)
}

func runAdminUsersCreate(ctx context.Context, client *adminClient, opts adminOptions, name string, stdout io.Writer) error {
	var created codexonly.CreatedUserAPIKey
	if err := client.doJSON(ctx, http.MethodPost, "/users", nil, userCreateRequest{Name: name}, &created); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, created)
	}
	fmt.Fprintf(stdout, "USER_ID\tNAME\tENABLED\tAPI_KEY_ID\tMASKED_KEY\tAPI_KEY_VALUE\n")
	fmt.Fprintf(stdout, "%s\t%s\t%t\t%s\t%s\t%s\n", created.User.ID, created.User.Name, created.User.Enabled, created.APIKey.ID, created.APIKey.MaskedKey, created.PlaintextAPIKey)
	return nil
}

func runAdminUsersGet(ctx context.Context, client *adminClient, opts adminOptions, userID string, stdout io.Writer) error {
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(ctx, http.MethodGet, "/users/"+url.PathEscape(userID), nil, nil, &user); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, user)
	}
	return writeUsersTable(stdout, []codexonly.UserWithAPIKey{user})
}

func runAdminUsersUpdate(ctx context.Context, client *adminClient, opts adminOptions, userID string, name string, stdout io.Writer) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("--name is required")
	}
	req := userUpdateRequest{Name: &name}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(ctx, http.MethodPatch, "/users/"+url.PathEscape(userID), nil, req, &user); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, user)
	}
	return writeUsersTable(stdout, []codexonly.UserWithAPIKey{user})
}

func runAdminUsersSetEnabled(ctx context.Context, client *adminClient, opts adminOptions, userID string, stdout io.Writer, enabled bool) error {
	req := userUpdateRequest{Enabled: &enabled}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(ctx, http.MethodPatch, "/users/"+url.PathEscape(userID), nil, req, &user); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, user)
	}
	return writeUsersTable(stdout, []codexonly.UserWithAPIKey{user})
}

func runAdminUsersResetKey(ctx context.Context, client *adminClient, opts adminOptions, userID string, stdout io.Writer) error {
	var created codexonly.CreatedUserAPIKey
	if err := client.doJSON(ctx, http.MethodPost, "/users/"+url.PathEscape(userID)+"/api-key/reset", nil, nil, &created); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, created)
	}
	fmt.Fprintf(stdout, "USER_ID\tNAME\tENABLED\tAPI_KEY_ID\tMASKED_KEY\tAPI_KEY_VALUE\n")
	fmt.Fprintf(stdout, "%s\t%s\t%t\t%s\t%s\t%s\n", created.User.ID, created.User.Name, created.User.Enabled, created.APIKey.ID, created.APIKey.MaskedKey, created.PlaintextAPIKey)
	return nil
}

func writeUsersTable(stdout io.Writer, users []codexonly.UserWithAPIKey) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tUSER_ID\tENABLED\tAPI_KEY\tMASKED_KEY\tKEY_ENABLED")
	for _, item := range users {
		keyID := ""
		masked := ""
		keyEnabled := ""
		if item.APIKey != nil {
			keyID = item.APIKey.ID
			masked = item.APIKey.MaskedKey
			keyEnabled = fmt.Sprintf("%t", item.APIKey.Enabled)
		}
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\t%s\n", item.User.Name, item.User.ID, item.User.Enabled, keyID, masked, keyEnabled)
	}
	return tw.Flush()
}
